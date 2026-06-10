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

func TestWindowsV056DaemonMigrationStart(t *testing.T) {
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

	startOut := runMigrationCmd(t, ".", append(os.Environ(), "ROBOREV_DATA_DIR="+dataDir),
		currentBin, "--verbose", "daemon", "start")
	assert.Contains(t, startOut, "Daemon started")

	select {
	case <-legacyDone:
		// The upgraded CLI should have stopped the legacy daemon before starting
		// the replacement daemon.
	case <-time.After(10 * time.Second):
		require.Fail(t, "v0.56 daemon still running after upgraded daemon start",
			"Output:\n%s", legacyOutput.String())
	}

	statusOut := runMigrationCmd(t, ".", append(os.Environ(), "ROBOREV_DATA_DIR="+dataDir),
		currentBin, "status")
	assert.Contains(t, statusOut, "Daemon: running")
	assert.NotContains(t, statusOut, "Daemon: not running")

	legacyMatches, err := filepath.Glob(filepath.Join(dataDir, "daemon.*.json"))
	require.NoError(t, err)
	assert.Empty(t, legacyMatches, "legacy root runtime files must be cleaned up")

	currentMatches, err := filepath.Glob(filepath.Join(dataDir, "runtime", "daemon.*.json"))
	require.NoError(t, err)
	assert.NotEmpty(t, currentMatches, "replacement daemon must publish a kit runtime record")
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
