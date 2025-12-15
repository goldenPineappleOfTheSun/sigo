package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"siclo"
)

func main() {
	characterName := flag.String("character-name", "", "Character name")
	characterPrompt := flag.String("character-prompt", "", "Character behavior description")
	question := flag.String("question", "", "Question text")
	rightAnswer := flag.String("right-answer", "", "Correct answer")
	givenAnswer := flag.String("given-answer", "", "Player answer")
	jsonOutput := flag.Bool("json", false, "Output raw JSON")

	flag.Parse()

	// Basic validation
	if *characterName == "" ||
		*characterPrompt == "" ||
		*question == "" ||
		*rightAnswer == "" ||
		*givenAnswer == "" {
		fmt.Fprintln(os.Stderr, "Missing required flags")
		flag.Usage()
		os.Exit(1)
	}

	result, err := siclo.ValidateAnswer(
		*characterName,
		*characterPrompt,
		*question,
		*givenAnswer,
		*rightAnswer,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
		return
	}

	// Human-friendly output
	if result.Result {
		fmt.Println("✅ Correct")
	} else {
		fmt.Println("❌ Incorrect")
	}
	fmt.Println(result.Justification)
}
