package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// TestModelForSelectedAgentACPWorkflowModelPrecedence pins the semantic that
// workflow models (review_model, leveled variants) follow their paired
// workflow agent. A workflow model configured for the default reviewer must
// NOT be handed to a CLI-selected configured ACP agent; the [acp].model must
// win in that case. When the workflow agent IS the ACP agent, the paired
// workflow model applies as before.
func TestModelForSelectedAgentACPWorkflowModelPrecedence(t *testing.T) {
	t.Parallel()

	const acpName = "agy-sdk"
	const acpModel = "gemini-3.5-flash"

	tests := []struct {
		name          string
		cfg           *config.Config
		workflow      string
		level         string
		selectedAgent string
		cliModel      string
		want          string
	}{
		{
			// THE BUG: global review_model paired with the default reviewer
			// must not leak to the CLI-selected ACP agent.
			name: "acp selected, workflow model but no workflow agent -> acp model",
			cfg: &config.Config{
				DefaultAgent: "codex",
				ReviewModel:  "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			level:         "standard",
			selectedAgent: acpName,
			want:          acpModel,
		},
		{
			// Workflow agent IS the ACP agent: paired workflow model applies.
			name: "acp selected and review_agent is acp -> workflow model",
			cfg: &config.Config{
				DefaultAgent: "codex",
				ReviewAgent:  acpName,
				ReviewModel:  "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			level:         "standard",
			selectedAgent: acpName,
			want:          "gpt-5.4",
		},
		{
			// Explicit CLI --model always wins.
			name: "acp selected with explicit cli model -> cli model",
			cfg: &config.Config{
				DefaultAgent: "codex",
				ReviewModel:  "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			level:         "standard",
			selectedAgent: acpName,
			cliModel:      "gpt-5.4-custom",
			want:          "gpt-5.4-custom",
		},
		{
			// Scope guard: non-ACP selected agent keeps legacy behavior
			// (workflow model still applies).
			name: "non-acp selected differing from default -> workflow model",
			cfg: &config.Config{
				DefaultAgent: "codex",
				ReviewModel:  "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			level:         "standard",
			selectedAgent: "gemini",
			want:          "gpt-5.4",
		},
		{
			// Existing fallback: no workflow model anywhere -> acp model.
			name: "acp selected, no workflow model -> acp model",
			cfg: &config.Config{
				DefaultAgent: "codex",
				ACP:          &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			level:         "standard",
			selectedAgent: acpName,
			want:          acpModel,
		},
		{
			// Leveled variant, no leveled workflow agent -> acp model.
			name: "acp selected, thorough model but no thorough agent -> acp model",
			cfg: &config.Config{
				DefaultAgent:        "codex",
				ReviewModelThorough: "gpt-5.4-thorough",
				ACP:                 &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			level:         "thorough",
			selectedAgent: acpName,
			want:          acpModel,
		},
		{
			// Default agent IS the ACP agent, but the workflow agent is a
			// different agent: the workflow model pairs with that workflow
			// agent and must not leak to the selected ACP agent. With no
			// generic model, the ACP fallback supplies [acp].model.
			name: "acp is default, workflow agent differs -> acp model",
			cfg: &config.Config{
				DefaultAgent: acpName,
				ReviewAgent:  "codex",
				ReviewModel:  "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			level:         "standard",
			selectedAgent: acpName,
			want:          acpModel,
		},
		{
			// Same, but a generic model is configured: the generic model
			// pairs with the default agent, which IS the selected ACP agent,
			// so it applies (and beats the [acp].model fallback).
			name: "acp is default, workflow agent differs, generic model -> generic model",
			cfg: &config.Config{
				DefaultAgent: acpName,
				DefaultModel: "gemini-custom",
				ReviewAgent:  "codex",
				ReviewModel:  "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			level:         "standard",
			selectedAgent: acpName,
			want:          "gemini-custom",
		},
		{
			// No workflow agent configured: the workflow model pairs with
			// the default agent, which IS the selected ACP agent, so the
			// workflow model still applies.
			name: "acp is default, no workflow agent, workflow model -> workflow model",
			cfg: &config.Config{
				DefaultAgent: acpName,
				ReviewModel:  "gemini-paired",
				ACP:          &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			level:         "standard",
			selectedAgent: acpName,
			want:          "gemini-paired",
		},
		{
			// Leveled variant with leveled workflow agent -> leveled model.
			name: "acp selected, thorough agent is acp -> thorough model",
			cfg: &config.Config{
				DefaultAgent:        "codex",
				ReviewAgentThorough: acpName,
				ReviewModelThorough: "gpt-5.4-thorough",
				ACP:                 &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			level:         "thorough",
			selectedAgent: acpName,
			want:          "gpt-5.4-thorough",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			resolution, err := ResolveWorkflowConfigFromConfig(
				tt.selectedAgent, nil, tt.cfg, tt.workflow, tt.level,
			)
			require.NoError(t, err)
			got := resolution.ModelForSelectedAgent(tt.selectedAgent, tt.cliModel)
			assert.Equal(tt.want, got,
				"ModelForSelectedAgent(%q, %q) = %q, want %q",
				tt.selectedAgent, tt.cliModel, got, tt.want)
		})
	}
}

func TestResolveWorkflowModelForAgentSkipsGenericDefaultModel(t *testing.T) {
	t.Parallel()

	mkCfg := func(workflow string, workflowAgent string, workflowModel string) *config.Config {
		cfg := &config.Config{
			DefaultAgent: "codex",
			DefaultModel: "gpt-5.4",
		}
		switch workflow {
		case "fix":
			cfg.FixAgent = workflowAgent
			cfg.FixModel = workflowModel
		case "review":
			cfg.ReviewAgent = workflowAgent
			cfg.ReviewModel = workflowModel
		case "refine":
			cfg.RefineAgent = workflowAgent
			cfg.RefineModel = workflowModel
		case "security":
			cfg.SecurityAgent = workflowAgent
			cfg.SecurityModel = workflowModel
		case "design":
			cfg.DesignAgent = workflowAgent
			cfg.DesignModel = workflowModel
		default:
			require.Condition(t, func() bool { return false }, "unsupported workflow %q", workflow)
		}
		return cfg
	}

	tests := []struct {
		name          string
		workflow      string
		selectedAgent string
		cfg           *config.Config
		want          string
	}{
		{
			name:          "fix skips default model for configured non-default agent",
			workflow:      "fix",
			selectedAgent: "claude-code",
			cfg:           mkCfg("fix", "claude", ""),
			want:          "",
		},
		{
			name:          "fix uses workflow model for configured non-default agent",
			workflow:      "fix",
			selectedAgent: "claude-code",
			cfg:           mkCfg("fix", "claude", "claude-sonnet"),
			want:          "claude-sonnet",
		},
		{
			name:          "fix uses default model for actual fallback default agent",
			workflow:      "fix",
			selectedAgent: "codex",
			cfg:           mkCfg("fix", "claude", ""),
			want:          "gpt-5.4",
		},
		{
			name:          "review skips default model for configured non-default agent",
			workflow:      "review",
			selectedAgent: "claude-code",
			cfg:           mkCfg("review", "claude", ""),
			want:          "",
		},
		{
			name:          "review uses workflow model for configured non-default agent",
			workflow:      "review",
			selectedAgent: "claude-code",
			cfg:           mkCfg("review", "claude", "claude-sonnet"),
			want:          "claude-sonnet",
		},
		{
			name:          "review uses default model for actual fallback default agent",
			workflow:      "review",
			selectedAgent: "codex",
			cfg:           mkCfg("review", "claude", ""),
			want:          "gpt-5.4",
		},
		{
			name:          "refine skips default model for configured non-default agent",
			workflow:      "refine",
			selectedAgent: "claude-code",
			cfg:           mkCfg("refine", "claude", ""),
			want:          "",
		},
		{
			name:          "refine uses workflow model for configured non-default agent",
			workflow:      "refine",
			selectedAgent: "claude-code",
			cfg:           mkCfg("refine", "claude", "claude-sonnet"),
			want:          "claude-sonnet",
		},
		{
			name:          "refine uses default model for actual fallback default agent",
			workflow:      "refine",
			selectedAgent: "codex",
			cfg:           mkCfg("refine", "claude", ""),
			want:          "gpt-5.4",
		},
		{
			name:          "security skips default model for configured non-default agent",
			workflow:      "security",
			selectedAgent: "claude-code",
			cfg:           mkCfg("security", "claude", ""),
			want:          "",
		},
		{
			name:          "security uses workflow model for configured non-default agent",
			workflow:      "security",
			selectedAgent: "claude-code",
			cfg:           mkCfg("security", "claude", "claude-sonnet"),
			want:          "claude-sonnet",
		},
		{
			name:          "security uses default model for actual fallback default agent",
			workflow:      "security",
			selectedAgent: "codex",
			cfg:           mkCfg("security", "claude", ""),
			want:          "gpt-5.4",
		},
		{
			name:          "design skips default model for configured non-default agent",
			workflow:      "design",
			selectedAgent: "claude-code",
			cfg:           mkCfg("design", "claude", ""),
			want:          "",
		},
		{
			name:          "design uses workflow model for configured non-default agent",
			workflow:      "design",
			selectedAgent: "claude-code",
			cfg:           mkCfg("design", "claude", "claude-sonnet"),
			want:          "claude-sonnet",
		},
		{
			name:          "design uses default model for actual fallback default agent",
			workflow:      "design",
			selectedAgent: "codex",
			cfg:           mkCfg("design", "claude", ""),
			want:          "gpt-5.4",
		},
	}

	repoPath := t.TempDir()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveWorkflowModelForAgent(
				tt.selectedAgent,
				"",
				repoPath,
				tt.cfg,
				tt.workflow,
				"standard",
			)
			require.Equal(t, tt.want, got, "ResolveWorkflowModelForAgent() = %q, want %q", got, tt.want)
		})
	}
}

func TestResolveWorkflowModelForAgentACPDefaultAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		workflow string
		cfg      *config.Config
		want     string
	}{
		{
			name:     "fix keeps default model for acp default alias",
			workflow: "fix",
			cfg: &config.Config{
				DefaultAgent: "custom-acp",
				DefaultModel: "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: "custom-acp"},
			},
			want: "gpt-5.4",
		},
		{
			name:     "review keeps default model for acp default alias",
			workflow: "review",
			cfg: &config.Config{
				DefaultAgent: "custom-acp",
				DefaultModel: "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: "custom-acp"},
			},
			want: "gpt-5.4",
		},
		{
			name:     "refine keeps default model for acp default alias",
			workflow: "refine",
			cfg: &config.Config{
				DefaultAgent: "custom-acp",
				DefaultModel: "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: "custom-acp"},
			},
			want: "gpt-5.4",
		},
		{
			name:     "security keeps default model for acp default alias",
			workflow: "security",
			cfg: &config.Config{
				DefaultAgent: "custom-acp",
				DefaultModel: "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: "custom-acp"},
			},
			want: "gpt-5.4",
		},
		{
			name:     "design keeps default model for acp default alias",
			workflow: "design",
			cfg: &config.Config{
				DefaultAgent: "custom-acp",
				DefaultModel: "gpt-5.4",
				ACP:          &config.ACPAgentConfig{Name: "custom-acp"},
			},
			want: "gpt-5.4",
		},
	}

	repoPath := t.TempDir()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveWorkflowModelForAgent(
				"acp",
				"",
				repoPath,
				tt.cfg,
				tt.workflow,
				"standard",
			)
			require.Equal(t, tt.want, got, "ResolveWorkflowModelForAgent() = %q, want %q", got, tt.want)
		})
	}
}

func TestResolveWorkflowModelForAgentRepoDefaultACPAgent(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoPath, ".roborev.toml"), []byte(`
agent = "custom-acp"
`), 0o644); err != nil {
		require.NoError(t, err)
	}

	cfg := &config.Config{
		DefaultAgent: "codex",
		DefaultModel: "gpt-5.4",
		ACP:          &config.ACPAgentConfig{Name: "custom-acp"},
	}

	got := ResolveWorkflowModelForAgent(
		"acp",
		"",
		repoPath,
		cfg,
		"review",
		"standard",
	)
	require.Equal(t, "gpt-5.4", got, "ResolveWorkflowModelForAgent() = %q, want %q", got, "gpt-5.4")
}

