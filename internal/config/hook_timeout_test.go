package config

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
	cfg := &Config{HookTimeout: "45s"}
	assert.Equal(t, 45*time.Second, ResolveHookTimeout(dir, cfg))
}

func TestResolveHookTimeoutRepoOverridesGlobal(t *testing.T) {
	dir := newTempRepo(t, `hook_timeout = "90s"`)
	cfg := &Config{HookTimeout: "45s"}
	assert.Equal(t, 90*time.Second, ResolveHookTimeout(dir, cfg))
}

func TestResolveHookTimeoutInvalidFallsBack(t *testing.T) {
	assert := assert.New(t)

	// Invalid global with no repo config falls back to platform default.
	dir := t.TempDir()
	for _, bad := range []string{"", "0", "-5s", "soon", "12"} {
		assert.Equal(platformHookDefault(),
			ResolveHookTimeout(dir, &Config{HookTimeout: bad}),
			"global %q should fall back to platform default", bad)
	}

	// Invalid repo value falls back to a valid global value.
	repoDir := newTempRepo(t, `hook_timeout = "nope"`)
	assert.Equal(30*time.Second,
		ResolveHookTimeout(repoDir, &Config{HookTimeout: "30s"}))
}
