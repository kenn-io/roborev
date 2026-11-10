package main

import (
	"context"
	"fmt"
	"os"

	"go.kenn.io/roborev/internal/testenv/gitfixture"
)

func main() {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	metadata, err := gitfixture.ReadMetadata(executable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read fixture Git metadata: %v\n", err)
		os.Exit(1)
	}
	// This standalone helper owns its root context and has no caller context.
	os.Exit(gitfixture.Forward(context.Background(), metadata, os.Args[1:]))
}
