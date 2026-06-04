package main

import (
	"io"

	"go.kenn.io/roborev/internal/agenthook"
)

func runAgentHookDaemon(addr string, stderr io.Writer) error {
	return agenthook.RunDaemon(addr, stderr)
}
