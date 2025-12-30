package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"html/template"

	"github.com/joho/godotenv"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"

	"github.com/goldenpineappleofthesun/siziph"
	"github.com/goldenpineappleofthesun/siclo"
)

const (
	StateJoining          = "joining"
	StateStartAck         = "wait-start-ack"
	StateSelectQuestion   = "select-question"
	//StateQuestionAck      = "wait-question-ack"
	StateQuestion         = "question"
	StateWaitAnswer       = "wait-answer"
	StateShowAnswer       = "show-answer"
	StateUnknown          = "unknown"
)

type GameState struct {
	mu                sync.RWMutex
	state             string
	packageJson       map[string]interface{}
	players           map[int]*Player
	nextNPCId         int
	currentPlayerId   int
	roundNum          int
	gameStartTime     time.Time
	answeredQuestions map[string]bool
	clients           map[*websocket.Conn]bool
	broadcast         chan []byte
	upgrader          websocket.Upgrader
}

type Player struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Score       int    `json:"score"`
	IsNPC       bool   `json:"-"`
	NPCCharacter *NPCCharacter `json:"-"` // Ссылка на персонажа NPC
}

type NPCCharacter struct {
	Name         string `json:"name"`
	Photo        string `json:"photo"`
	HostPrompt   string `json:"host_prompt"`
	PlayerPrompt string `json:"player_prompt"`
}

type Question struct {
	ID      int    `json:"id"`
	Price   int    `json:"price"`
	Theme   string `json:"theme"`
	Text    string `json:"text,omitempty"`
	Answer  string `json:"answer,omitempty"`
	Comment string `json:"comment,omitempty"`
}

type Theme struct {
	Name      string `json:"name"`
	Questions []Question `json:"questions"`
}

var gameState *GameState
var npcCharacters []NPCCharacter
var npcCharactersMap map[string]*NPCCharacter // Для быстрого поиска по имени
/*var acknowledgeWaitStarted bool*/

func init() {
	gameState = &GameState{
		state:             StateJoining,
		players:           make(map[int]*Player),
		nextNPCId:         100,
		answeredQuestions: make(map[string]bool),
		clients:           make(map[*websocket.Conn]bool),
		broadcast:         make(chan []byte, 256),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
	npcCharactersMap = make(map[string]*NPCCharacter)
}

func main() {
	_ = godotenv.Load("../.env")
	
	// Очищаем папку players при старте сервера
	os.RemoveAll("package")
	os.RemoveAll("players")
	
	// Создаем необходимые папки
	os.MkdirAll("package", 0755)
	os.MkdirAll("players", 0755)
	os.MkdirAll("npc_characters", 0755)

	// Загружаем NPC персонажей при старте
	if err := loadNPCCharacters(); err != nil {
		log.Printf("Warning: Failed to load NPC characters: %v", err)
	}

	// Запускаем broadcaster для WebSocket
	go gameState.broadcaster()

	http.Handle("/index.html",       render("index"))
	http.Handle("/join.html",        render("join"))
	http.Handle("/joinhost.html",    render("joinhost"))
	http.Handle("/joinnpc.html",     render("joinnpc"))
	http.Handle("/client/",          http.StripPrefix("/client/",http.FileServer(http.Dir("/app/client")),),)
	http.Handle("/upload",           withCORS(http.HandlerFunc(handleUpload)))
	http.Handle("/join",             withCORS(http.HandlerFunc(handleJoin)))
	http.Handle("/joinnpc",          withCORS(http.HandlerFunc(handleJoinNPC)))
	http.Handle("/joinshowman",      withCORS(http.HandlerFunc(handleJoinShowman)))
	http.Handle("/npccharacters",    withCORS(http.HandlerFunc(handleNPCCharacters)))
	http.Handle("/state",            withCORS(http.HandlerFunc(handleState)))
	http.Handle("/scores",           withCORS(http.HandlerFunc(handleScores)))
	http.Handle("/data",             withCORS(http.HandlerFunc(handleData)))
	http.Handle("/media",            withCORS(http.HandlerFunc(handleMedia)))
	http.Handle("/currentplayer",    withCORS(http.HandlerFunc(handleCurrentPlayer)))
	http.Handle("/currentround",     withCORS(http.HandlerFunc(handleCurrentRound)))
	http.Handle("/playerstate",      withCORS(http.HandlerFunc(handlePlayerState)))
	http.Handle("/start",            withCORS(http.HandlerFunc(handleStart)))
	http.Handle("/startacknowledge", withCORS(http.HandlerFunc(handleStartAcknowledge)))
	http.Handle("/selectquestion",   withCORS(http.HandlerFunc(handleSelectQuestion)))
	//http.Handle("/acknowledge",      withCORS(http.HandlerFunc(handleAcknowledge)))
	http.Handle("/questionbeenshown",withCORS(http.HandlerFunc(handleQuestionBeenShown)))
	http.Handle("/requestanswer",    withCORS(http.HandlerFunc(handleRequestAnswer)))
	http.Handle("/answer",           withCORS(http.HandlerFunc(handleAnswer)))
	http.Handle("/timerdone",        withCORS(http.HandlerFunc(handleTimerDone)))
	http.Handle("/reset",            withCORS(http.HandlerFunc(handleReset)))
	http.Handle("/ws",               withCORS(http.HandlerFunc(handleWebSocket)))

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func render(page string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := template.ParseFiles("/app/client/" + page + ".html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		data := map[string]string{
			"host": os.Getenv("HOST_ADDRESS"),
		}

		if data["host"] == "" {
			data["host"] = "http://localhost:3000"
		}

		if err := tmpl.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func (gs *GameState) broadcaster() {
	for {
		select {
		case message := <-gs.broadcast:
			gs.mu.RLock()
			for client := range gs.clients {
				err := client.WriteMessage(websocket.TextMessage, message)
				if err != nil {
					log.Printf("WebSocket error: %v", err)
					client.Close()
					delete(gs.clients, client)
				}
			}
			gs.mu.RUnlock()
		}
	}
}

func (gs *GameState) broadcastMessage(msgType string, data map[string]interface{}) {
	message := map[string]interface{}{
		"type": msgType,
	}
	for k, v := range data {
		message[k] = v
	}
	jsonMsg, _ := json.Marshal(message)
	gs.broadcast <- jsonMsg
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := gameState.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	gameState.mu.Lock()
	gameState.clients[conn] = true
	gameState.mu.Unlock()

	// Keep connection alive
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}

	gameState.mu.Lock()
	delete(gameState.clients, conn)
	gameState.mu.Unlock()
}

// HTTP обработчики

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	// Очищаем папку package
	os.RemoveAll("package")
	os.MkdirAll("package", 0755)

	// Очищаем состояние игры
	gameState.players = make(map[int]*Player)
	gameState.nextNPCId = 100
	gameState.answeredQuestions = make(map[string]bool)
	gameState.state = StateJoining

	var siqBytes []byte
	var err error

	// Пробуем получить файл из multipart form (если есть)
	if err := r.ParseMultipartForm(100 << 20); err == nil { // 100 MB
		// Пробуем получить файл из поля "file" или "siq"
		var file multipart.File
		
		if f, _, err := r.FormFile("file"); err == nil {
			file = f
		} else if f, _, err := r.FormFile("siq"); err == nil {
			file = f
		}
		
		if file != nil {
			defer file.Close()
			siqBytes, err = io.ReadAll(file)
			if err != nil {
				http.Error(w, "Error reading SIQ file", http.StatusBadRequest)
				return
			}
		} else {
			// Если multipart form есть, но файла нет, читаем из body
			siqBytes, err = io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Error reading SIQ file", http.StatusBadRequest)
				return
			}
		}
	} else {
		// Если multipart form не удалось распарсить, читаем из body
		siqBytes, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Error reading SIQ file", http.StatusBadRequest)
			return
		}
	}
	defer r.Body.Close()

	if len(siqBytes) == 0 {
		http.Error(w, "No file provided", http.StatusBadRequest)
		return
	}

	// Создаем временный файл для SIQ
	tmpFile, err := os.CreateTemp("", "upload_*.siq")
	if err != nil {
		http.Error(w, "Error creating temporary file", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpFile.Name()) // Удаляем временный файл после использования
	defer tmpFile.Close()

	// Записываем SIQ данные во временный файл
	if _, err := tmpFile.Write(siqBytes); err != nil {
		http.Error(w, "Error writing temporary file", http.StatusInternalServerError)
		return
	}
	tmpFile.Close()

	// Извлекаем SIQ файл в папку package
	if err := siziph.Extract(tmpFile.Name(), "package"); err != nil {
		http.Error(w, fmt.Sprintf("Error extracting SIQ file: %v", err), http.StatusInternalServerError)
		return
	}

	// Загружаем content.json из извлеченного пакета
	contentJsonPath := filepath.Join("package", "content.json")
	jsonBytes, err := os.ReadFile(contentJsonPath)
	if err != nil {
		// Если content.json не найден, пробуем загрузить content.xml и преобразовать
		contentXmlPath := filepath.Join("package", "content.xml")
		if _, err := os.Stat(contentXmlPath); err == nil {
			// siziph автоматически конвертирует XML в JSON, попробуем еще раз
			jsonBytes, err = os.ReadFile(contentJsonPath)
			if err != nil {
				http.Error(w, "Error reading content.json after extraction", http.StatusInternalServerError)
				return
			}
		} else {
			http.Error(w, "Error reading content.json: content.json not found", http.StatusInternalServerError)
			return
		}
	}

	var packageJson map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &packageJson); err != nil {
		http.Error(w, fmt.Sprintf("Error parsing content.json: %v", err), http.StatusBadRequest)
		return
	}

	gameState.packageJson = packageJson

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Package uploaded successfully"))
}

func handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateJoining {
		http.Error(w, "Game is not in joining state", http.StatusBadRequest)
		return
	}

	err := r.ParseMultipartForm(32 << 20)
	if err != nil {
		http.Error(w, "Error parsing multipart form", http.StatusBadRequest)
		return
	}

	idStr := r.FormValue("id")
	name := r.FormValue("name")
	
	if idStr == "" || name == "" {
		http.Error(w, "Missing id or name", http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	// Сохраняем фото
	photo, photoHeader, err := r.FormFile("photo")
	if err == nil {
		defer photo.Close()
		
		ext := filepath.Ext(photoHeader.Filename)
		if ext == "" {
			ext = ".jpg"
		}
		
		photoPath := filepath.Join("players", fmt.Sprintf("%d%s", id, ext))
		dst, err := os.Create(photoPath)
		if err == nil {
			io.Copy(dst, photo)
			dst.Close()
		}
	}

	gameState.players[id] = &Player{
		ID:    id,
		Name:  name,
		Score: 0,
		IsNPC: false,
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Player joined successfully"))
}

func handleJoinNPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseMultipartForm(10 << 20)
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	characterName := r.FormValue("character")
	if characterName == "" {
		http.Error(w, "Missing character", http.StatusBadRequest)
		return
	}

	// Ищем персонажа по имени
	npcChar, exists := npcCharactersMap[strings.ToLower(characterName)]
	if !exists {
		http.Error(w, "Character not found", http.StatusNotFound)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateJoining {
		http.Error(w, "Game is not in joining state", http.StatusBadRequest)
		return
	}

	id := gameState.nextNPCId
	gameState.nextNPCId++

	// Копируем фото из папки npc_characters в папку players
	sourcePhoto := filepath.Join("npc_characters", npcChar.Photo)
	destPhoto := filepath.Join("players", fmt.Sprintf("%d%s", id, filepath.Ext(npcChar.Photo)))
	
	if err := copyFile(sourcePhoto, destPhoto); err != nil {
		log.Printf("Warning: Failed to copy NPC photo: %v", err)
		// Продолжаем даже если не удалось скопировать фото
	}

	gameState.players[id] = &Player{
		ID:           id,
		Name:         npcChar.Name,
		Score:        0,
		IsNPC:        true,
		NPCCharacter: npcChar,
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("NPC joined with id %d", id)))
}

func handleJoinShowman(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseMultipartForm(10 << 20)
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	characterName := r.FormValue("character")
	if characterName == "" {
		http.Error(w, "Missing character", http.StatusBadRequest)
		return
	}

	// Ищем персонажа по имени
	npcChar, exists := npcCharactersMap[strings.ToLower(characterName)]
	if !exists {
		http.Error(w, "Character not found", http.StatusNotFound)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateJoining {
		http.Error(w, "Game is not in joining state", http.StatusBadRequest)
		return
	}

	// Всегда используем ID 1000 для ведущего
	id := 1000

	// Удаляем предыдущего игрока с ID 1000, если он существует
	if _, exists := gameState.players[id]; exists {
		// Удаляем фото предыдущего игрока, если оно есть
		extensions := []string{".jpg", ".jpeg", ".png", ".gif"}
		for _, ext := range extensions {
			photoPath := filepath.Join("players", fmt.Sprintf("%d%s", id, ext))
			os.Remove(photoPath)
		}
		delete(gameState.players, id)
	}

	// Копируем фото из папки npc_characters в папку players
	sourcePhoto := filepath.Join("npc_characters", npcChar.Photo)
	destPhoto := filepath.Join("players", fmt.Sprintf("%d%s", id, filepath.Ext(npcChar.Photo)))
	
	if err := copyFile(sourcePhoto, destPhoto); err != nil {
		log.Printf("Warning: Failed to copy showman photo: %v", err)
		// Продолжаем даже если не удалось скопировать фото
	}

	gameState.players[id] = &Player{
		ID:           id,
		Name:         npcChar.Name,
		Score:        0,
		IsNPC:        true,
		NPCCharacter: npcChar,
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("Showman joined with id %d", id)))
}

func handleNPCCharacters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(npcCharacters)
}

