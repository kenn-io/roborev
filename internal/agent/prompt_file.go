package agent

import (
	"fmt"
	"os"
)

// createPromptFile writes prompts outside argv and returns their cleanup.
func createPromptFile(name, prompt string) (string, func(), error) {
	f, err := os.CreateTemp("", "roborev-"+name+"-prompt-*.md")
	if err != nil {
		return "", nil, fmt.Errorf("create temp prompt file: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("write prompt to temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close temp prompt file: %w", err)
	}
	return f.Name(), cleanup, nil
}
