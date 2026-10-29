package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestReviewAgentProblems(t *testing.T) {
	// Only the built-in test agent runs; codex is unavailable.
	t.Setenv("PATH", t.TempDir())

	const panels = `
[review.subagents.ok]
agent = "test"

[review.panels.guard]
members = ["ok"]
synthesis_agent = "test"
`
	const experiment = `
[experiments.try]
enabled = true
ratio = %s
workflows = ["review"]

[experiments.try.config]
agent = "codex"
`
	tests := []struct {
		name    string
		config  string
		problem string
	}{
		{
			name:   "single agent that runs",
			config: `default_agent = "test"`,
		},
		{
			name:    "single agent that cannot run",
			config:  `default_agent = "codex"`,
			problem: `"codex"`,
		},
		{
			name: "agent chosen by the default reasoning level",
			config: `default_agent = "test"
review_agent_thorough = "codex"`,
			problem: `"codex"`,
		},
		{
			name: "agent chosen by another reasoning level is not used",
			config: `default_agent = "test"
review_agent_fast = "codex"`,
		},
		{
			name: "post-commit panel does not hide a broken manual single agent",
			config: `default_agent = "codex"
[review]
hook_review_panel = "guard"
` + panels,
			problem: `"codex"`,
		},
		{
			name: "manual panel does not hide a broken post-commit single agent",
			config: `default_agent = "codex"
[review]
default_panel = "guard"
` + panels,
			problem: `"codex"`,
		},
		{
			name: "panels for both kinds of review",
			config: `default_agent = "codex"
[review]
default_panel = "guard"
hook_review_panel = "guard"
` + panels,
		},
		{
			name:    "experiment arm with a broken single agent",
			config:  `default_agent = "test"` + fmt.Sprintf(experiment, "0.5"),
			problem: `"codex"`,
		},
		{
			name:   "experiment arm that no review is assigned to is not checked",
			config: `default_agent = "test"` + fmt.Sprintf(experiment, "0"),
		},
		{
			name:    "experiment arm that every review is assigned to is checked",
			config:  `default_agent = "test"` + fmt.Sprintf(experiment, "1"),
			problem: `"codex"`,
		},
		{
			name: "default arm is checked at ratio 1 because unenrolled reviews use it",
			config: `default_agent = "codex"
[experiments.try]
enabled = true
ratio = 1
workflows = ["review"]

[experiments.try.config]
agent = "test"
`,
			problem: `"codex"`,
		},
		{
			name: "panel with one voting member needs no synthesis agent",
			config: `default_agent = "test"
[review]
default_panel = "guard"
hook_review_panel = "guard"
[review.subagents.ok]
agent = "test"
[review.panels.guard]
members = ["ok"]
synthesis_agent = "codex"
`,
		},
		{
			name: "non-voting observers do not make a panel need a synthesis agent",
			config: `default_agent = "test"
[review]
default_panel = "guard"
hook_review_panel = "guard"
[review.subagents.ok]
agent = "test"
[review.subagents.watcher]
agent = "test"
non_voting = true
[review.panels.guard]
members = ["ok", "watcher"]
synthesis_agent = "codex"
`,
		},
		{
			name: "panel with several voting members needs its synthesis agent",
			config: `default_agent = "test"
[review]
default_panel = "guard"
hook_review_panel = "guard"
[review.subagents.one]
agent = "test"
[review.subagents.two]
agent = "test"
[review.panels.guard]
members = ["one", "two"]
synthesis_agent = "codex"
`,
			problem: "synthesis",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tt.config), 0o600))
			cfg, err := config.LoadGlobalFrom(path)
			require.NoError(t, err)

			problems := reviewAgentProblems(t.Context(), cfg)
			if tt.problem == "" {
				assert.Empty(t, problems)
				return
			}
			require.NotEmpty(t, problems)
			assert.Contains(t, problems[0], tt.problem)
		})
	}
}

func TestAgentHealthCache(t *testing.T) {
	cfg := config.DefaultConfig()
	unhealthy := storage.ComponentHealth{Name: "agents", Message: "no agent"}
	newCache := func(t *testing.T, run func(context.Context, *config.Config) storage.ComponentHealth) *agentHealthCache {
		c := newAgentHealthCache()
		c.run = run
		t.Cleanup(c.stop)
		return c
	}

	t.Run("requests share one check and see a pending result until it finishes", func(t *testing.T) {
		started := make(chan struct{}, 10)
		release := make(chan struct{})
		var runs atomic.Int32
		c := newCache(t, func(context.Context, *config.Config) storage.ComponentHealth {
			runs.Add(1)
			started <- struct{}{}
			<-release
			return unhealthy
		})
		var firsts []storage.ComponentHealth
		c.onFirst = func(h storage.ComponentHealth) { firsts = append(firsts, h) }

		for range 5 {
			pending := c.observe(cfg)
			assert.True(t, pending.Healthy)
			assert.Contains(t, pending.Message, "still running")
		}
		<-started
		assert.EqualValues(t, 1, runs.Load(), "concurrent requests start one check")

		close(release)
		c.wait()
		assert.Equal(t, unhealthy, c.observe(cfg))
		c.wait()
		assert.Equal(t, []storage.ComponentHealth{unhealthy}, firsts, "only the first result is reported at startup")
	})

	t.Run("stop cancels a running check, waits for it, and starts no more", func(t *testing.T) {
		started := make(chan struct{}, 10)
		var runs atomic.Int32
		c := newCache(t, func(ctx context.Context, _ *config.Config) storage.ComponentHealth {
			runs.Add(1)
			started <- struct{}{}
			<-ctx.Done()
			return unhealthy
		})

		c.observe(cfg)
		<-started
		c.stop()

		pending := c.observe(cfg)
		assert.True(t, pending.Healthy, "a result that finished after stop is discarded")
		assert.Contains(t, pending.Message, "still running")
		assert.EqualValues(t, 1, runs.Load(), "no check starts after stop")
	})
}

func TestReviewAgentProblemsStopsWhenCancelled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.DefaultAgent = "codex"
	require.NotEmpty(t, reviewAgentProblems(t.Context(), cfg), "codex is unavailable")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Empty(t, reviewAgentProblems(ctx, cfg), "a cancelled check does not run its probes")
}
