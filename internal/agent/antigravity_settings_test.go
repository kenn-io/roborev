package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureAntigravityReviewPermissionsCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	require.NoError(t, ensureAntigravityReviewPermissions(context.Background(), path))

	assertSettingsAllow(t, path, antigravityReviewAllowPermissions...)
}

func TestEnsureAntigravityReviewPermissionsHandlesNullRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte("null\n"), 0o644))

	require.NoError(t, ensureAntigravityReviewPermissions(context.Background(), path))
	assertSettingsAllow(t, path, antigravityReviewAllowPermissions...)
}

func TestEnsureAntigravityReviewPermissionsPreservesLargeNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "large": 9007199254740993,
  "permissions": {"allow": []}
}
`), 0o644))

	require.NoError(t, ensureAntigravityReviewPermissions(context.Background(), path))

	assert.Contains(t, string(readRaw(t, path)), `"large": 9007199254740993`)
}

func TestEnsureAntigravityReviewPermissionsPreservesSymlinkAndMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink and permission semantics differ on Windows")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	path := filepath.Join(dir, "settings.json")
	writeSettings(t, target, map[string]any{
		"permissions": map[string]any{"allow": []any{}},
	})
	require.NoError(t, os.Chmod(target, 0o600))
	require.NoError(t, os.Symlink(target, path))

	require.NoError(t, ensureAntigravityReviewPermissions(context.Background(), path))

	linkInfo, err := os.Lstat(path)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSymlink, linkInfo.Mode()&os.ModeSymlink)
	targetInfo, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), targetInfo.Mode().Perm())
	assertSettingsAllow(t, target, antigravityReviewAllowPermissions...)
}

func TestEnsureAntigravityReviewPermissionsWaitsForConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, map[string]any{
		"permissions": map[string]any{"allow": []any{}},
	})

	lock := flock.New(path+".lock", flock.SetPermissions(0o600))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() {
		_ = lock.Unlock()
		require.NoError(t, lock.Close())
	})

	done := make(chan error, 1)
	go func() {
		done <- ensureAntigravityReviewPermissions(context.Background(), path)
	}()

	completed := false
	select {
	case err := <-done:
		completed = true
		require.NoError(t, err)
	case <-time.After(100 * time.Millisecond):
	}
	require.False(t, completed)

	require.NoError(t, lock.Unlock())
	require.NoError(t, <-done)
	assertSettingsAllow(t, path, antigravityReviewAllowPermissions...)
}

func TestEnsureAntigravityReviewPermissionsMergesAllow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	writeSettings(t, path, map[string]any{
		"model": "gemini-3.1-pro-preview",
		"permissions": map[string]any{
			"allow": []any{"command(git)", "read_file(*)"},
			"deny":  []any{"command(rm -rf)"},
			"ask":   []any{"command(*)"},
		},
		"enableTerminalSandbox": true,
	})

	require.NoError(t, ensureAntigravityReviewPermissions(context.Background(), path))

	doc := readSettings(t, path)
	assert.Equal(t, "gemini-3.1-pro-preview", doc["model"])
	assert.Equal(t, true, doc["enableTerminalSandbox"])

	permissions := doc["permissions"].(map[string]any)
	assert.Equal(t, []any{"command(rm -rf)"}, permissions["deny"])
	assert.Equal(t, []any{"command(*)"}, permissions["ask"])

	allow := asStrings(t, permissions["allow"])
	assert.Equal(t, "command(git)", allow[0], "existing allow entries stay first")
	assert.Contains(t, allow, "read_file(*)")
	assert.Equal(t, 1, countStrings(allow, "read_file(*)"), "do not duplicate existing allows")
	for _, rule := range antigravityReviewAllowPermissions {
		assert.Contains(t, allow, rule)
	}
}

func TestEnsureAntigravityReviewPermissionsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, ensureAntigravityReviewPermissions(context.Background(), path))
	first := readRaw(t, path)

	require.NoError(t, ensureAntigravityReviewPermissions(context.Background(), path))
	second := readRaw(t, path)
	assert.Equal(t, first, second)
}

func TestEnsureAntigravityReviewPermissionsDoesNotClobberInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte("{not-json"), 0o644))

	err := ensureAntigravityReviewPermissions(context.Background(), path)
	require.Error(t, err)
	assert.Equal(t, "{not-json", string(readRaw(t, path)))
}

func TestEnsureAntigravityReviewPermissionsRejectsNonObjectPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, map[string]any{"permissions": "always-proceed"})

	err := ensureAntigravityReviewPermissions(context.Background(), path)
	require.Error(t, err)
	doc := readSettings(t, path)
	assert.Equal(t, "always-proceed", doc["permissions"])
}

func TestEnsureAntigravityReviewPermissionsEmptyPathIsNoop(t *testing.T) {
	require.NoError(t, ensureAntigravityReviewPermissions(context.Background(), ""))
}

func TestAntigravityReviewAllowPermissionsCoverProdFailures(t *testing.T) {
	assert.Contains(t, antigravityReviewAllowPermissions, "read_file(*)")
	for _, cmd := range []string{"pwd", "wc", "ls", "cat", "head", "tail", "stat", "file"} {
		assert.Contains(t, antigravityReviewAllowPermissions, "command("+cmd+")")
	}
}

func writeSettings(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o644))
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(readRaw(t, path), &doc))
	return doc
}

func readRaw(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

func assertSettingsAllow(t *testing.T, path string, want ...string) {
	t.Helper()
	doc := readSettings(t, path)
	permissions, ok := doc["permissions"].(map[string]any)
	require.True(t, ok)
	allow := asStrings(t, permissions["allow"])
	for _, rule := range want {
		assert.Contains(t, allow, rule)
	}
}

func asStrings(t *testing.T, raw any) []string {
	t.Helper()
	items, ok := raw.([]any)
	require.True(t, ok)
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		require.True(t, ok)
		out = append(out, s)
	}
	return out
}

func countStrings(items []string, want string) int {
	n := 0
	for _, item := range items {
		if item == want {
			n++
		}
	}
	return n
}
