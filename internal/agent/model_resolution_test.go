package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

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
