package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDaemonRunLogsBeforeConfigFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	cmd := daemonRunCmd()
	cmd.SetArgs([]string{"--config", filepath.Join(dir, "missing.toml")})
	require.Error(t, cmd.Execute())
	data, err := os.ReadFile(filepath.Join(dir, "daemon.log"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "Starting roborev daemon")
	assert.Equal(t, 1, strings.Count(string(data), "Starting roborev daemon"))
}

func TestDaemonLoggingRotatesAndCloses(t *testing.T) {
	for _, mode := range []string{"foreground", "detached"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			var terminal bytes.Buffer
			var stderr io.Writer = &terminal
			if mode == "detached" {
				t.Setenv("ROBOREV_DATA_DIR", dir)
				_, capture, closeCapture, err := openDetachedDaemonLogs()
				require.NoError(t, err)
				t.Cleanup(closeCapture)
				stderr = capture
			}
			oldWriter, oldFlags := log.Writer(), log.Flags()
			closeLogs, err := setupDaemonLogging(dir, stderr, 256)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, closeLogs()) })
			for _, message := range []string{"first", "second", "third"} {
				log.Print(message + strings.Repeat("x", 160))
			}
			active, err := os.ReadFile(filepath.Join(dir, "daemon.log"))
			require.NoError(t, err)
			previous, err := os.ReadFile(filepath.Join(dir, "daemon.log.1"))
			require.NoError(t, err)
			assert.Contains(t, string(active), "third")
			assert.NotContains(t, string(active), "second")
			assert.Contains(t, string(previous), "second")
			assert.NotContains(t, string(previous), "first")
			assert.LessOrEqual(t, len(active), 256)
			assert.LessOrEqual(t, len(previous), 256)
			_, err = time.ParseInLocation("2006/01/02 15:04:05", string(active[:19]), time.Local)
			require.NoError(t, err)
			assert.Equal(t, 1, strings.Count(string(active), "third"))
			if mode == "detached" {
				output, err := os.ReadFile(filepath.Join(dir, "logs", "daemon.stderr.log"))
				require.NoError(t, err)
				assert.Equal(t, 1, strings.Count(string(output), "third"))
			} else {
				assert.Equal(t, 1, strings.Count(terminal.String(), "third"))
			}
			writer := log.Writer()
			require.NoError(t, closeLogs())
			assert.Equal(t, oldWriter, log.Writer())
			assert.Equal(t, oldFlags, log.Flags())
			_, err = writer.Write([]byte("after close"))
			require.ErrorIs(t, err, os.ErrClosed)
		})
	}
}

func TestDaemonLoggingBoundsOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	closeLogs, err := setupDaemonLogging(dir, &stderr, 256)
	require.NoError(t, err)
	defer func() { require.NoError(t, closeLogs()) }()
	log.Print("large " + strings.Repeat("x", 300))
	log.Print("next")
	previous, err := os.ReadFile(filepath.Join(dir, "daemon.log.1"))
	require.NoError(t, err)
	assert.LessOrEqual(t, len(previous), 256)
	assert.Contains(t, string(previous), "large")
	assert.True(t, bytes.HasSuffix(previous, []byte("[truncated]\n")))
	assert.Contains(t, stderr.String(), strings.Repeat("x", 300))
	active, err := os.ReadFile(filepath.Join(dir, "daemon.log"))
	require.NoError(t, err)
	assert.Contains(t, string(active), "next")
}

func TestDaemonLoggingBoundsExistingFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"daemon.log", "daemon.log.1"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("x", 300)), 0o600))
	}
	closeLogs, err := setupDaemonLogging(dir, io.Discard, 256)
	require.NoError(t, err)
	defer func() { require.NoError(t, closeLogs()) }()
	log.Print("fresh startup")
	for _, name := range []string{"daemon.log", "daemon.log.1"} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.LessOrEqual(t, info.Size(), int64(256))
	}
}
