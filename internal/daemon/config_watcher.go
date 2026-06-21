package daemon

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

// ConfigGetter provides access to the current config
type ConfigGetter interface {
	Config() *config.Config
}

// StaticConfig wraps a config for use without hot-reloading (e.g., in tests)
type StaticConfig struct {
	cfg *config.Config
}

// NewStaticConfig creates a ConfigGetter that always returns the same config
func NewStaticConfig(cfg *config.Config) *StaticConfig {
	return &StaticConfig{cfg: cfg}
}

// Config returns the static config
func (sc *StaticConfig) Config() *config.Config {
	return sc.cfg
}

// ConfigWatcher watches config.toml for changes and reloads configuration.
//
// Hot-reloadable settings take effect immediately: default_agent, job_timeout,
// agent_quota_cooldown, allow_unsafe_agents, anthropic_api_key,
// review_context_count.
//
// Settings requiring restart: server_addr, max_workers, [sync] section.
// These are read at startup and the running values are preserved even if the
// config file changes. CLI flag overrides (--addr, --workers) only apply to
// restart-required settings, so they remain in effect for the daemon's lifetime.
// The config object may show file values after reload, but the actual running
// server address and worker pool size are fixed at startup.
//
// Note: ConfigWatcher is not restart-safe. Once Stop() is called, Start() will
// return an error. Create a new ConfigWatcher instance if restart is needed.
type ConfigWatcher struct {
	configPath     string
	cfg            *config.Config
	cfgMu          sync.RWMutex
	broadcaster    Broadcaster
	activityLog    *ActivityLog
	watcher        *fsnotify.Watcher
	stopCh         chan struct{}
	stopOnce       sync.Once
	stopped        bool      // True after Stop() is called
	lastReloadedAt time.Time // Time of last successful config reload
	reloadCounter  uint64    // Monotonic counter for reload events (sub-second precision)
}

// NewConfigWatcher creates a new config watcher
func NewConfigWatcher(configPath string, cfg *config.Config, broadcaster Broadcaster, activityLog *ActivityLog) *ConfigWatcher {
	return &ConfigWatcher{
		configPath:  configPath,
		cfg:         cfg,
		broadcaster: broadcaster,
		activityLog: activityLog,
		stopCh:      make(chan struct{}),
	}
}

// Start begins watching the config file for changes.
// Returns an error if the watcher has already been stopped.
func (cw *ConfigWatcher) Start(ctx context.Context) error {
	// Check if already stopped (not restart-safe)
	cw.cfgMu.RLock()
	stopped := cw.stopped
	cw.cfgMu.RUnlock()
	if stopped {
		return fmt.Errorf("config watcher already stopped; create a new instance to restart")
	}

	// Skip watching if no config path provided (e.g., in tests)
	if cw.configPath == "" {
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	cw.cfgMu.Lock()
	cw.watcher = watcher
	cw.cfgMu.Unlock()

	// Watch the directory containing the config file, not the file itself.
	// This handles editors that do atomic writes (delete + create).
	configDir := filepath.Dir(cw.configPath)
	configFile := filepath.Base(cw.configPath)

	if err := watcher.Add(configDir); err != nil {
		watcher.Close()
		cw.cfgMu.Lock()
		cw.watcher = nil
		cw.cfgMu.Unlock()
		return err
	}

	go cw.watchLoop(ctx, configFile)
	return nil
}

// Stop stops the config watcher. Safe to call multiple times.
func (cw *ConfigWatcher) Stop() {
	cw.stopOnce.Do(func() {
		cw.cfgMu.Lock()
		cw.stopped = true
		w := cw.watcher
		cw.cfgMu.Unlock()
		close(cw.stopCh)
		if w != nil {
			w.Close()
		}
	})
}

// Config returns the current config with read lock
func (cw *ConfigWatcher) Config() *config.Config {
	cw.cfgMu.RLock()
	defer cw.cfgMu.RUnlock()
	return cw.cfg
}

// LastReloadedAt returns the time of the last successful config reload
func (cw *ConfigWatcher) LastReloadedAt() time.Time {
	cw.cfgMu.RLock()
	defer cw.cfgMu.RUnlock()
	return cw.lastReloadedAt
}

// ReloadCounter returns a monotonic counter incremented on each reload.
// Use this instead of timestamp comparison to detect reloads that happen
// within the same second.
func (cw *ConfigWatcher) ReloadCounter() uint64 {
	cw.cfgMu.RLock()
	defer cw.cfgMu.RUnlock()
	return cw.reloadCounter
}

func (cw *ConfigWatcher) watchLoop(ctx context.Context, configFile string) {
	// Debounce timer to handle rapid file changes
	var debounceTimer *time.Timer
	debounceDelay := 200 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return
		case <-cw.stopCh:
			return
		case event, ok := <-cw.watcher.Events:
			if !ok {
				return
			}

			// Only react to changes to our config file
			if filepath.Base(event.Name) != configFile {
				continue
			}

			// React to write, create, or rename events
			// Rename is needed for editors that do atomic saves via rename (e.g., vim)
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}

			// Debounce: reset timer on each event
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounceDelay, func() {
				cw.reloadConfig()
			})

		case err, ok := <-cw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Config watcher error: %v", err)
		}
	}
}