func loadNPCCharacters() error {
	jsonPath := filepath.Join("npc_characters", "characters.json")
	
	jsonBytes, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("failed to read NPC characters file: %w", err)
	}

	if err := json.Unmarshal(jsonBytes, &npcCharacters); err != nil {
		return fmt.Errorf("failed to parse NPC characters JSON: %w", err)
	}

	// Заполняем map для быстрого поиска
	npcCharactersMap = make(map[string]*NPCCharacter)
	for i := range npcCharacters {
		char := &npcCharacters[i]
		npcCharactersMap[strings.ToLower(char.Name)] = char
	}

	log.Printf("Loaded %d NPC characters", len(npcCharacters))
	return nil
}

func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}

func handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.RLock()
	state := gameState.state
	gameState.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(state))
}

func handleScores(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.RLock()
	scores := make([]map[string]interface{}, 0, len(gameState.players))
	for _, player := range gameState.players {
		scores = append(scores, map[string]interface{}{
			"id":    player.ID,
			"name":  player.Name,
			"score": player.Score,
		})
	}
	gameState.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(scores)
}

func handleData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.RLock()
	players := make([]map[string]interface{}, 0, len(gameState.players))
	for _, player := range gameState.players {
		players = append(players, map[string]interface{}{
			"id":   player.ID,
			"name": player.Name,
		})
	}
	packageJson := gameState.packageJson
	gameState.mu.RUnlock()

	response := map[string]interface{}{
		"packageJson": packageJson,
		"players":     players,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=media.zip")

	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()

	// Добавляем медиа из package
	filepath.Walk("package", func(path string, info os.FileInfo, err error) error {
		baseName := filepath.Base(path)
		if err != nil || info.IsDir() || baseName == "package.json" || baseName == "content.json" || baseName == "content.xml" {
			return nil
		}
		
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()

		relPath, _ := filepath.Rel("package", path)
		zipPath := filepath.Join("questions", relPath)
		
		zipFile, err := zipWriter.Create(zipPath)
		if err != nil {
			return nil
		}

		io.Copy(zipFile, file)
		return nil
	})

	// Добавляем фото игроков
	filepath.Walk("players", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()

		filename := filepath.Base(path)
		zipPath := filepath.Join("players", filename)
		
		zipFile, err := zipWriter.Create(zipPath)
		if err != nil {
			return nil
		}

		io.Copy(zipFile, file)
		return nil
	})
}

func handleCurrentPlayer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.RLock()
	currentPlayerId := gameState.currentPlayerId
	gameState.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(strconv.Itoa(currentPlayerId)))
}

func handleCurrentRound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.RLock()
	roundNum := gameState.roundNum
	gameState.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(strconv.Itoa(roundNum)))
}

