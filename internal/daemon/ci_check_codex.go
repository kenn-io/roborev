package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// resolveCICheckCodex returns the Codex command and the directory the sandbox must read to run it.
func resolveCICheckCodex(ctx context.Context, dir, command string) (string, string, error) {
	codex, err := resolveCICheckCommand(ctx, dir, command)
	if err != nil {
		return "", "", fmt.Errorf("find checker codex: %w", err)
	}
	real, err := filepath.EvalSymlinks(codex)
	if err != nil {
		return "", "", err
	}
	// mise's npm backend uses a shell shim beside the linked @openai package.
	if filepath.Base(real) == "codex" && filepath.Base(filepath.Dir(real)) == ".bin" {
		real, err = filepath.EvalSymlinks(filepath.Join(filepath.Dir(real), "..", "@openai", "codex", "bin", "codex.js"))
		if err != nil {
			return "", "", err
		}
	}
	// The npm launcher needs Node, so run the native package installed beside @openai/codex.
	if filepath.Base(real) == "codex.js" && filepath.Base(filepath.Dir(real)) == "bin" && filepath.Base(filepath.Dir(filepath.Dir(real))) == "codex" {
		scope := filepath.Dir(filepath.Dir(filepath.Dir(real)))
		real, err = filepath.EvalSymlinks(filepath.Join(scope, "codex-linux-x64", "vendor", "x86_64-unknown-linux-musl", "bin", "codex"))
		if err != nil {
			return "", "", fmt.Errorf("find checker Codex native package: %w", err)
		}
		codex = real
	}
	return codex, filepath.Dir(real), nil
}

func resolveCICheckCommand(ctx context.Context, dir, name string) (string, error) {
	command, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	command, err = filepath.Abs(command)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(command)
	if err != nil {
		return "", err
	}
	// Resolve mise outside the checkout so repository config cannot select a tool.
	if filepath.Base(real) == "mise" {
		which := exec.CommandContext(ctx, real, "which", filepath.Base(name))
		which.Dir = dir
		path, err := which.Output()
		if err != nil {
			return "", err
		}
		return filepath.Abs(strings.TrimSpace(string(path)))
	}
	return command, nil
}
