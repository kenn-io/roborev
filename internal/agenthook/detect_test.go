package agenthook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstalledMissingFile(t *testing.T) {
	ok, err := Installed(filepath.Join(t.TempDir(), "nope.json"))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestInstalledDetectsRoborevHook(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"roborev agent-hook run"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	ok, err := Installed(path)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestInstalledIgnoresUnrelatedHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo hi"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	ok, err := Installed(path)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestInstalledInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
	_, err := Installed(path)
	assert.Error(t, err)
}
