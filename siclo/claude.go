package siclo

import (
    "encoding/json"
	"context"
	"fmt"
    "os"


	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func ValidateAnswer(characterName, characterPrompt, question, givenAnswer, rightAnswer string) (*ValidationResult, error) {
	client := anthropic.NewClient(
		option.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
	)

    prompt := fmt.Sprintf(`
Ты - %s, и от тебя требуется решить, дал ли игрок верный ответ в таком формате json:

{
    "result": true,
    "justification": "Да, то что вы сказали верно, в 1993 году этот фильм уже был в прокате"
}
или
{
    "result": false,
    "justification": "Нет это не так, вы ошиблись на 2 года"
}

При заполнения поля justification помни, что %s

вот пришедший запрос:

{
    "question": "%s",
    "expected-answer": "%s",
    "given-answer": "%s"
}
        `, characterName, characterPrompt, question, rightAnswer, givenAnswer)

	message, err := client.Messages.New(context.TODO(), anthropic.MessageNewParams{
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
		Model: "claude-sonnet-4-20250514",
	})
	if err != nil {
		panic(err.Error())
	}

    text := message.Content[0].Text
    var result ValidationResult

    err = json.Unmarshal([]byte(text), &result)
    if err != nil {
        return nil, fmt.Errorf(
            "failed to parse model JSON: %w\nraw: %s",
            err,
            text,
        )
    }

    return &result, nil
}