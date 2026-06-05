package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/roborev/internal/daemon"
)

func main() {
	output := flag.String("o", "", "output file")
	downgrade := flag.Bool("openapi-3.0", false, "emit OpenAPI 3.0 for code generators")
	format := flag.String("format", "json", "OpenAPI format to write: json or yaml")
	flag.Parse()
	if *output == "" {
		fmt.Fprintln(os.Stderr, "missing -o")
		os.Exit(2)
	}

	spec, err := openAPISpec(*downgrade, *format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate OpenAPI spec: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", filepath.Dir(*output), err)
		os.Exit(1)
	}
	if err := os.WriteFile(*output, ensureTrailingNewline(spec), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write OpenAPI spec: %v\n", err)
		os.Exit(1)
	}
}

func openAPISpec(downgrade bool, format string) ([]byte, error) {
	switch format {
	case "json":
		if downgrade {
			return daemon.OpenAPISpec30()
		}
		return daemon.OpenAPISpec()
	case "yaml", "yml":
		if downgrade {
			return daemon.OpenAPISpec30YAML()
		}
		return daemon.OpenAPISpecYAML()
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

func ensureTrailingNewline(data []byte) []byte {
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return data
	}
	return append(data, '\n')
}
