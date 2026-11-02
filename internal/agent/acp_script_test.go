package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func acpScriptsDir(t *testing.T) string {
	t.Helper()
	// go test runs from this package's directory.
	dir, err := filepath.Abs(filepath.Join("..", "..", "scripts"))
	require.NoError(t, err, "failed to resolve scripts directory")
	return dir
}

func writeACPTestCommand(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	err := os.WriteFile(path, []byte(script), 0o755)
	require.NoError(t, err, "failed to write command %s", name)
}

func runACPWrapperCommand(t *testing.T, commandPath string, env map[string]string, args ...string) (string, string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), commandPath, args...)
	pathValue := os.Getenv("PATH")
	if customPath, ok := env["PATH"]; ok {
		pathValue = customPath
	}

	cmd.Env = []string{
		"PATH=" + pathValue,
	}
	if home := os.Getenv("HOME"); home != "" {
		cmd.Env = append(cmd.Env, "HOME="+home)
	}
	if tmpDir := os.Getenv("TMPDIR"); tmpDir != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+tmpDir)
	}

	for k, v := range env {
		if k == "PATH" {
			continue
		}
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func TestACPWrapperScripts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ACP wrapper scripts are POSIX shell scripts")
	}

	scriptsDir := acpScriptsDir(t)
	basePath := os.Getenv("PATH")

	t.Run("ROBOREV_ACP_ADAPTER_COMMAND override takes precedence", func(t *testing.T) {
		fakeBinDir := t.TempDir()
		overrideCommand := filepath.Join(fakeBinDir, "override-acp")
		writeACPTestCommand(t, fakeBinDir, "override-acp", "#!/bin/sh\necho ACP_OVERRIDE_OK\n")

		stdout, _, err := runACPWrapperCommand(t, filepath.Join(scriptsDir, "acp-agent"), map[string]string{
			"ROBOREV_ACP_ADAPTER_COMMAND": overrideCommand,
			"ROBOREV_ACP_ADAPTER":         "gemini",
			"PATH":                        fakeBinDir + string(os.PathListSeparator) + basePath,
		})
		require.NoError(t, err, "expected override command to succeed")
		assert.Contains(t, stdout, "ACP_OVERRIDE_OK", "expected override marker in stdout")
	})

	t.Run("Adapter dispatch invokes claude wrapper binary from PATH", func(t *testing.T) {
		fakeBinDir := t.TempDir()
		writeACPTestCommand(t, fakeBinDir, "claude-agent-acp", "#!/bin/sh\necho ACP_CLAUDE_OK\n")

		stdout, _, err := runACPWrapperCommand(t, filepath.Join(scriptsDir, "acp-agent"), map[string]string{
			"ROBOREV_ACP_ADAPTER": "claude",
			"PATH":                fakeBinDir + string(os.PathListSeparator) + basePath,
		})
		require.NoError(t, err, "expected claude adapter dispatch to succeed")
		assert.Contains(t, stdout, "ACP_CLAUDE_OK", "expected claude marker in stdout")
	})

	t.Run("Adapter dispatch invokes codex wrapper binary from PATH", func(t *testing.T) {
		fakeBinDir := t.TempDir()
		writeACPTestCommand(t, fakeBinDir, "codex-acp", "#!/bin/sh\necho ACP_CODEX_OK\n")

		stdout, _, err := runACPWrapperCommand(t, filepath.Join(scriptsDir, "acp-agent"), map[string]string{
			"ROBOREV_ACP_ADAPTER": "codex",
			"PATH":                fakeBinDir + string(os.PathListSeparator) + basePath,
		})
		require.NoError(t, err, "expected codex adapter dispatch to succeed")
		assert.Contains(t, stdout, "ACP_CODEX_OK", "expected codex marker in stdout")
	})

	t.Run("Default adapter dispatch uses codex when adapter env is unset", func(t *testing.T) {
		fakeBinDir := t.TempDir()
		writeACPTestCommand(t, fakeBinDir, "codex-acp", "#!/bin/sh\necho ACP_DEFAULT_CODEX_OK\n")

		stdout, _, err := runACPWrapperCommand(t, filepath.Join(scriptsDir, "acp-agent"), map[string]string{
			"PATH": fakeBinDir + string(os.PathListSeparator) + basePath,
		})
		require.NoError(t, err, "expected default adapter dispatch to succeed")
		assert.Contains(t, stdout, "ACP_DEFAULT_CODEX_OK", "expected default codex marker in stdout")
	})

	t.Run("Gemini wrapper appends --experimental-acp flag", func(t *testing.T) {
		fakeBinDir := t.TempDir()
		writeACPTestCommand(t, fakeBinDir, "gemini", "#!/bin/sh\necho GEMINI_ARGS:$@\n")

		stdout, _, err := runACPWrapperCommand(t, filepath.Join(scriptsDir, "acp-agent"), map[string]string{
			"ROBOREV_ACP_ADAPTER": "gemini",
			"PATH":                fakeBinDir + string(os.PathListSeparator) + basePath,
		}, "--test-arg")
		require.NoError(t, err, "expected gemini adapter dispatch to succeed")
		assert.Contains(t, stdout, "GEMINI_ARGS:--experimental-acp --test-arg", "expected gemini experimental flag in args")
	})

	t.Run("Unsupported adapter returns error", func(t *testing.T) {
		stdout, stderr, err := runACPWrapperCommand(t, filepath.Join(scriptsDir, "acp-agent"), map[string]string{
			"ROBOREV_ACP_ADAPTER": "unsupported",
			"PATH":                basePath,
		})
		require.Error(t, err, "expected unsupported adapter to fail")
		assert.NotEmpty(t, stdout+stderr)
		assert.Contains(t, stderr, "unsupported ACP adapter", "expected unsupported adapter error message")
	})
}
