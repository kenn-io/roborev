package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// configWatcherHarness encapsulates the watcher, broadcaster, temp paths, and event channel.
type configWatcherHarness struct {
	Watcher     *ConfigWatcher
	Broadcaster Broadcaster
	ConfigPath  string
	EventCh     <-chan Event
	dir         string
}

const (
	eventConfigReloaded = "config.reloaded"
	reloadTimeout       = 2 * time.Second
)

func newConfigWatcherHarness(t *testing.T, initialConfig string) *configWatcherHarness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	writeTestFile(t, path, initialConfig)

	cfg, err := config.LoadGlobalFrom(path)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to load config: %v", err)
	}

	bc := NewBroadcaster()
	_, ch := bc.Subscribe("")
	cw := NewConfigWatcher(path, cfg, bc, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := cw.Start(ctx); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to start watcher: %v", err)
	}
	t.Cleanup(cw.Stop)

	return &configWatcherHarness{
		Watcher:     cw,
		Broadcaster: bc,
		ConfigPath:  path,
		EventCh:     ch,
		dir:         dir,
	}
}

func setupUnstartedWatcher(t *testing.T, configPath string) *ConfigWatcher {
	t.Helper()
	cfg := &config.Config{DefaultAgent: "test"}
	broadcaster := NewBroadcaster()
	return NewConfigWatcher(configPath, cfg, broadcaster, nil)
}

func (h *configWatcherHarness) updateConfig(t *testing.T, content string) {
	t.Helper()
	writeTestFile(t, h.ConfigPath, content)
}

func (h *configWatcherHarness) updateConfigAndWait(t *testing.T, content string) {
	t.Helper()
	h.updateConfig(t, content)
	h.waitForReload(t)
}

func (h *configWatcherHarness) waitForReload(t *testing.T) {
	t.Helper()
	timeout := time.After(reloadTimeout)
	for {
		select {
		case event := <-h.EventCh:
			if event.Type == eventConfigReloaded {
				return
			}
		case <-timeout:
			require.Condition(t, func() bool {
				return false
			}, "Timeout waiting for config.reloaded event")
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to write %s: %v", filepath.Base(path), err)
	}
}

func requireNever(t *testing.T, condition func() bool, duration, interval time.Duration) {
	t.Helper()
	timeout := time.After(duration)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return
		case <-ticker.C:
			if condition() {
				require.Condition(t, func() bool {
					return false
				}, "condition evaluated to true within %v", duration)
			}
		}
	}
}

func TestStaticConfig(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent: "test-agent",
		MaxWorkers:   5,
	}

	sc := NewStaticConfig(cfg)

	// Should always return the same config
	if sc.Config() != cfg {
		assert.Condition(t, func() bool {
			return false
		}, "StaticConfig.Config() should return the same config object")
	}

	// Call multiple times to verify consistency
	for range 3 {
		if sc.Config().DefaultAgent != "test-agent" {
			assert.Condition(t, func() bool {
				return false
			}, "StaticConfig.Config().DefaultAgent = %q, want %q", sc.Config().DefaultAgent, "test-agent")
		}
	}
}

func TestNewConfigWatcher(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent: "initial-agent",
		MaxWorkers:   3,
	}
	broadcaster := NewBroadcaster()

	cw := NewConfigWatcher("/path/to/config.toml", cfg, broadcaster, nil)

	if cw.Config() != cfg {
		assert.Condition(t, func() bool {
			return false
		}, "NewConfigWatcher should store the initial config")
	}

	if cw.configPath != "/path/to/config.toml" {
		assert.Condition(t, func() bool {
			return false
		}, "configPath = %q, want %q", cw.configPath, "/path/to/config.toml")
	}

	// LastReloadedAt should be zero initially
	if !cw.LastReloadedAt().IsZero() {
		assert.Condition(t, func() bool {
			return false
		}, "LastReloadedAt should be zero initially")
	}
}

