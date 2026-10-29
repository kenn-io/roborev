package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
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
			name: "default arm that no review is assigned to is not checked",
			config: `default_agent = "codex"
[experiments.try]
enabled = true
ratio = 1
workflows = ["review"]

[experiments.try.config]
agent = "test"
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tt.config), 0o600))
			cfg, err := config.LoadGlobalFrom(path)
			require.NoError(t, err)

			problems := reviewAgentProblems(cfg)
			if tt.problem == "" {
				assert.Empty(t, problems)
				return
			}
			require.NotEmpty(t, problems)
			assert.Contains(t, problems[0], tt.problem)
		})
	}
}
