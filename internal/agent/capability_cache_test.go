package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeHelpCommand(t *testing.T, path, help string, modTime time.Time) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' '" + help + "'\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	require.NoError(t, os.Chtimes(path, modTime, modTime))
}

func TestCapabilityCacheReprobesUpgradedCommand(t *testing.T) {
	skipIfWindows(t)

	command := filepath.Join(t.TempDir(), "copilot")
	installed := time.Now().Add(-time.Hour)
	writeHelpCommand(t, command, "--stream", installed)

	supported, err := copilotSupportsJSONOutput(context.Background(), command)
	require.NoError(t, err)
	assert.False(t, supported)

	writeHelpCommand(t, command, "--stream --output-format", installed.Add(time.Minute))

	supported, err = copilotSupportsJSONOutput(context.Background(), command)
	require.NoError(t, err)
	assert.True(t, supported, "an upgraded CLI must be probed again instead of reusing the old answer")
}

func TestCapabilityCacheReusesUnchangedCommand(t *testing.T) {
	skipIfWindows(t)

	command := filepath.Join(t.TempDir(), "copilot")
	installed := time.Now().Add(-time.Hour)
	writeHelpCommand(t, command, "--output-format", installed)

	var cache capabilityCache
	cache.Store(command, command, true)

	supported, ok := cache.Load(command, command)
	assert.True(t, ok)
	assert.True(t, supported)
}

func TestCapabilityCacheFollowsSymlinkTarget(t *testing.T) {
	skipIfWindows(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "loader.js")
	link := filepath.Join(dir, "copilot")
	installed := time.Now().Add(-time.Hour)
	writeHelpCommand(t, target, "--stream", installed)
	require.NoError(t, os.Symlink(target, link))

	var cache capabilityCache
	cache.Store(link, link, false)
	// Package managers such as npm upgrade the file the stable name links to.
	writeHelpCommand(t, target, "--stream --output-format", installed.Add(time.Minute))

	_, ok := cache.Load(link, link)
	assert.False(t, ok)
}
