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
	executable, _, _ := cache.Load(command, command)
	cache.Store(command, command, executable, true)

	_, supported, ok := cache.Load(command, command)
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
	executable, _, _ := cache.Load(link, link)
	cache.Store(link, link, executable, false)
	// Package managers such as npm upgrade the file the stable name links to.
	writeHelpCommand(t, target, "--stream --output-format", installed.Add(time.Minute))

	_, _, ok := cache.Load(link, link)
	assert.False(t, ok)
}

func TestCapabilityCacheDropsResultWhenUpgradedDuringProbe(t *testing.T) {
	skipIfWindows(t)

	dir := t.TempDir()
	command := filepath.Join(dir, "copilot")
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	script := "#!/bin/sh\ntouch '" + started + "'\nwhile [ ! -f '" + release + "' ]; do sleep 0.01; done\nprintf '%s\\n' '--stream'\n"
	require.NoError(t, os.WriteFile(command, []byte(script), 0o755))
	installed := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(command, installed, installed))

	type probeResult struct {
		supported bool
		err       error
	}
	done := make(chan probeResult, 1)
	go func() {
		supported, err := copilotSupportsJSONOutput(context.Background(), command)
		done <- probeResult{supported, err}
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(started)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)

	// Upgrade by renaming a new file into place, so the running probe keeps
	// executing the old script.
	upgraded := filepath.Join(dir, "copilot.new")
	writeHelpCommand(t, upgraded, "--stream --output-format", installed.Add(time.Minute))
	require.NoError(t, os.Rename(upgraded, command))
	require.NoError(t, os.WriteFile(release, nil, 0o644))

	result := <-done
	require.NoError(t, result.err)
	assert.False(t, result.supported, "the probe ran the executable from before the upgrade")

	supported, err := copilotSupportsJSONOutput(context.Background(), command)
	require.NoError(t, err)
	assert.True(t, supported, "the old executable's answer must not be cached for the upgraded one")
}
