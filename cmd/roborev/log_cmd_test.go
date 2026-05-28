package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/streamfmt"
)

func TestLogCleanCmd_NegativeDays(t *testing.T) {
	cmd := logCleanCmd()
	cmd.SetArgs([]string{"--days", "-1"})
	err := cmd.Execute()
	require.Error(t, err, "expected error for negative --days")
}

func TestLogCleanCmd_OverflowDays(t *testing.T) {
	cmd := logCleanCmd()
	cmd.SetArgs([]string{"--days", "999999"})
	err := cmd.Execute()
	require.Error(t, err, "expected error for oversized --days")
}

func TestIsBrokenPipe(t *testing.T) {
	assert := assert.New(t)
	assert.False(isBrokenPipe(nil), "nil should not be broken pipe")
	assert.False(isBrokenPipe(fmt.Errorf("other error")), "non-EPIPE error should not be broken pipe")
	assert.True(isBrokenPipe(syscall.EPIPE), "bare EPIPE should be broken pipe")
	assert.True(isBrokenPipe(fmt.Errorf("write: %w", syscall.EPIPE)), "wrapped EPIPE should be broken pipe")
}

func TestLogCmd_InvalidJobID(t *testing.T) {
	assert := assert.New(t)

	cmd := logCmd()
	cmd.SetArgs([]string{"abc"})
	cmd.SilenceUsage = true
	err := cmd.Execute()
	require.Error(t, err, "expected error for non-numeric job ID")
	assert.Contains(err.Error(), "invalid job ID")
}

func TestLogCmd_MissingLogFile(t *testing.T) {
	assert := assert.New(t)

	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	cmd := logCmd()
	cmd.SetArgs([]string{"99999"})
	cmd.SilenceUsage = true
	err := cmd.Execute()
	require.Error(t, err, "expected error for missing log file")
	assert.Contains(err.Error(), "no log for job")
}

func TestLogCmd_PathFlag(t *testing.T) {
	assert := assert.New(t)

	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)

	var buf bytes.Buffer
	cmd := logCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--path", "42"})
	cmd.SilenceUsage = true
	// --path succeeds even if log file doesn't exist.
	err := cmd.Execute()
	require.NoError(t, err, "--path should succeed")

	out := strings.TrimSpace(buf.String())
	want := filepath.Join(dir, "logs", "jobs", "42.log")
	assert.Equal(want, out)
}

func TestLogCmd_RawFlag(t *testing.T) {
	assert := assert.New(t)

	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)

	// Create a log file at the expected path.
	logDir := filepath.Join(dir, "logs", "jobs")
	os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(logDir, "42.log")
	rawContent := `{"type":"assistant"}` + "\n"
	os.WriteFile(logPath, []byte(rawContent), 0o644)

	var buf bytes.Buffer
	cmd := logCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--raw", "42"})
	cmd.SilenceUsage = true
	err := cmd.Execute()
	require.NoError(t, err, "--raw should succeed")
	assert.Equal(rawContent, buf.String())
}

func TestLooksLikeJSON(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{`{"type":"assistant"}`, true},
		{`  {"type":"tool"}`, true},
		{`not json`, false},
		{``, false},
		{`[1,2,3]`, false},
		{`{invalid}`, false},
		// Valid JSON but no "type" field — should NOT match
		{`{"foo":"bar"}`, false},
		// Empty type — should NOT match
		{`{"type":""}`, false},
	}
	for _, tt := range tests {
		assert := assert.New(t)
		got := streamfmt.LooksLikeJSON(tt.input)
		assert.Equal(tt.want, got, "streamfmt.LooksLikeJSON(%q)", tt.input)
	}
}