func TestResolveWorkflowConfigModelForSelectedAgent_UsesBackupModelForAliasMatch(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ReviewAgent:       "gemini",
		ReviewBackupAgent: "claude",
		ReviewBackupModel: "claude-sonnet",
	}

	resolution, err := ResolveWorkflowConfig(
		"", t.TempDir(), cfg, "review", "standard",
	)
	require.NoError(t, err)

	require.Equal(t, "gemini", resolution.PreferredAgent)
	require.Equal(t, "claude", resolution.BackupAgent)
	require.Equal(t, "claude-sonnet", resolution.ModelForSelectedAgent("claude-code", ""))
}

func TestResolveWorkflowConfigModelForSelectedAgent_BackupWithoutModelKeepsDefault(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		DefaultAgent:      "codex",
		DefaultModel:      "gpt-5.4",
		ReviewAgent:       "gemini",
		ReviewBackupAgent: "claude",
	}

	resolution, err := ResolveWorkflowConfig(
		"", t.TempDir(), cfg, "review", "standard",
	)
	require.NoError(t, err)

	require.Empty(t, resolution.ModelForSelectedAgent("claude-code", ""))
}

func TestResolveWorkflowConfigIgnoresMalformedRepoConfig(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	err := os.WriteFile(
		filepath.Join(repoPath, ".roborev.toml"),
		[]byte("review_model = ["),
		0o644,
	)
	require.NoError(t, err)

	resolution, err := ResolveWorkflowConfig("", repoPath, &config.Config{
		ReviewAgent: "gemini",
	}, "review", "fast")
	require.NoError(t, err)
	require.Equal(t, "gemini", resolution.PreferredAgent)
}

func TestResolveWorkflowConfigFromConfigNilRepoConfigDoesNotReadCWD(t *testing.T) {
	cwd := t.TempDir()
	err := os.WriteFile(
		filepath.Join(cwd, ".roborev.toml"),
		[]byte("review_model = \"cwd-model\"\nreview_backup_model = \"cwd-backup\"\n"),
		0o644,
	)
	require.NoError(t, err)
	t.Chdir(cwd)

	cfg := &config.Config{
		DefaultAgent:      "codex",
		DefaultModel:      "global-model",
		ReviewBackupAgent: "claude",
		ReviewBackupModel: "global-backup",
	}
	resolution, err := ResolveWorkflowConfigFromConfig(
		"", nil, cfg, "review", "standard",
	)
	require.NoError(t, err)

	require.Equal(t, "global-model", resolution.ModelForSelectedAgent("codex", ""))
	require.Equal(t, "global-backup", resolution.ModelForSelectedAgent("claude-code", ""))
}