func TestConfigWatcher_NoConfigPath(t *testing.T) {
	cw := setupUnstartedWatcher(t, "")

	ctx := t.Context()

	err := cw.Start(ctx)
	if err != nil {
		assert.Condition(t, func() bool {
			return false
		}, "Start with empty configPath should not error, got: %v", err)
	}

	// Stop should not panic
	cw.Stop()
}

func TestConfigWatcher_Reloads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		initialConfig string
		updateConfig  string
		validate      func(*testing.T, *config.Config)
	}{
		{
			name: "Update Agent and Workers",
			initialConfig: `
default_agent = "codex"
max_workers = 2
`,
			updateConfig: `
default_agent = "gemini"
max_workers = 4
`,
			validate: func(t *testing.T, c *config.Config) {
				if c.DefaultAgent != "gemini" {
					assert.Condition(t, func() bool {
						return false
					}, "got agent %q, want %q", c.DefaultAgent, "gemini")
				}
				if c.MaxWorkers != 4 {
					assert.Condition(t, func() bool {
						return false
					}, "got max workers %d, want 4", c.MaxWorkers)
				}
			},
		},
		{
			name: "Update Hot Reloadable Settings",
			initialConfig: `
default_agent = "codex"
review_context_count = 3
job_timeout_minutes = 10
`,
			updateConfig: `
default_agent = "claude-code"
review_context_count = 7
job_timeout_minutes = 30
`,
			validate: func(t *testing.T, c *config.Config) {
				if c.ReviewContextCount != 7 {
					assert.Condition(t, func() bool {
						return false
					}, "got context count %d, want 7", c.ReviewContextCount)
				}
				if c.JobTimeoutMinutes != 30 {
					assert.Condition(t, func() bool {
						return false
					}, "got timeout %d, want 30", c.JobTimeoutMinutes)
				}
				if c.DefaultAgent != "claude-code" {
					assert.Condition(t, func() bool {
						return false
					}, "got agent %q, want %q", c.DefaultAgent, "claude-code")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newConfigWatcherHarness(t, tt.initialConfig)

			// Verify initial state
			if !h.Watcher.LastReloadedAt().IsZero() {
				assert.Condition(t, func() bool {
					return false
				}, "LastReloadedAt should be zero initially, got %v", h.Watcher.LastReloadedAt())
			}

			h.updateConfigAndWait(t, tt.updateConfig)

			tt.validate(t, h.Watcher.Config())

			// Verify LastReloadedAt was updated and is recent
			if h.Watcher.LastReloadedAt().IsZero() {
				assert.Condition(t, func() bool {
					return false
				}, "LastReloadedAt should not be zero after reload")
			}
			if time.Since(h.Watcher.LastReloadedAt()) > 5*time.Second {
				assert.Condition(t, func() bool {
					return false
				}, "LastReloadedAt should be recent (within 5s), got %v", h.Watcher.LastReloadedAt())
			}
		})
	}
}

func TestConfigWatcher_InvalidConfigDoesNotCrash(t *testing.T) {
	t.Parallel()
	h := newConfigWatcherHarness(t, `default_agent = "codex"`)

	// Write invalid TOML - this should not crash the watcher
	h.updateConfig(t, `this is not valid toml [[[`)

	// Wait for debounce (200ms) plus margin for the reload attempt (no event fired for failure)
	requireNever(t, func() bool {
		return h.Watcher.Config().DefaultAgent != "codex"
	}, 300*time.Millisecond, 50*time.Millisecond)

	// Config should still be the original value
	if h.Watcher.Config().DefaultAgent != "codex" {
		assert.Condition(t, func() bool {
			return false
		}, "Config should not change on invalid TOML, DefaultAgent = %q", h.Watcher.Config().DefaultAgent)
	}

	// Watcher should still be working - fix the config
	h.updateConfigAndWait(t, `default_agent = "gemini"`)

	// Now config should be updated
	if h.Watcher.Config().DefaultAgent != "gemini" {
		assert.Condition(t, func() bool {
			return false
		}, "After fix, DefaultAgent = %q, want %q", h.Watcher.Config().DefaultAgent, "gemini")
	}
}