func handlePlayerState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	gameState.mu.RLock()
	player, exists := gameState.players[id]
	gameState.mu.RUnlock()

	if !exists {
		http.Error(w, "Player not found", http.StatusNotFound)
		return
	}

	response := map[string]interface{}{
		"name":  player.Name,
		"score": player.Score,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

type PlayerAnswerRequest struct {
    playerId  int
    timestamp int64
}

// Дополнительные поля для GameState для управления игрой
type GameStateInternal struct {
	acknowledgesReceived     map[int]bool
	questionShownReceived    map[int]bool
	requestAnswerReceived    []PlayerAnswerRequest
	playersAnswered          []int
	startAcknowledgeReceived map[int]bool
	selectedQuestionId       string
	selectedQuestionTime     time.Time
	canAnswerTimestamp       int64
	waitAnswerTimeout        *time.Timer
	acknowledgeTimeout       *time.Timer
	questionShownTimeout     *time.Timer
	showAnswerTimeout        *time.Timer
	startAcknowledgeTimeout  *time.Timer
}

var gameStateInternal = &GameStateInternal{
	acknowledgesReceived:     make(map[int]bool),
	questionShownReceived:    make(map[int]bool),
	requestAnswerReceived:    make([]PlayerAnswerRequest, 0),
	playersAnswered      :    make([]int, 0),
	startAcknowledgeReceived: make(map[int]bool),
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	log.Printf("call handleStart()")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateJoining {
		http.Error(w, "Game is not in joining state", http.StatusBadRequest)
		return
	}
	gameState.state = StateStartAck

	// Выбираем случайного первого игрока
	playerIds := make([]int, 0, len(gameState.players))
	for id := range gameState.players {
		if id < 1000 {            
			playerIds = append(playerIds, id)
		}
	}

	if len(playerIds) == 0 {
		http.Error(w, "No eligible players", http.StatusBadRequest)
		return
	}

	// Случайный выбор первого игрока
	idx := time.Now().UnixNano() % int64(len(playerIds))

	gameState.currentPlayerId = playerIds[idx]

	gameState.roundNum = 1
	gameState.gameStartTime = time.Now().UTC()

	log.Printf("currentPlayerId is %d", gameState.currentPlayerId)

	
	// Инициализируем карту подтверждений для старта
	gameStateInternal.startAcknowledgeReceived = make(map[int]bool)
	
	// Отправляем сообщение start и ждем подтверждений
	gameState.broadcastMessage("start", map[string]interface{}{})
	log.Printf("ws start")

	// Таймаут для startacknowledge
	if gameStateInternal.startAcknowledgeTimeout != nil {
		gameStateInternal.startAcknowledgeTimeout.Stop()
	}
	gameStateInternal.startAcknowledgeTimeout = time.AfterFunc(30*time.Second, func() {
		gameState.mu.Lock()
		transitionToSelectQuestionForced()
		gameState.mu.Unlock()
	})

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Game started"))
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	log.Printf("call handleReset()")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	// Очищаем папку package
	os.RemoveAll("package")
	os.MkdirAll("package", 0755)

	// Очищаем папку players
	os.RemoveAll("players")
	os.MkdirAll("players", 0755)

	// Очищаем состояние игры
	gameState.players = make(map[int]*Player)
	gameState.nextNPCId = 100
	gameState.answeredQuestions = make(map[string]bool)
	gameState.state = StateJoining
	gameState.currentPlayerId = 0
	gameState.roundNum = 0
	gameState.packageJson = nil

	// Очищаем внутреннее состояние
	gameStateInternal.acknowledgesReceived = make(map[int]bool)
	gameStateInternal.questionShownReceived = make(map[int]bool)
	gameStateInternal.requestAnswerReceived = make([]PlayerAnswerRequest, 0)
	gameStateInternal.playersAnswered = make([]int, 0)
	gameStateInternal.startAcknowledgeReceived = make(map[int]bool)
	gameStateInternal.selectedQuestionId = ""
	gameStateInternal.canAnswerTimestamp = 0

	// Останавливаем все таймеры
	if gameStateInternal.waitAnswerTimeout != nil {
		gameStateInternal.waitAnswerTimeout.Stop()
		gameStateInternal.waitAnswerTimeout = nil
	}
	if gameStateInternal.acknowledgeTimeout != nil {
		gameStateInternal.acknowledgeTimeout.Stop()
		gameStateInternal.acknowledgeTimeout = nil
	}
	if gameStateInternal.questionShownTimeout != nil {
		gameStateInternal.questionShownTimeout.Stop()
		gameStateInternal.questionShownTimeout = nil
	}
	if gameStateInternal.showAnswerTimeout != nil {
		gameStateInternal.showAnswerTimeout.Stop()
		gameStateInternal.showAnswerTimeout = nil
	}
	if gameStateInternal.startAcknowledgeTimeout != nil {
		gameStateInternal.startAcknowledgeTimeout.Stop()
		gameStateInternal.startAcknowledgeTimeout = nil
	}

	// Отправляем сообщение о сбросе через WebSocket
	gameState.broadcastMessage("reset", map[string]interface{}{})

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Game reset successfully"))
}

func getQuestionStringId(round int, theme int, question int) (string, error) {
	result := fmt.Sprintf("%d_%d_%d", round, theme, question)
	return result, nil
}

func handleSelectQuestion(w http.ResponseWriter, r *http.Request) {
	log.Printf("call handleSelectQuestion")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateSelectQuestion {
		http.Error(w, "Game is not in select-question state", http.StatusBadRequest)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	log.Printf("handleSelectQuestion query is ok")

	idRoundString := r.FormValue("round")
	idThemeString := r.FormValue("theme")
	idQuestString := r.FormValue("question")
	idPlayerString := r.FormValue("player")

	if idRoundString == "" || idThemeString == "" || idQuestString == "" || idPlayerString == ""{
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}

	idRound, _ := strconv.Atoi(idRoundString)
	idTheme, _ := strconv.Atoi(idThemeString)
	idQuest, _ := strconv.Atoi(idQuestString)
	stringId, _ := getQuestionStringId(idRound, idTheme, idQuest)
	idPlayer, _ := strconv.Atoi(idPlayerString)

	log.Printf("handleSelectQuestion parameters parsed")

	// Проверяем что это текущий игрок
	if idPlayer != gameState.currentPlayerId {
		log.Printf("Not current player")
		http.Error(w, "Not current player", http.StatusBadRequest)
		return
	}

	// Проверяем что вопрос еще не отвечен
	if gameState.answeredQuestions[stringId] {
		log.Printf("Question already answered")
		http.Error(w, "Question already answered", http.StatusBadRequest)
		return
	}

	gameStateInternal.selectedQuestionId = stringId

	log.Printf("select question %s", stringId)

	// Отправляем оповещение
	gameState.broadcastMessage("questionselected", map[string]interface{}{
		"id":        stringId,
		"isspecial": false,
	})

	gameState.state = StateQuestion

	gameStateInternal.questionShownReceived = make(map[int]bool)
	gameStateInternal.requestAnswerReceived = make([]PlayerAnswerRequest, 0)
	gameStateInternal.playersAnswered       = make([]int, 0)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Question selected"))
}

func transitionToSelectQuestionAfterStart() {
	if gameState.state != StateStartAck {
		return
	}

	allAcknowledged := true
	for id, player := range gameState.players {
		if !player.IsNPC && !gameStateInternal.startAcknowledgeReceived[id] {
			allAcknowledged = false
			log.Printf("player not started: %i", id)
			break
		}
	}

	if allAcknowledged {
		log.Printf("all player started")
		transitionToSelectQuestionForced()
	}
}

func transitionToSelectQuestionForced() {
	log.Printf("call transitionToSelectQuestionForced()")

	// предотвращаем повторные вызовы
	if gameState.state != StateStartAck {
		log.Printf("call transitionToSelectQuestionForced stopped because state is %s", gameState.state)
		return
	}

	gameState.state = StateSelectQuestion

	log.Printf("before send table")
	table, _, _ := getQuestionsTable(gameState.roundNum - 1, true)
	gameState.broadcastMessage("questionstable", map[string]interface{}{
		"table": table,
	})
	log.Printf("ws table")

	// ход NPC
	if p := gameState.players[gameState.currentPlayerId]; p != nil && p.IsNPC {
		go handleNPCTurn()
	}
}

/*func transitionToQuestion() {
	log.Printf("call transitionToQuestion")

	// Проверяем что все реальные игроки отправили acknowledge
	allAcknowledged := true
	for id, player := range gameState.players {
		if !player.IsNPC {
			if !gameStateInternal.acknowledgesReceived[id] {
				allAcknowledged = false
				break
			}
		}
	}

	if allAcknowledged {
		gameState.state = StateQuestion
		gameStateInternal.questionShownReceived = make(map[int]bool)
		gameStateInternal.requestAnswerReceived = make([]PlayerAnswerRequest, 0)
		gameStateInternal.playersAnswered       = make([]int, 0)

		// Время показа вопроса (с небольшой задержкой)
		showTime := time.Now().UTC().Add(500 * time.Millisecond)
		gameStateInternal.selectedQuestionTime = showTime

		gameState.broadcastMessage("showquestion", map[string]interface{}{
			"id":   gameStateInternal.selectedQuestionId,
			"time": showTime.Unix() * 1000, // UTC в миллисекундах
		})

		// Таймаут для questionbeenshown
		if gameStateInternal.questionShownTimeout != nil {
			gameStateInternal.questionShownTimeout.Stop()
		}
		gameStateInternal.questionShownTimeout = time.AfterFunc(3*time.Second, func() {
			gameState.mu.Lock()
			if gameState.state == StateQuestion {
				sendCanAnswer()
			}
			gameState.mu.Unlock()
		})
	}
}*/

/*func handleAcknowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateStartAck {
		http.Error(w, "Game is not in wait-acknowledge state", http.StatusBadRequest)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	idStr := r.FormValue("id")
	if idStr == "" {
		http.Error(w, "Missing id", http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	gameStateInternal.acknowledgesReceived[id] = true

	// Проверяем можно ли переходить к question
	transitionToQuestion()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Acknowledged"))
}*/

func handleStartAcknowledge(w http.ResponseWriter, r *http.Request) {
	log.Printf("call handleStartAcknowledge()")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	// Если уже перешли — просто OK
	if gameState.state == StateSelectQuestion {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Already started"))
		log.Printf("Already started")
		return
	}

	err := r.ParseForm()
	if err != nil {
		log.Printf("Error parsing form")
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	idStr := r.FormValue("id")
	if idStr == "" {
		log.Printf("Missing id")
		http.Error(w, "Missing id", http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		log.Printf("Invalid id")
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	player, exists := gameState.players[id]
	if !exists {
		log.Printf("Player not found")
		http.Error(w, "Player not found", http.StatusNotFound)
		return
	}
	
	if player.IsNPC {
		log.Printf("NPC players don't need to acknowledge")
		http.Error(w, "NPC players don't need to acknowledge", http.StatusBadRequest)
		return
	}

	// сохраняем ack
	gameStateInternal.startAcknowledgeReceived[id] = true
	log.Printf("player acknoledged %i", id)

	// проверяем, не все ли уже ответили
	transitionToSelectQuestionAfterStart()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Start acknowledged"))
}

func handleQuestionBeenShown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	playerIdStr := r.FormValue("playerId")
	if playerIdStr == "" {
		http.Error(w, "Missing playerId", http.StatusBadRequest)
		return
	}

	playerId, err := strconv.Atoi(playerIdStr)
	if err != nil {
		http.Error(w, "Invalid playerId", http.StatusBadRequest)
		return
	}

	gameStateInternal.questionShownReceived[playerId] = true

	// Проверяем все ли реальные игроки просмотрели вопрос
	allShown := true
	for id, player := range gameState.players {
		if !player.IsNPC {
			if !gameStateInternal.questionShownReceived[id] {
				allShown = false
				break
			}
		}
	}

	if allShown {
		sendCanAnswer()
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Question shown"))
}

func sendCanAnswer() {
	if gameStateInternal.questionShownTimeout != nil {
		gameStateInternal.questionShownTimeout.Stop()
	}

	// Время когда всем станет доступна кнопка
	canAnswerTime := time.Now().UTC().Add(500 * time.Millisecond)
	gameStateInternal.canAnswerTimestamp = canAnswerTime.Unix() * 1000

	gameState.broadcastMessage("cananswer", map[string]interface{}{
		"timestamp": gameStateInternal.canAnswerTimestamp,
	})
}

func handleRequestAnswer(w http.ResponseWriter, r *http.Request) {
	log.Printf("call handleRequestAnswer()")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateQuestion {
		http.Error(w, "Game is not in question state", http.StatusBadRequest)
		log.Printf("call handleRequestAnswer stopped because state is %s", gameState.state)
		return
	}

	gameState.broadcastMessage("stoptimer", map[string]interface{}{})

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	playerStr := r.FormValue("player")

	if playerStr == "" {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(playerStr)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	// Сохраняем запрос
	gameStateInternal.requestAnswerReceived = append(gameStateInternal.requestAnswerReceived, PlayerAnswerRequest{
		playerId:  id,
		timestamp: 0,
	})
	if !contains(gameStateInternal.playersAnswered, id) {
		gameStateInternal.playersAnswered = append(gameStateInternal.playersAnswered, id)
	}
	log.Printf("playersAnswered = %s", gameStateInternal.playersAnswered)
	

	// Если это первый запрос, запускаем таймер для обработки
	if len(gameStateInternal.requestAnswerReceived) == 1 {
		time.AfterFunc(3*time.Second, func() {
			gameState.mu.Lock()
			processAnswerRequests()
			gameState.mu.Unlock()
		})
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Answer request received"))
}

func processAnswerRequests() {
	log.Printf("call processAnswerRequests()")

	if gameState.state != StateQuestion {
		log.Printf("call processAnswerRequests stopped because state is %s", gameState.state)
		return
	}

	if (len(gameStateInternal.requestAnswerReceived) == 0) {
		log.Printf("no more people answered")
		return
	}

	// Находим random player who answered
	idx := time.Now().UnixNano() % int64(len(gameStateInternal.requestAnswerReceived))
	winnerId := gameStateInternal.requestAnswerReceived[idx].playerId
	log.Printf("winnerId %d", winnerId)

	if winnerId != -1 {
		gameState.state = StateWaitAnswer
		gameState.broadcastMessage("waitanswer", map[string]interface{}{
			"playerId": winnerId,
		})

		// Исключаем победителя
		gameStateInternal.requestAnswerReceived = append(
			gameStateInternal.requestAnswerReceived[:idx],
            gameStateInternal.requestAnswerReceived[idx+1:]...)
	}
}

func processNPCAnswers() {

}

func handleAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateWaitAnswer {
		http.Error(w, "Game is not in wait-answer state", http.StatusBadRequest)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	idQuest:= r.FormValue("id-quest")
	idPlayerStr := r.FormValue("id-player")
	text := r.FormValue("text")
	log.Printf("call handleAnswer(%s, %s, %s)", idQuest, idPlayerStr, text)

	if idQuest == "" || idPlayerStr == "" {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}

	idPlayer, err := strconv.Atoi(idPlayerStr)
	if err != nil {
		http.Error(w, "Invalid idPlayer", http.StatusBadRequest)
		return
	}

	checkAndProcessAnswer(idQuest, idPlayer, text)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Answer processed"))
}

func handleTimerDone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	idPlayerStr := r.FormValue("id-player")
	log.Printf("call handleTimerDone(%s)", idPlayerStr)

	if idPlayerStr == "" {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}

	idPlayer, err := strconv.Atoi(idPlayerStr)
	if err != nil {
		http.Error(w, "Invalid idPlayer", http.StatusBadRequest)
		return
	}
	if !contains(gameStateInternal.playersAnswered, idPlayer) {
		gameStateInternal.playersAnswered = append(gameStateInternal.playersAnswered, idPlayer)
	}

	if (isAllPlayersAnswered()) {
		showAnswer()
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Answer processed"))
}

func checkAndProcessAnswer(idQuest string, idPlayer int, answerText string) bool {
	log.Printf("call checkAndProcessAnswer(%s, %d, %s)", idQuest, idPlayer, answerText)

	question := getQuestion(idQuest)
	expectedAnswer := getAnswer(idQuest)
	
	// Get host player (ID 1000) and their character
	hostPlayer, exists := gameState.players[1000]
	hostName := "Ведущий" // Default fallback
	hostDescription := "(описание ведущего)"

	if exists && hostPlayer.NPCCharacter != nil {
		hostName = hostPlayer.NPCCharacter.Name
		hostDescription = hostPlayer.NPCCharacter.HostPrompt
	}
	
	claudeAnswer, _ := siclo.ValidateAnswer(hostName, "Пиши коротко, не называя то что написано на карточке ответа. " + hostDescription, 
		question, answerText, expectedAnswer);
	log.Printf("send to claude (%s, %s, %s, %s, %s)", hostName, "Пиши коротко, не называя то что написано на карточке ответа. " + hostDescription, 
		question, answerText, expectedAnswer)
	result := claudeAnswer.Result;
	hostSpeak := claudeAnswer.Justification;

	log.Printf("claudeAnswer is %b", result)
	log.Printf("claudeAnswer is %s", hostSpeak)

	queueIsEmpty := len(gameStateInternal.requestAnswerReceived) == 0
	done := isAllPlayersAnswered()

	gameState.broadcastMessage("validated", map[string]interface{}{
		"idQuest":  idQuest,
		"playerId": idPlayer,
		"result":   result,
		"unfreeze": queueIsEmpty,
	})

	gameState.broadcastMessage("playertalk", map[string]interface{}{
		"playerId": idPlayer,
		"text":   answerText,
	})

	gameState.broadcastMessage("hosttalk", map[string]interface{}{
		"text":   hostSpeak,
	})

	// ответ верный
	if (result) {
		gameState.currentPlayerId = idPlayer
		gameState.players[idPlayer].Score += getScore(gameStateInternal.selectedQuestionId);
		showAnswer()
		return result
	}
	
	gameState.players[idPlayer].Score -= getScore(gameStateInternal.selectedQuestionId);

	// остались люди, кто нажал на кнопку
	if (!queueIsEmpty) {
		log.Printf("ball to other player")
		gameState.state = StateQuestion
		processAnswerRequests()
		return result
	}
	
	// остались люди кто еще не попробовал ответить
	if (!done) {
		gameState.state = StateQuestion
		return result
	}

	showAnswer()

	return result
}

func isAllPlayersAnswered() bool {
	log.Printf("playersAnswered = %d, players = %d", len(gameStateInternal.playersAnswered), numberOfRealPlayers())
	return len(gameStateInternal.playersAnswered) == numberOfRealPlayers()
}

func showAnswer() {
	log.Printf("call showAnswer()")

	gameState.broadcastMessage("showanswer", map[string]interface{}{
		"idQuest": gameStateInternal.selectedQuestionId,
	})

	gameState.answeredQuestions[gameStateInternal.selectedQuestionId] = true

	// переходим к вопросам
	time.AfterFunc(5 * time.Second, func() {
		transitionToSelectQuestion()
	})
}

func transitionToSelectQuestion() {
	log.Printf("call transitionToSelectQuestion()")

	gameState.state = StateSelectQuestion

	log.Printf("before send table")
	table, isempty, _  := getQuestionsTable(gameState.roundNum - 1, true)

	if (isempty) {
		log.Printf("next round")
		if isThereNextRound(gameState.roundNum) {
			gameState.roundNum++
		} else {
			gameState.state = StateUnknown
			return
		}
	}
	table, isempty, _  = getQuestionsTable(gameState.roundNum - 1, true)

	gameState.broadcastMessage("questionstable", map[string]interface{}{
		"table": table,
	})
	log.Printf("ws table")

	return
}

func handleNPCTurn() {
	
}

func getScore(questionId string) int {
	var a, b, c int
	_, err := fmt.Sscanf(questionId, "%d_%d_%d", &a, &b, &c)
	priceStr := getQuestionPrice(a-1, b-1, c-1)
	price, err := strconv.Atoi(priceStr)
	if ( err != nil) {
		return 0;
	}
	log.Printf("question questionId gives: %d", price)
	return price
}

func getQuestion(questionId string) string {
	var a, b, c int
	_, err := fmt.Sscanf(questionId, "%d_%d_%d", &a, &b, &c)
	question := getQuestionText(a-1, b-1, c-1)
	if ( err != nil) {
		log.Printf("question questionId is: %s", "err!")
		return "";
	}
	log.Printf("question questionId is: %s", question)
	return question
}

func getAnswer(questionId string) string {
	var a, b, c int
	_, err := fmt.Sscanf(questionId, "%d_%d_%d", &a, &b, &c)
	if ( err != nil) {
		return "";
	}
	
	question := getQuestionAnswer1(a-1, b-1, c-1)
	// If getQuestionAnswer1 didn't return a result, fallback to getQuestionAnswer2
	if question == "" {
		question = getQuestionAnswer2(a-1, b-1, c-1)
	}
	
	log.Printf("question answer is: %s", question)
	return question
}

// Функции для работы с вопросами

func getQuestionsTable(roundNum int, filterAnswered bool) (string, bool, error) {
	var result []string
	isempty := true

	log.Printf("answered: %+v", gameState.answeredQuestions)

	themesCount := getThemesCountForRound(roundNum)
	for i := 0; i < themesCount; i++ {
		result = append(result, getThemeName(roundNum, i))
		questionsCount := getQuestionsCount(roundNum, i)
		for j := 0; j < 10; j++ {
			if (j < questionsCount) {
				questId, _ := getQuestionStringId(roundNum+1, i+1, j+1)
				if (filterAnswered && gameState.answeredQuestions[questId]) {
					result = append(result, "-")
				} else {
					isempty = false
					log.Printf("not empty: %d %d %d", roundNum, i, j)
					result = append(result, getQuestionPrice(roundNum, i, j))
				}
			} else {
				result = append(result, "")
			}
		}
	}

	log.Printf("isempty = %t", isempty)
	return strings.Join(result, "|"), isempty, nil
}

func getThemesCountForRound(roundNum int) int {
	raw, _ := json.Marshal(gameState.packageJson)
	jsonString := string(raw)

	path := fmt.Sprintf("rounds.0.round.%d.themes.0.theme.#", roundNum)
	count := gjson.Get(jsonString, path).Int()
	return int(count)
}

func getThemeName(roundNum int, themeNum int) string {
	raw, _ := json.Marshal(gameState.packageJson)
	jsonString := string(raw)

	path := fmt.Sprintf("rounds.0.round.%d.themes.0.theme.%d.@name", roundNum, themeNum)
	return gjson.Get(jsonString, path).String()
}

func getQuestionsCount(roundNum int, themeNum int) int {
	raw, _ := json.Marshal(gameState.packageJson)
	jsonString := string(raw)

	path := fmt.Sprintf("rounds.0.round.%d.themes.0.theme.%d.questions.0.question.#", roundNum, themeNum)
	count := gjson.Get(jsonString, path).Int()
	return int(count)
}

func getQuestionPrice(roundNum int, themeNum int, questNum int) string {
	raw, _ := json.Marshal(gameState.packageJson)
	jsonString := string(raw)

	path := fmt.Sprintf("rounds.0.round.%d.themes.0.theme.%d.questions.0.question.%d.@price", roundNum, themeNum, questNum)
	return gjson.Get(jsonString, path).String()
}

func getQuestionText(roundNum int, themeNum int, questNum int) string {
    raw, err := json.Marshal(gameState.packageJson)
    if err != nil {
        return ""
    }

    jsonString := string(raw)

    // 1. get all params
    basePath := fmt.Sprintf(
        "rounds.0.round.%d.themes.0.theme.%d.questions.0.question.%d.params.0.param",
        roundNum, themeNum, questNum,
    )

    params := gjson.Get(jsonString, basePath).Array()

    var questionParam gjson.Result

    // 2. find param with @name == "question"
    for _, p := range params {
        if p.Get("@name").String() == "question" {
            questionParam = p
            break
        }
    }

    if !questionParam.Exists() {
        return ""
    }

    // 3. get number of lines
    items := questionParam.Get("item").Array()

    // 4. loop and collect text lines
    parts := make([]string, 0, len(items))
    for _, item := range items {
        text := item.Get("#text").String()
        if text != "" {
            parts = append(parts, text)
        }
    }

    // 5. join lines
    return strings.Join(parts, " ")
}

func getQuestionAnswer1(roundNum int, themeNum int, questNum int) string {
	raw, _ := json.Marshal(gameState.packageJson)
	jsonString := string(raw)

	path := fmt.Sprintf(
		"rounds.0.round.%d.themes.0.theme.%d.questions.0.question.%d.right.0.answer.0.#text",
		roundNum, themeNum, questNum,
	)

	return gjson.Get(jsonString, path).String()
}

func getQuestionAnswer2(roundNum int, themeNum int, questNum int) string {
	raw, _ := json.Marshal(gameState.packageJson)
	jsonString := string(raw)

	path := fmt.Sprintf(
		"rounds.0.round.%d.themes.0.theme.%d.questions.0.question.%d.params.#.param.#(@name==\"answer\").item.#.#text|@flatten",
		roundNum, themeNum, questNum,
	)

	return gjson.Get(jsonString, path).String()
}

func getQuestionsForTheme(theme Theme) []Question {
	return theme.Questions
}

func checkAnswer(idQuest int, answer string) (bool, string) {
	gameState.mu.RLock()
	packageJson := gameState.packageJson
	gameState.mu.RUnlock()

	// Используем вспомогательную функцию для поиска вопроса
	questionMap, correctAnswer, _ := findQuestionById(packageJson, idQuest)
	if questionMap == nil {
		return false, "Вопрос не найден"
	}

	if correctAnswer == "" {
		return false, "Ответ не найден в вопросе"
	}

	// Простая проверка (можно улучшить)
	result := strings.EqualFold(strings.TrimSpace(answer), strings.TrimSpace(correctAnswer))
	comment := "" // В новой структуре комментарий может быть в другом месте, если нужно

	return result, comment
}

func getPointsForQuestion(idQuest int) int {
	gameState.mu.RLock()
	packageJson := gameState.packageJson
	gameState.mu.RUnlock()

	// Используем вспомогательную функцию для поиска вопроса
	questionMap, _, _ := findQuestionById(packageJson, idQuest)
	if questionMap == nil {
		return 0
	}

	// Извлекаем цену из @price
	priceStr := getString(questionMap, "@price")
	price, err := strconv.Atoi(priceStr)
	if err != nil {
		return 0
	}

	return price
}

type NPCAnswer struct {
	HaveAnswer bool
	Answer     string
}

func generateAnswer(npc *Player) NPCAnswer {
	return NPCAnswer{HaveAnswer: false}
}

func isThereNextRound(currentRound int) bool {
	gameState.mu.RLock()
	packageJson := gameState.packageJson
	gameState.mu.RUnlock()

	rounds, ok := packageJson["rounds"].([]interface{})
	if !ok {
		return false
	}

	// В новой структуре rounds - это массив объектов с полем "round"
	// Каждый round может содержать массив round элементов
	// Считаем общее количество раундов
	totalRounds := 0
	for _, roundData := range rounds {
		roundMap, ok := roundData.(map[string]interface{})
		if !ok {
			continue
		}
		if roundArray, ok := roundMap["round"].([]interface{}); ok {
			totalRounds += len(roundArray)
		} else {
			totalRounds++ // Если round не массив, считаем как один раунд
		}
	}

	return currentRound < totalRounds
}

// Вспомогательные функции

func getString(m map[string]interface{}, key string) string {
	if val, ok := m[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return ""
}

func getFloat(m map[string]interface{}, key string) float64 {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case int64:
			return float64(v)
		case string:
			// Пробуем распарсить строку как число
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

// Вспомогательные функции для работы с новой структурой JSON

// extractTextFromItem извлекает текст из item структуры
func extractTextFromItem(item interface{}) string {
	if itemMap, ok := item.(map[string]interface{}); ok {
		if text, ok := itemMap["#text"].(string); ok {
			return text
		}
	}
	return ""
}

// extractTextFromParam извлекает текст из param структуры по имени
func extractTextFromParam(param interface{}, paramName string) string {
	if paramMap, ok := param.(map[string]interface{}); ok {
		if name, ok := paramMap["@name"].(string); ok && name == paramName {
			if items, ok := paramMap["item"].([]interface{}); ok {
				for _, item := range items {
					if text := extractTextFromItem(item); text != "" {
						return text
					}
				}
			} else if item, ok := paramMap["item"].(map[string]interface{}); ok {
				return extractTextFromItem(item)
			}
		}
	}
	return ""
}

// findQuestionById находит вопрос по ID в новой структуре
func findQuestionById(packageJson map[string]interface{}, idQuest int) (map[string]interface{}, string, string) {
	rounds, ok := packageJson["rounds"].([]interface{})
	if !ok {
		return nil, "", ""
	}

	questionCounter := 1
	for _, roundData := range rounds {
		roundMap, ok := roundData.(map[string]interface{})
		if !ok {
			continue
		}

		roundArray, ok := roundMap["round"].([]interface{})
		if !ok {
			continue
		}

		for _, roundItem := range roundArray {
			roundItemMap, ok := roundItem.(map[string]interface{})
			if !ok {
				continue
			}

			themesData, ok := roundItemMap["themes"].([]interface{})
			if !ok {
				continue
			}

			for _, themeData := range themesData {
				themeMap, ok := themeData.(map[string]interface{})
				if !ok {
					continue
				}

				themeArray, ok := themeMap["theme"].([]interface{})
				if !ok {
					continue
				}

				for _, themeItem := range themeArray {
					themeItemMap, ok := themeItem.(map[string]interface{})
					if !ok {
						continue
					}

					questionsData, ok := themeItemMap["questions"].([]interface{})
					if !ok {
						continue
					}

					for _, qData := range questionsData {
						qMap, ok := qData.(map[string]interface{})
						if !ok {
							continue
						}

						questionArray, ok := qMap["question"].([]interface{})
						if !ok {
							continue
						}

						for _, questionItem := range questionArray {
							questionMap, ok := questionItem.(map[string]interface{})
							if !ok {
								continue
							}

							// Генерируем ID на основе позиции
							currentId := questionCounter
							questionCounter++

							if currentId == idQuest {
								// Извлекаем тему
								themeName := getString(themeItemMap, "@name")
								
								// Извлекаем ответ
								answer := ""
								if params, ok := questionMap["params"].([]interface{}); ok {
									for _, paramGroup := range params {
										if paramGroupMap, ok := paramGroup.(map[string]interface{}); ok {
											if paramArray, ok := paramGroupMap["param"].([]interface{}); ok {
												for _, param := range paramArray {
													if text := extractTextFromParam(param, "answer"); text != "" {
														answer = text
														break
													}
												}
											}
										}
									}
								}

								return questionMap, answer, themeName
							}
						}
					}
				}
			}
		}
	}

	return nil, "", ""
}

func withCORS(h http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
        w.Header().Set("Access-Control-Allow-Credentials", "true")

        if r.Method == "OPTIONS" {
            w.WriteHeader(http.StatusOK)
            return
        }

        h.ServeHTTP(w, r)
    })
}

func numberOfAllPlayers() int {
	count := 0
	for id := range gameState.players {
		if id < 1000 {
			count++
		}
	}
	return count
}

func numberOfRealPlayers() int {
	count := 0
	for id := range gameState.players {
		if id < 100 {
			count++
		}
	}
	return count
}

func contains(xs []int, x int) bool {
    for _, v := range xs {
        if v == x {
            return true
        }
    }
    return false
}