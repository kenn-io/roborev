//go:build integration && migration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsV056DaemonMigrationRefusesUnsafeReplacement(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only daemon migration repro")
	}

	oldBin := os.Getenv("ROBOREV_MIGRATION_OLD_BINARY")
	if oldBin == "" {
		t.Skip("set ROBOREV_MIGRATION_OLD_BINARY to a v0.56 Windows roborev.exe")
	}
	require.FileExists(t, oldBin)

	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	currentBin := filepath.Join(tmpDir, "roborev-current.exe")

	runMigrationCmd(t, ".", nil, "go", "build", "-tags", "kit_posthog_disabled", "-o", currentBin, ".")

	dbPath := filepath.Join(dataDir, "reviews.db")
	configPath := filepath.Join(dataDir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, nil, 0o644))

	legacyOutput := new(syncBuffer)
	legacyCmd := exec.Command(oldBin, "daemon", "run",
		"--db", dbPath,
		"--config", configPath,
		"--addr", "127.0.0.1:7373",
	)
	legacyCmd.Env = append(os.Environ(), "ROBOREV_DATA_DIR="+dataDir)
	legacyCmd.Stdout = legacyOutput
	legacyCmd.Stderr = legacyOutput
	require.NoError(t, legacyCmd.Start())

	legacyDone := make(chan error, 1)
	go func() { legacyDone <- legacyCmd.Wait() }()
	t.Cleanup(func() {
		if legacyCmd.ProcessState == nil || !legacyCmd.ProcessState.Exited() {
			_ = legacyCmd.Process.Kill()
			select {
			case <-legacyDone:
			case <-time.After(2 * time.Second):
			}
		}
		runMigrationCmd(t, ".", append(os.Environ(), "ROBOREV_DATA_DIR="+dataDir),
			currentBin, "daemon", "stop")
	})

	require.True(t, waitFor(t, 30*time.Second, func() bool {
		matches, err := filepath.Glob(filepath.Join(dataDir, "daemon.*.json"))
		return err == nil && len(matches) > 0
	}), "v0.56 daemon never published legacy runtime. Output:\n%s", legacyOutput.String())

	startCtx, cancelStart := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelStart()
	startCmd := exec.CommandContext(startCtx, currentBin, "--verbose", "daemon", "start")
	startCmd.Dir = "."
	startCmd.Env = append(os.Environ(), "ROBOREV_DATA_DIR="+dataDir)
	startOut, startErr := startCmd.CombinedOutput()
	require.Error(t, startErr)
	assert.Contains(t, string(startOut), "failed to start daemon: context deadline exceeded")

	legacyExited := false
	select {
	case <-legacyDone:
		legacyExited = true
	default:
	}
	assert.False(t, legacyExited, "replacement refusal must not stop the v0.56 daemon")

	legacyMatches, err := filepath.Glob(filepath.Join(dataDir, "daemon.*.json"))
	require.NoError(t, err)
	assert.NotEmpty(t, legacyMatches, "legacy runtime must remain for manual recovery")

	currentMatches, err := filepath.Glob(filepath.Join(dataDir, "runtime", "daemon.*.json"))
	require.NoError(t, err)
	assert.Empty(t, currentMatches, "replacement daemon must not start beside v0.56")
}

func runMigrationCmd(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s %s failed:\n%s", name, strings.Join(args, " "), out)
	return string(out)
}