func (cw *ConfigWatcher) reloadConfig() {
	newCfg, err := config.LoadGlobalFrom(cw.configPath)
	if err != nil {
		log.Printf("Failed to reload config: %v", err)
		return
	}

	cw.cfgMu.Lock()
	oldCfg := cw.cfg
	cw.cfg = newCfg
	cw.lastReloadedAt = time.Now()
	cw.reloadCounter++
	cw.cfgMu.Unlock()

	// Update global agent settings
	agent.SetAllowUnsafeAgents(newCfg.AllowUnsafeAgents != nil && *newCfg.AllowUnsafeAgents)
	agent.SetCodexSandboxDisabled(newCfg.DisableCodexSandbox)
	agent.SetAnthropicAPIKey(newCfg.AnthropicAPIKey)

	// Log what changed (for debugging)
	logConfigChanges(oldCfg, newCfg)

	// Broadcast config reloaded event to notify connected clients
	cw.broadcaster.Broadcast(Event{
		Type: "config.reloaded",
		TS:   time.Now(),
	})

	if cw.activityLog != nil {
		cw.activityLog.Log(
			"config.reloaded", "config",
			"config reloaded",
			map[string]string{"path": cw.configPath},
		)
	}

	log.Printf("Config reloaded successfully")
}

func logConfigChanges(old, new *config.Config) {
	if old.DefaultAgent != new.DefaultAgent {
		log.Printf("Config change: default_agent %q -> %q", old.DefaultAgent, new.DefaultAgent)
	}
	if old.ReviewContextCount != new.ReviewContextCount {
		log.Printf("Config change: review_context_count %d -> %d", old.ReviewContextCount, new.ReviewContextCount)
	}
	if old.JobTimeoutMinutes != new.JobTimeoutMinutes {
		log.Printf("Config change: job_timeout_minutes %d -> %d", old.JobTimeoutMinutes, new.JobTimeoutMinutes)
	}
	if old.AgentQuotaCooldown != new.AgentQuotaCooldown {
		log.Printf("Config change: agent_quota_cooldown %q -> %q", old.AgentQuotaCooldown, new.AgentQuotaCooldown)
	}
	oldUnsafe := old.AllowUnsafeAgents != nil && *old.AllowUnsafeAgents
	newUnsafe := new.AllowUnsafeAgents != nil && *new.AllowUnsafeAgents
	if oldUnsafe != newUnsafe {
		log.Printf("Config change: allow_unsafe_agents %v -> %v", oldUnsafe, newUnsafe)
	}
	if old.MaxWorkers != new.MaxWorkers {
		log.Printf("Config change: max_workers %d -> %d (requires daemon restart to take effect)", old.MaxWorkers, new.MaxWorkers)
	}
	if old.ServerAddr != new.ServerAddr {
		log.Printf("Config change: server_addr %q -> %q (requires daemon restart to take effect)", old.ServerAddr, new.ServerAddr)
	}
}
