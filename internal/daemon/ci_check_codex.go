package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ciCheckCodex struct {
	Command   string
	ReadPaths []string
}

func resolveCICheckCodex(ctx context.Context, dir, command string) (ciCheckCodex, error) {
	codex, err := resolveCICheckCommand(ctx, dir, command)
	if err != nil {
		return ciCheckCodex{}, fmt.Errorf("find checker codex: %w", err)
	}
	real, err := filepath.EvalSymlinks(codex)
	if err != nil {
		return ciCheckCodex{}, err
	}
	// The supported npm install keeps the native package beside @openai/codex.
	if filepath.Base(real) == "codex.js" && filepath.Base(filepath.Dir(real)) == "bin" && filepath.Base(filepath.Dir(filepath.Dir(real))) == "codex" {
		scope := filepath.Dir(filepath.Dir(filepath.Dir(real)))
		if filepath.Base(scope) != "@openai" {
			return ciCheckCodex{}, fmt.Errorf("unsupported checker Codex install: %s", real)
		}
		real = filepath.Join(scope, "codex-linux-x64", "vendor", "x86_64-unknown-linux-musl", "bin", "codex")
		codex = real
	}
	binary, err := os.Open(real)
	if err != nil {
		return ciCheckCodex{}, err
	}
	var magic [4]byte
	_, err = io.ReadFull(binary, magic[:])
	closeErr := binary.Close()
	if err != nil {
		return ciCheckCodex{}, err
	}
	if closeErr != nil {
		return ciCheckCodex{}, closeErr
	}
	if string(magic[:]) != "\x7fELF" {
		return ciCheckCodex{}, fmt.Errorf("unsupported checker Codex executable: %s", real)
	}
	paths := []string{filepath.Dir(real)}
	if _, err := os.Stat("/etc/codex"); err == nil {
		paths = append(paths, "/etc/codex")
	} else if !os.IsNotExist(err) {
		return ciCheckCodex{}, err
	}
	return ciCheckCodex{Command: codex, ReadPaths: paths}, nil
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
