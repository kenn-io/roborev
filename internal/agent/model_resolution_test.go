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

// TestModelForSelectedAgentACPBackupPairing pins the backup-path pairing
// semantics for a configured ACP backup agent. Backup agent and model
// precedence resolve independently, so a backup model pairs with the backup
// agent resolvable AT THE MODEL'S OWN LAYER: repo-layer models (repo
// {workflow}_backup_model, repo backup_model) pair with the normally-resolved
// backup agent — the selected agent on this path — and always apply; a global
// {workflow}_backup_model pairs with the agent resolvable from global-layer
// fields only (global {workflow}_backup_agent, else default_backup_agent); a
// global default_backup_model pairs with default_backup_agent only. A model
// whose paired agent is not the selected ACP backup agent is skipped in favor
// of the agent's own [acp].model, because ACP exact-membership validation
// would otherwise reject the foreign value and break the backup handoff.
// Non-ACP backup agents keep legacy behavior.
func TestModelForSelectedAgentACPBackupPairing(t *testing.T) {
	t.Parallel()

	const acpName = "agy-acp"
	const acpModel = "gemini-3.5-flash"

	tests := []struct {
		name          string
		repoCfg       *config.RepoConfig
		cfg           *config.Config
		workflow      string
		selectedAgent string
		want          string
		note          string
	}{
		{
			// Explicit workflow-specific backup_model paired with the ACP
			// backup agent: it is used verbatim (thinking suffixes included).
			name: "acp backup, workflow-specific backup_model -> that model",
			cfg: &config.Config{
				DefaultAgent:      "codex",
				ReviewAgent:       "gemini",
				ReviewBackupAgent: acpName,
				ReviewBackupModel: "gemini-3.5-flash:high",
				ACP:               &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          "gemini-3.5-flash:high",
			note:          "explicit paired backup_model wins",
		},
		{
			// No backup_model anywhere: returns "" so the ACP backup agent
			// keeps its own baked-in [acp].model (callers only override when
			// the resolved model is non-empty). This is the safe empty case.
			name: "acp backup, no backup_model -> empty (keeps [acp].model)",
			cfg: &config.Config{
				DefaultAgent:      "codex",
				ReviewAgent:       "gemini",
				ReviewBackupAgent: acpName,
				ACP:               &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          "",
			note:          "empty backup_model leaves [acp].model intact",
		},
		{
			// THE LEAK (cross-layer mispair): the ACP backup agent is
			// workflow-specific (review_backup_agent), but the only backup
			// model is the inherited global default_backup_model, which pairs
			// with default_backup_agent (unset here). Handing the inherited
			// model to the ACP agent would fail its exact-membership
			// validation and break the backup handoff, so the agent's own
			// [acp].model is surfaced instead (keeping persisted job metadata
			// accurate).
			name: "acp backup, inherited default_backup_model, no default_backup_agent -> acp model",
			cfg: &config.Config{
				DefaultAgent:       "codex",
				ReviewAgent:        "gemini",
				ReviewBackupAgent:  acpName,
				DefaultBackupModel: "gpt-5.4-mini",
				ACP:                &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          acpModel,
			note:          "inherited default_backup_model is unpaired -> [acp].model",
		},
		{
			// Same mispair with default_backup_agent set to a DIFFERENT
			// agent: default_backup_model pairs with that agent, not the
			// workflow-selected ACP backup agent.
			name: "acp backup, default_backup_model paired with other default_backup_agent -> acp model",
			cfg: &config.Config{
				DefaultAgent:       "codex",
				ReviewAgent:        "gemini",
				ReviewBackupAgent:  acpName,
				DefaultBackupAgent: "claude",
				DefaultBackupModel: "claude-sonnet",
				ACP:                &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          acpModel,
			note:          "default_backup_model belongs to claude, not the ACP agent",
		},
		{
			// Mispair with no [acp].model configured either: nothing safe to
			// hand the agent, so resolve to "" (the agent runs its own
			// default).
			name: "acp backup, mispaired inherited model, no [acp].model -> empty",
			cfg: &config.Config{
				DefaultAgent:       "codex",
				ReviewAgent:        "gemini",
				ReviewBackupAgent:  acpName,
				DefaultBackupModel: "gpt-5.4-mini",
				ACP:                &config.ACPAgentConfig{Name: acpName},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          "",
			note:          "mispaired inherited model with no [acp].model resolves empty",
		},
		{
			// Legit same-layer global pair: default_backup_agent IS the ACP
			// agent and default_backup_model was configured alongside it.
			name: "acp backup via default_backup_agent, default_backup_model -> that model",
			cfg: &config.Config{
				DefaultAgent:       "codex",
				ReviewAgent:        "gemini",
				DefaultBackupAgent: acpName,
				DefaultBackupModel: "gemini-3.0-pro",
				ACP:                &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          "gemini-3.0-pro",
			note:          "same-layer default_backup_agent+default_backup_model pair holds",
		},
		{
			// Legit inheritance: the backup agent inherits from
			// default_backup_agent=ACP while the model is workflow-scoped
			// (global review_backup_model). Workflow-scoped models pair with
			// the resolved backup agent, so the pair holds — thinking
			// suffixes included.
			name: "acp backup inherited from default_backup_agent, workflow backup_model -> that model",
			cfg: &config.Config{
				DefaultAgent:       "codex",
				ReviewAgent:        "gemini",
				DefaultBackupAgent: acpName,
				ReviewBackupModel:  "gemini-3.5-flash:high",
				ACP:                &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          "gemini-3.5-flash:high",
			note:          "workflow-scoped backup_model pairs with the resolved backup agent",
		},
		{
			// Scope guard: a non-ACP backup agent keeps legacy behavior — the
			// inherited default_backup_model still applies even though no
			// default_backup_agent pairs with it. Any incompatibility
			// surfaces at the agent layer as a visible error (e.g. gemini
			// via agy rejects explicit models loudly), never as ACP's
			// silent failover break.
			name: "non-acp backup, inherited default_backup_model -> honored (legacy)",
			cfg: &config.Config{
				DefaultAgent:       "codex",
				ReviewAgent:        "gemini",
				ReviewBackupAgent:  "claude",
				DefaultBackupModel: "claude-sonnet",
				ACP:                &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: "claude-code",
			want:          "claude-sonnet",
			note:          "non-ACP backup agents are unaffected by the guard",
		},
		{
			// Cross-file mispair: a repo-layer review_backup_agent selects
			// the ACP agent, but the only backup model is a global
			// review_backup_model written against the global backup agent
			// resolution (empty here). The repo agent override must not
			// capture the global model.
			name: "repo acp backup agent, global review_backup_model -> acp model",
			repoCfg: &config.RepoConfig{
				ReviewBackupAgent: acpName,
			},
			cfg: &config.Config{
				DefaultAgent:      "codex",
				ReviewAgent:       "gemini",
				ReviewBackupModel: "gpt-5.4",
				ACP:               &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          acpModel,
			note:          "repo agent override must not capture a global workflow model",
		},
		{
			// Cross-file pair that holds: the global review_backup_model is
			// paired with the global review_backup_agent, which IS the ACP
			// agent the repo override also selects.
			name: "repo acp backup agent, global pair also acp -> global model",
			repoCfg: &config.RepoConfig{
				ReviewBackupAgent: acpName,
			},
			cfg: &config.Config{
				DefaultAgent:      "codex",
				ReviewAgent:       "gemini",
				ReviewBackupAgent: acpName,
				ReviewBackupModel: "gemini-3.0-pro",
				ACP:               &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          "gemini-3.0-pro",
			note:          "global model paired with a matching global agent applies",
		},
		{
			// Repo-layer model with the backup agent inherited from the
			// global layer: the repo author wrote the model against the
			// inherited agent, so the pair holds (suffixes included).
			name: "repo review_backup_model, global acp backup agent -> repo model",
			repoCfg: &config.RepoConfig{
				ReviewBackupModel: "gemini-3.5-flash:high",
			},
			cfg: &config.Config{
				DefaultAgent:      "codex",
				ReviewAgent:       "gemini",
				ReviewBackupAgent: acpName,
				ACP:               &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          "gemini-3.5-flash:high",
			note:          "repo model pairs with the normally-resolved backup agent",
		},
		{
			// Both repo-layer: agent and model configured together in the
			// repo file.
			name: "repo acp backup agent and repo backup model -> repo model",
			repoCfg: &config.RepoConfig{
				ReviewBackupAgent: acpName,
				ReviewBackupModel: "gemini-3.0-pro",
			},
			cfg: &config.Config{
				DefaultAgent: "codex",
				ReviewAgent:  "gemini",
				ACP:          &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: acpName,
			want:          "gemini-3.0-pro",
			note:          "same-file repo pair applies",
		},
		{
			// Scope guard for the cross-file case: a non-ACP repo backup
			// agent still receives the global workflow model (legacy
			// behavior).
			name: "repo non-acp backup agent, global review_backup_model -> honored (legacy)",
			repoCfg: &config.RepoConfig{
				ReviewBackupAgent: "claude",
			},
			cfg: &config.Config{
				DefaultAgent:      "codex",
				ReviewAgent:       "gemini",
				ReviewBackupModel: "gpt-5.4",
				ACP:               &config.ACPAgentConfig{Name: acpName, Model: acpModel},
			},
			workflow:      "review",
			selectedAgent: "claude-code",
			want:          "gpt-5.4",
			note:          "non-ACP backup agents are unaffected by the layer rule",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolution, err := ResolveWorkflowConfigFromConfig(
				"", tt.repoCfg, tt.cfg, tt.workflow, "standard",
			)
			require.NoError(t, err)
			require.True(t, resolution.UsesBackupAgent(tt.selectedAgent),
				"expected %q to be resolved via the backup path", tt.selectedAgent)
			got := resolution.ModelForSelectedAgent(tt.selectedAgent, "")
			require.Equal(t, tt.want, got, tt.note)
		})
	}
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
