package main

import (
	"fmt"
	"os"
)

func main() {
	const name = "planbill"
	args := os.Args[1:]
	if len(args) > 0 && !(len(args) == 1 && (args[0] == "--help" || args[0] == "-h")) {
		fmt.Fprintln(os.Stderr, name+": unknown arguments; use --help")
		os.Exit(2)
	}
	fmt.Printf("%s\n\nUsage: go run . [--help]\n\n订阅与用量账单管理。当前仅提供帮助信息。\n", name)
}