func TestConfigWatcherPreservesRestartRequiredWebConfig(t *testing.T) {
	h := newConfigWatcherHarness(t, `[web]
listen = "127.0.0.1:7400"
public_origin = "https://reviews.example.com"
auth_token = "MDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1ub3BxcnN0dXY"
`)

	h.updateConfigAndWait(t, `[web]
listen = "127.0.0.1:7500"
public_origin = "https://review-ui.example.com"
auth_token = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU"
`)

	assert.Equal(t, "127.0.0.1:7400", h.Watcher.Config().Web.Listen)
	assert.Equal(t, "https://reviews.example.com", h.Watcher.Config().Web.PublicOrigin)
	assert.Equal(t, testBrowserAuthToken, h.Watcher.Config().Web.AuthToken)
}

func TestConfigGetter_Interface(t *testing.T) {
	// Verify both StaticConfig and ConfigWatcher implement ConfigGetter
	var _ ConfigGetter = (*StaticConfig)(nil)
	var _ ConfigGetter = (*ConfigWatcher)(nil)
}

func TestConfigWatcher_DoubleStopSafe(t *testing.T) {
	cw := setupUnstartedWatcher(t, "")

	// Multiple Stop() calls should not panic
	cw.Stop()
	cw.Stop()
	cw.Stop()
}

func TestConfigWatcher_StopAfterStart(t *testing.T) {
	h := newConfigWatcherHarness(t, `default_agent = "test"`)

	// Stop multiple times should not panic (cleanup will also call Stop)
	h.Watcher.Stop()
	h.Watcher.Stop()
}

func TestConfigWatcher_StartAfterStopErrors(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	writeTestFile(t, configPath, `default_agent = "test"`)

	cfg, _ := config.LoadGlobalFrom(configPath)
	broadcaster := NewBroadcaster()
	cw := NewConfigWatcher(configPath, cfg, broadcaster, nil)

	ctx := context.Background()

	// Start and stop
	if err := cw.Start(ctx); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "First Start failed: %v", err)
	}
	cw.Stop()

	// Start after Stop should error (not restart-safe)
	err := cw.Start(ctx)
	if err == nil {
		assert.Condition(t, func() bool {
			return false
		}, "Expected error when calling Start after Stop")
	}
}

func TestConfigWatcher_ReloadCounter(t *testing.T) {
	t.Parallel()
	h := newConfigWatcherHarness(t, `default_agent = "codex"`)

	// Initial counter should be 0
	if h.Watcher.ReloadCounter() != 0 {
		assert.Condition(t, func() bool {
			return false
		}, "Initial ReloadCounter = %d, want 0", h.Watcher.ReloadCounter())
	}

	// First reload
	h.updateConfigAndWait(t, `default_agent = "gemini"`)
	if h.Watcher.ReloadCounter() != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "After first reload, ReloadCounter = %d, want 1", h.Watcher.ReloadCounter())
	}

	// Second reload
	h.updateConfigAndWait(t, `default_agent = "claude-code"`)
	if h.Watcher.ReloadCounter() != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "After second reload, ReloadCounter = %d, want 2", h.Watcher.ReloadCounter())
	}
}

func TestConfigWatcher_AtomicSaveViaRename(t *testing.T) {
	t.Parallel()
	h := newConfigWatcherHarness(t, `default_agent = "codex"`)

	// Simulate atomic save: write to temp file then rename
	tmpFile := filepath.Join(h.dir, "config.toml.tmp")
	writeTestFile(t, tmpFile, `default_agent = "gemini"`)
	if err := os.Rename(tmpFile, h.ConfigPath); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to rename: %v", err)
	}

	h.waitForReload(t)

	// Verify config was updated
	if h.Watcher.Config().DefaultAgent != "gemini" {
		assert.Condition(t, func() bool {
			return false
		}, "After atomic save, DefaultAgent = %q, want %q", h.Watcher.Config().DefaultAgent, "gemini")
	}
}
