package main

import (
    "flag"
    "fmt"
    "os"
    "github.com/goldenpineappleofthesun/siziph"
)

func main() {
    // CLI flags
    input := flag.String("in", "", "Input .siq file path")
    output := flag.String("out", "", "Output folder path")

    flag.Parse()

    // Validate input parameters
    if *input == "" || *output == "" {
        fmt.Println("Usage:")
        fmt.Println("  siqcli -in file.siq -out folder")
        fmt.Println()
        flag.PrintDefaults()
        os.Exit(1)
    }

    // Run extractor
    err := siziph.Extract(*input, *output)
    if err != nil {
        fmt.Println("Failed:", err)
        os.Exit(1)
    }

    fmt.Println("OK!")
}
