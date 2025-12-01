# NPC Characters

Эта папка содержит предопределенных NPC персонажей для игры.

## Структура

- `characters.json` - JSON файл с описанием всех доступных NPC персонажей
- `*.jpeg`, `*.jpg`, `*.png` - файлы с фотографиями персонажей

## Формат JSON

```json
[
  {
    "name": "da vinci",
    "photo": "da_vinci.jpeg",
    "showman_prompt": "you are very smart but a grouch",
    "player_prompt": "you are leonardo and you know a lot if it not a knowledge from the future"
  }
]
```

## Поля

- `name` - имя персонажа (используется для поиска при `/joinnpc`)
- `photo` - имя файла с фотографией (должен находиться в этой же папке)
- `showman_prompt` - промпт для ведущего при взаимодействии с этим персонажем
- `player_prompt` - промпт для персонажа при генерации ответов

## Добавление новых персонажей

1. Добавьте фотографию персонажа в эту папку
2. Добавьте запись в `characters.json` с указанием имени файла фото
3. Перезапустите сервер



