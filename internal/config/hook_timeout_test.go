package config

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func platformHookDefault() time.Duration {
	if runtime.GOOS == "windows" {
		return DefaultHookTimeoutWindows
	}
	return DefaultHookTimeout
}

func TestDefaultHookTimeoutForOS(t *testing.T) {
	want := DefaultHookTimeout
	if runtime.GOOS == "windows" {
		want = DefaultHookTimeoutWindows
	}
	assert.Equal(t, want, DefaultHookTimeoutForOS())
}

func TestResolveHookTimeoutDefault(t *testing.T) {
	// No repo config and no global config: platform default.
	dir := t.TempDir()
	assert.Equal(t, platformHookDefault(), ResolveHookTimeout(dir, nil))
	assert.Equal(t, platformHookDefault(), ResolveHookTimeout(dir, &Config{}))
}

func TestResolveHookTimeoutGlobal(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{HookTimeoutSeconds: 45}
	assert.Equal(t, 45*time.Second, ResolveHookTimeout(dir, cfg))
}

func TestResolveHookTimeoutRepoOverridesGlobal(t *testing.T) {
	dir := newTempRepo(t, `hook_timeout_seconds = 90`)
	cfg := &Config{HookTimeoutSeconds: 45}
	assert.Equal(t, 90*time.Second, ResolveHookTimeout(dir, cfg))
}

func TestResolveHookTimeoutNonPositiveFallsBack(t *testing.T) {
	assert := assert.New(t)

	// Zero/negative global with no repo config falls back to platform default.
	dir := t.TempDir()
	for _, bad := range []int{0, -5} {
		assert.Equal(platformHookDefault(),
			ResolveHookTimeout(dir, &Config{HookTimeoutSeconds: bad}),
			"global %d should fall back to platform default", bad)
	}

	// Non-positive repo value falls back to a valid global value.
	repoDir := newTempRepo(t, `hook_timeout_seconds = -1`)
	assert.Equal(30*time.Second,
		ResolveHookTimeout(repoDir, &Config{HookTimeoutSeconds: 30}))
}

// TestResolveHookTimeoutWorktreeStaysFilesystemOnly verifies the hook path
// never spawns git: a linked worktree without its own .roborev.toml must NOT
// inherit the main checkout's hook_timeout_seconds via LoadRepoConfig's git-backed
// worktree fallback. It falls back to the global value instead.
func TestResolveHookTimeoutWorktreeStaysFilesystemOnly(t *testing.T) {
	main := t.TempDir()
	execGit(t, main, "init")
	execGit(t, main, "config", "user.email", "t@example.com")
	execGit(t, main, "config", "user.name", "t")
	writeTestFile(t, main, "base.txt", "base\n")
	execGit(t, main, "add", ".")
	execGit(t, main, "commit", "-m", "init")

	// Main checkout carries a per-repo hook_timeout_seconds, gitignored so the
	// git fallback in LoadRepoConfig would otherwise inherit it into a worktree.
	writeTestFile(t, main, ".gitignore", ".roborev.toml\n")
	writeRepoConfigStr(t, main, `hook_timeout_seconds = 90`)

	wt := filepath.Join(t.TempDir(), "wt")
	execGit(t, main, "worktree", "add", wt, "HEAD")

	// Sanity: the git-backed loader DOES inherit the main value -- proving the
	// fallback path exists and that ResolveHookTimeout must avoid it.
	inherited, err := LoadRepoConfig(wt)
	require.NoError(t, err)
	require.NotNil(t, inherited)
	require.Equal(t, 90, inherited.HookTimeoutSeconds)

	// ResolveHookTimeout stays filesystem-only, so it ignores the main config
	// and uses the global value.
	assert.Equal(t, 12*time.Second,
		ResolveHookTimeout(wt, &Config{HookTimeoutSeconds: 12}))
}
