package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/roborev/internal/testutil/webfixture"
)

func main() {
	out := flag.String("out", "", "path to the SQLite fixture database")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "seed-web: -out is required")
		os.Exit(2)
	}

	path, err := filepath.Abs(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-web: resolve output path: %v\n", err)
		os.Exit(1)
	}
	if err := webfixture.Seed(path); err != nil {
		fmt.Fprintf(os.Stderr, "seed-web: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(path)
}
