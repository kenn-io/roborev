//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
)

func TestExtendDaemonPATHFindsAgentsInVersionManagerShims(t *testing.T) {
	home := t.TempDir()
	shims := filepath.Join(home, ".local", "share", "mise", "shims")
	require.NoError(t, os.MkdirAll(shims, 0o755))
	codex := filepath.Join(shims, "codex")
	require.NoError(t, os.WriteFile(codex, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("MISE_DATA_DIR", "")
	// A service manager's PATH: no user tool directories.
	t.Setenv("PATH", t.TempDir())
	require.False(t, agent.IsAvailable("codex"))

	extendDaemonPATH()

	assert.True(t, agent.IsAvailable("codex"))
	found, err := exec.LookPath("codex")
	require.NoError(t, err)
	assert.Equal(t, codex, found)
}
