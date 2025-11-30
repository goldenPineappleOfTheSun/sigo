package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	StateJoining          = "joining"
	StateSelectQuestion   = "select-question"
	StateWaitAcknowledge  = "wait-acknowledge"
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
	answeredQuestions map[int]bool
	clients           map[*websocket.Conn]bool
	broadcast         chan []byte
	upgrader          websocket.Upgrader
}

type Player struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Score int    `json:"score"`
	IsNPC bool   `json:"-"`
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

func init() {
	gameState = &GameState{
		state:             StateJoining,
		players:           make(map[int]*Player),
		nextNPCId:         100,
		answeredQuestions: make(map[int]bool),
		clients:           make(map[*websocket.Conn]bool),
		broadcast:         make(chan []byte, 256),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

func main() {
	// Создаем необходимые папки
	os.MkdirAll("package", 0755)
	os.MkdirAll("players", 0755)

	// Запускаем broadcaster для WebSocket
	go gameState.broadcaster()

	// HTTP эндпоинты
	http.HandleFunc("/upload", handleUpload)
	http.HandleFunc("/join", handleJoin)
	http.HandleFunc("/joinnpc", handleJoinNPC)
	http.HandleFunc("/state", handleState)
	http.HandleFunc("/scores", handleScores)
	http.HandleFunc("/data", handleData)
	http.HandleFunc("/media", handleMedia)
	http.HandleFunc("/currentplayer", handleCurrentPlayer)
	http.HandleFunc("/playerstate", handlePlayerState)
	http.HandleFunc("/start", handleStart)
	http.HandleFunc("/selectquestion", handleSelectQuestion)
	http.HandleFunc("/acknowledge", handleAcknowledge)
	http.HandleFunc("/questionbeenshown", handleQuestionBeenShown)
	http.HandleFunc("/requestanswer", handleRequestAnswer)
	http.HandleFunc("/answer", handleAnswer)
	
	// WebSocket эндпоинт
	http.HandleFunc("/ws", handleWebSocket)

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
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

	// Очищаем папку players
	os.RemoveAll("players")
	os.MkdirAll("players", 0755)

	// Очищаем состояние игры
	gameState.players = make(map[int]*Player)
	gameState.nextNPCId = 100
	gameState.answeredQuestions = make(map[int]bool)
	gameState.state = StateJoining

	err := r.ParseMultipartForm(32 << 20) // 32 MB
	if err != nil {
		http.Error(w, "Error parsing multipart form", http.StatusBadRequest)
		return
	}

	// Получаем JSON файл
	jsonFile, _, err := r.FormFile("data")
	if err != nil {
		http.Error(w, "Error reading JSON file", http.StatusBadRequest)
		return
	}
	defer jsonFile.Close()

	jsonBytes, err := io.ReadAll(jsonFile)
	if err != nil {
		http.Error(w, "Error reading JSON content", http.StatusBadRequest)
		return
	}

	var packageJson map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &packageJson); err != nil {
		http.Error(w, "Error parsing JSON", http.StatusBadRequest)
		return
	}

	gameState.packageJson = packageJson

	// Сохраняем JSON файл
	jsonPath := filepath.Join("package", "package.json")
	if err := os.WriteFile(jsonPath, jsonBytes, 0644); err != nil {
		http.Error(w, "Error saving JSON file", http.StatusInternalServerError)
		return
	}

	// Получаем медиа файлы
	files := r.MultipartForm.File["media"]
	for _, fileHeader := range files {
		file, err := fileHeader.Open()
		if err != nil {
			continue
		}
		defer file.Close()

		dst, err := os.Create(filepath.Join("package", fileHeader.Filename))
		if err != nil {
			continue
		}
		defer dst.Close()

		io.Copy(dst, file)
	}

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

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateJoining {
		http.Error(w, "Game is not in joining state", http.StatusBadRequest)
		return
	}

	character := r.FormValue("character")
	if character == "" {
		http.Error(w, "Missing character", http.StatusBadRequest)
		return
	}

	id := gameState.nextNPCId
	gameState.nextNPCId++

	gameState.players[id] = &Player{
		ID:    id,
		Name:  character,
		Score: 0,
		IsNPC: true,
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("NPC joined with id %d", id)))
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
		if err != nil || info.IsDir() || filepath.Base(path) == "package.json" {
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

// Дополнительные поля для GameState для управления игрой
type GameStateInternal struct {
	acknowledgesReceived    map[int]bool
	questionShownReceived   map[int]bool
	requestAnswerReceived   map[int]int64 // playerId -> timestamp
	selectedQuestionId      int
	selectedQuestionTime    time.Time
	canAnswerTimestamp      int64
	waitAnswerTimeout       *time.Timer
	acknowledgeTimeout      *time.Timer
	questionShownTimeout    *time.Timer
	showAnswerTimeout       *time.Timer
}

var gameStateInternal = &GameStateInternal{
	acknowledgesReceived:  make(map[int]bool),
	questionShownReceived: make(map[int]bool),
	requestAnswerReceived: make(map[int]int64),
}

func handleStart(w http.ResponseWriter, r *http.Request) {
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

	// Выбираем случайного первого игрока
	playerIds := make([]int, 0, len(gameState.players))
	for id := range gameState.players {
		playerIds = append(playerIds, id)
	}
	if len(playerIds) == 0 {
		http.Error(w, "No players", http.StatusBadRequest)
		return
	}

	// Случайный выбор первого игрока
	gameState.currentPlayerId = playerIds[time.Now().UnixNano()%int64(len(playerIds))]
	gameState.roundNum = 1
	gameState.gameStartTime = time.Now().UTC()
	gameState.state = StateSelectQuestion

	// Отправляем таблицу вопросов
	table := getQuestionsTable(gameState.roundNum)
	gameState.broadcastMessage("questionstable", map[string]interface{}{
		"table": table,
	})

	// Если текущий игрок - NPC, обрабатываем его ход
	if player := gameState.players[gameState.currentPlayerId]; player != nil && player.IsNPC {
		go handleNPCTurn()
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Game started"))
}

func handleSelectQuestion(w http.ResponseWriter, r *http.Request) {
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

	idQuestStr := r.FormValue("idQuest")
	idPlayerStr := r.FormValue("idPlayer")

	if idQuestStr == "" || idPlayerStr == "" {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}

	idQuest, err := strconv.Atoi(idQuestStr)
	if err != nil {
		http.Error(w, "Invalid idQuest", http.StatusBadRequest)
		return
	}

	idPlayer, err := strconv.Atoi(idPlayerStr)
	if err != nil {
		http.Error(w, "Invalid idPlayer", http.StatusBadRequest)
		return
	}

	// Проверяем что это текущий игрок
	if idPlayer != gameState.currentPlayerId {
		http.Error(w, "Not current player", http.StatusBadRequest)
		return
	}

	// Проверяем что вопрос еще не отвечен
	if gameState.answeredQuestions[idQuest] {
		http.Error(w, "Question already answered", http.StatusBadRequest)
		return
	}

	gameStateInternal.selectedQuestionId = idQuest
	gameState.state = StateWaitAcknowledge
	gameStateInternal.acknowledgesReceived = make(map[int]bool)

	// Отправляем оповещение
	gameState.broadcastMessage("questionselected", map[string]interface{}{
		"id":        idQuest,
		"isspecial": false,
	})

	// Таймаут для acknowledge
	if gameStateInternal.acknowledgeTimeout != nil {
		gameStateInternal.acknowledgeTimeout.Stop()
	}
	gameStateInternal.acknowledgeTimeout = time.AfterFunc(5*time.Second, func() {
		gameState.mu.Lock()
		if gameState.state == StateWaitAcknowledge {
			transitionToQuestion()
		}
		gameState.mu.Unlock()
	})

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Question selected"))
}

func transitionToQuestion() {
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

	if allAcknowledged || gameState.state == StateWaitAcknowledge {
		gameState.state = StateQuestion
		gameStateInternal.questionShownReceived = make(map[int]bool)
		gameStateInternal.requestAnswerReceived = make(map[int]int64)

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
}

func handleAcknowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateWaitAcknowledge {
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
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gameState.mu.Lock()
	defer gameState.mu.Unlock()

	if gameState.state != StateQuestion {
		http.Error(w, "Game is not in question state", http.StatusBadRequest)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	idStr := r.FormValue("id")
	timestampStr := r.FormValue("timestamp")

	if idStr == "" || timestampStr == "" {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid timestamp", http.StatusBadRequest)
		return
	}

	// Сохраняем запрос
	gameStateInternal.requestAnswerReceived[id] = timestamp

	// Если это первый запрос, запускаем таймер для обработки
	if len(gameStateInternal.requestAnswerReceived) == 1 {
		time.AfterFunc(500*time.Millisecond, func() {
			gameState.mu.Lock()
			processAnswerRequests()
			gameState.mu.Unlock()
		})
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Answer request received"))
}

func processAnswerRequests() {
	if gameState.state != StateQuestion {
		return
	}

	// Находим игрока с наименьшим timestamp
	minTimestamp := int64(^uint64(0) >> 1)
	winnerId := -1

	for id, ts := range gameStateInternal.requestAnswerReceived {
		if ts < minTimestamp {
			minTimestamp = ts
			winnerId = id
		}
	}

	if winnerId != -1 {
		gameState.state = StateWaitAnswer
		gameState.broadcastMessage("waitanswer", map[string]interface{}{
			"playerId": winnerId,
		})

		// Очищаем остальные запросы
		gameStateInternal.requestAnswerReceived = make(map[int]int64)

		// Если никто не ответил верно и время вышло, обрабатываем NPC
		time.AfterFunc(10*time.Second, func() {
			gameState.mu.Lock()
			if gameState.state == StateWaitAnswer {
				processNPCAnswers()
			}
			gameState.mu.Unlock()
		})
	}
}

func processNPCAnswers() {
	gameState.mu.Lock()
	if gameState.state != StateWaitAnswer {
		gameState.mu.Unlock()
		return
	}

	selectedQuestionId := gameStateInternal.selectedQuestionId
	playersCopy := make(map[int]*Player)
	for k, v := range gameState.players {
		playersCopy[k] = v
	}
	gameState.mu.Unlock()

	// Обрабатываем NPC по очереди
	for id, player := range playersCopy {
		if !player.IsNPC {
			continue
		}

		gameState.mu.Lock()
		if gameState.state != StateWaitAnswer {
			gameState.mu.Unlock()
			return
		}
		gameState.mu.Unlock()

		answer := generateAnswer(player)
		if answer.HaveAnswer {
			// Отправляем ответ через /answer (как будто NPC сам отправил)
			formData := fmt.Sprintf("id-quest=%d&id-player=%d&text=%s", 
				selectedQuestionId, id, answer.Answer)
			req, _ := http.NewRequest("POST", "http://localhost:8080/answer", strings.NewReader(formData))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}

			// Проверяем изменилось ли состояние (если ответ был верный)
			gameState.mu.RLock()
			stateChanged := gameState.state != StateWaitAnswer
			gameState.mu.RUnlock()
			if stateChanged {
				return // Прекращаем если бот ответил верно
			}
		}
	}

	// Если никто не ответил верно, переходим к следующему вопросу
	gameState.mu.Lock()
	if gameState.state == StateWaitAnswer {
		// Помечаем вопрос как отвеченный (неверно)
		gameState.answeredQuestions[selectedQuestionId] = true
		// Переходим к следующему вопросу (current player не меняется)
		transitionToSelectQuestion(0) // 0 означает что никто не ответил верно
	}
	gameState.mu.Unlock()
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

	idQuestStr := r.FormValue("id-quest")
	idPlayerStr := r.FormValue("id-player")
	text := r.FormValue("text")

	if idQuestStr == "" || idPlayerStr == "" {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}

	idQuest, err := strconv.Atoi(idQuestStr)
	if err != nil {
		http.Error(w, "Invalid idQuest", http.StatusBadRequest)
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

func checkAndProcessAnswer(idQuest int, idPlayer int, answerText string) bool {
	result, comment := checkAnswer(idQuest, answerText)
	
	// Отправляем validated
	gameState.broadcastMessage("validated", map[string]interface{}{
		"idQuest":  idQuest,
		"idPlayer": idPlayer,
		"result":   result,
	})

	// Отправляем showmantalk
	gameState.broadcastMessage("showmantalk", map[string]interface{}{
		"text": comment,
	})

	// Отправляем playertalk
	playerText := answerText
	if answerText == "" {
		playerText = "не знаю"
	}
	gameState.broadcastMessage("playertalk", map[string]interface{}{
		"playerId": idPlayer,
		"text":     playerText,
	})

	// Обновляем очки
	player := gameState.players[idPlayer]
	if player != nil {
		points := getPointsForQuestion(idQuest)
		if result {
			player.Score += points
		} else {
			player.Score -= points
		}
	}

	// Если ответ верный, переходим в show-answer
	if result {
		gameState.answeredQuestions[idQuest] = true
		gameState.state = StateShowAnswer
		gameState.broadcastMessage("showanswer", map[string]interface{}{
			"questId": idQuest,
		})

		// Таймер на 10 секунд
		if gameStateInternal.showAnswerTimeout != nil {
			gameStateInternal.showAnswerTimeout.Stop()
		}
		gameStateInternal.showAnswerTimeout = time.AfterFunc(10*time.Second, func() {
			gameState.mu.Lock()
			transitionToSelectQuestion(idPlayer)
			gameState.mu.Unlock()
		})
		return true
	}
	return false
}

func transitionToSelectQuestion(answeredPlayerId int) {
	// Если был дан верный ответ, ответивший становится current player
	// Если никто не ответил верно, current player не меняется
	if answeredPlayerId > 0 {
		gameState.currentPlayerId = answeredPlayerId
	}
	
	gameState.state = StateSelectQuestion

	// Проверяем закончились ли вопросы раунда
	allAnswered := true
	themes := getThemesForRound(gameState.roundNum)
	for _, theme := range themes {
		questions := getQuestionsForTheme(theme)
		for _, q := range questions {
			if !gameState.answeredQuestions[q.ID] {
				allAnswered = false
				break
			}
		}
		if !allAnswered {
			break
		}
	}

	// Если все вопросы раунда отвечены, проверяем есть ли следующий раунд
	if allAnswered {
		if isThereNextRound(gameState.roundNum) {
			gameState.roundNum++
		} else {
			gameState.state = StateUnknown
			return
		}
	}

	// Отправляем таблицу вопросов
	table := getQuestionsTable(gameState.roundNum)
	gameState.broadcastMessage("questionstable", map[string]interface{}{
		"table": table,
	})

	// Если текущий игрок - NPC, обрабатываем его ход
	if player := gameState.players[gameState.currentPlayerId]; player != nil && player.IsNPC {
		go handleNPCTurn()
	}
}

func handleNPCTurn() {
	// Небольшая задержка
	time.Sleep(1 * time.Second)

	gameState.mu.Lock()
	currentPlayerId := gameState.currentPlayerId
	roundNum := gameState.roundNum
	answeredQuestions := make(map[int]bool)
	for k, v := range gameState.answeredQuestions {
		answeredQuestions[k] = v
	}
	gameState.mu.Unlock()

	// Получаем доступные вопросы
	themes := getThemesForRound(roundNum)
	availableQuestions := make([]int, 0)
	for _, theme := range themes {
		questions := getQuestionsForTheme(theme)
		for _, q := range questions {
			if !answeredQuestions[q.ID] {
				availableQuestions = append(availableQuestions, q.ID)
			}
		}
	}

	if len(availableQuestions) > 0 {
		// Случайный выбор вопроса
		selectedId := availableQuestions[time.Now().UnixNano()%int64(len(availableQuestions))]

		// Отправляем запрос на выбор вопроса
		formData := fmt.Sprintf("idQuest=%d&idPlayer=%d", selectedId, currentPlayerId)
		req, _ := http.NewRequest("POST", "http://localhost:8080/selectquestion", strings.NewReader(formData))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		http.DefaultClient.Do(req)
	}
}

// Функции для работы с вопросами

func getQuestionsTable(roundNum int) string {
	gameState.mu.RLock()
	answeredQuestions := make(map[int]bool)
	for k, v := range gameState.answeredQuestions {
		answeredQuestions[k] = v
	}
	gameState.mu.RUnlock()

	themes := getThemesForRound(roundNum)
	
	var table strings.Builder
	table.WriteString("Темы:\n")
	
	for _, theme := range themes {
		questions := getQuestionsForTheme(theme)
		table.WriteString(fmt.Sprintf("\n%s:\n", theme.Name))
		for _, q := range questions {
			if !answeredQuestions[q.ID] {
				table.WriteString(fmt.Sprintf("  [%d] %d\n", q.ID, q.Price))
			} else {
				table.WriteString(fmt.Sprintf("  [%d] ---\n", q.ID))
			}
		}
	}
	
	return table.String()
}

func getThemesForRound(roundNum int) []Theme {
	gameState.mu.RLock()
	packageJson := gameState.packageJson
	gameState.mu.RUnlock()

	if packageJson == nil {
		return []Theme{}
	}

	// Предполагаем структуру JSON: {"rounds": [{"themes": [...]}]}
	rounds, ok := packageJson["rounds"].([]interface{})
	if !ok || roundNum < 1 || roundNum > len(rounds) {
		return []Theme{}
	}

	round := rounds[roundNum-1].(map[string]interface{})
	themesData, ok := round["themes"].([]interface{})
	if !ok {
		return []Theme{}
	}

	themes := make([]Theme, 0, len(themesData))
	for _, themeData := range themesData {
		themeMap := themeData.(map[string]interface{})
		theme := Theme{
			Name:      getString(themeMap, "name"),
			Questions: []Question{},
		}

		questionsData, ok := themeMap["questions"].([]interface{})
		if ok {
			for _, qData := range questionsData {
				qMap := qData.(map[string]interface{})
				theme.Questions = append(theme.Questions, Question{
					ID:    int(getFloat(qMap, "id")),
					Price: int(getFloat(qMap, "price")),
					Theme: theme.Name,
					Text:  getString(qMap, "text"),
				})
			}
		}

		themes = append(themes, theme)
	}

	return themes
}

func getQuestionsForTheme(theme Theme) []Question {
	return theme.Questions
}

func checkAnswer(idQuest int, answer string) (bool, string) {
	gameState.mu.RLock()
	packageJson := gameState.packageJson
	gameState.mu.RUnlock()

	// Ищем вопрос в пакете
	rounds, ok := packageJson["rounds"].([]interface{})
	if !ok {
		return false, "Ошибка в структуре пакета"
	}

	for _, roundData := range rounds {
		round := roundData.(map[string]interface{})
		themesData, ok := round["themes"].([]interface{})
		if !ok {
			continue
		}

		for _, themeData := range themesData {
			themeMap := themeData.(map[string]interface{})
			questionsData, ok := themeMap["questions"].([]interface{})
			if !ok {
				continue
			}

			for _, qData := range questionsData {
				qMap := qData.(map[string]interface{})
				if int(getFloat(qMap, "id")) == idQuest {
					correctAnswer := getString(qMap, "answer")
					comment := getString(qMap, "comment")
					
					// Простая проверка (можно улучшить)
					result := strings.EqualFold(strings.TrimSpace(answer), strings.TrimSpace(correctAnswer))
					return result, comment
				}
			}
		}
	}

	return false, "Вопрос не найден"
}

func getPointsForQuestion(idQuest int) int {
	gameState.mu.RLock()
	packageJson := gameState.packageJson
	gameState.mu.RUnlock()

	rounds, ok := packageJson["rounds"].([]interface{})
	if !ok {
		return 0
	}

	for _, roundData := range rounds {
		round := roundData.(map[string]interface{})
		themesData, ok := round["themes"].([]interface{})
		if !ok {
			continue
		}

		for _, themeData := range themesData {
			themeMap := themeData.(map[string]interface{})
			questionsData, ok := themeMap["questions"].([]interface{})
			if !ok {
				continue
			}

			for _, qData := range questionsData {
				qMap := qData.(map[string]interface{})
				if int(getFloat(qMap, "id")) == idQuest {
					return int(getFloat(qMap, "price"))
				}
			}
		}
	}

	return 0
}

type NPCAnswer struct {
	HaveAnswer bool
	Answer     string
}

func generateAnswer(npc *Player) NPCAnswer {
	// Простая логика: случайно решает ответить или нет
	// В реальной игре здесь может быть более сложная логика
	if time.Now().UnixNano()%3 == 0 {
		// Пытается ответить (простой случайный ответ)
		gameState.mu.RLock()
		packageJson := gameState.packageJson
		selectedQuestionId := gameStateInternal.selectedQuestionId
		gameState.mu.RUnlock()

		// Пытаемся найти правильный ответ для текущего вопроса
		rounds, ok := packageJson["rounds"].([]interface{})
		if ok {
			for _, roundData := range rounds {
				round := roundData.(map[string]interface{})
				themesData, ok := round["themes"].([]interface{})
				if !ok {
					continue
				}

				for _, themeData := range themesData {
					themeMap := themeData.(map[string]interface{})
					questionsData, ok := themeMap["questions"].([]interface{})
					if !ok {
						continue
					}

					for _, qData := range questionsData {
						qMap := qData.(map[string]interface{})
						if int(getFloat(qMap, "id")) == selectedQuestionId {
							// Случайно решает ответить правильно или неправильно
							if time.Now().UnixNano()%2 == 0 {
								return NPCAnswer{
									HaveAnswer: true,
									Answer:     getString(qMap, "answer"),
								}
							} else {
								return NPCAnswer{
									HaveAnswer: true,
									Answer:     "Не знаю",
								}
							}
						}
					}
				}
			}
		}

		return NPCAnswer{
			HaveAnswer: true,
			Answer:     "Не знаю",
		}
	}

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

	return currentRound < len(rounds)
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
		}
	}
	return 0
}
