package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

func setupGhActionTest(t *testing.T) (*testutil.TestRepo, string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	t.Setenv("ROBOREV_DATA_DIR", filepath.Join(tmpHome, ".roborev"))

	repo := testutil.NewTestRepo(t)
	t.Cleanup(repo.Chdir())

	outPath := filepath.Join(repo.Root, ".github", "workflows", "roborev.yml")
	return repo, outPath
}

func TestGhActionCmd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to shell script stubs")
	}

	tests := []struct {
		name             string
		flags            []string
		repoConfig       string
		expectError      bool
		errorContains    string
		expectedContains []string
		notContains      []string
	}{
		{
			name:             "default flags",
			flags:            []string{},
			expectedContains: []string{"OPENAI_API_KEY", "@openai/codex@latest", "roborev ci review"},
		},
		{
			name:             "custom agent flag",
			flags:            []string{"--agent", "claude-code"},
			expectedContains: []string{"ANTHROPIC_API_KEY", "@anthropic-ai/claude-code@latest"},
		},
		{
			name:             "multi-agent flag",
			flags:            []string{"--agent", "codex,claude-code"},
			expectedContains: []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "@openai/codex@latest", "@anthropic-ai/claude-code@latest"},
		},
		{
			name:             "pinned version",
			flags:            []string{"--roborev-version", "0.33.1"},
			expectedContains: []string{`ROBOREV_VERSION="0.33.1"`},
		},
		{
			name:             "infers agent from repo config",
			repoConfig:       "agent = \"gemini\"\n",
			expectedContains: []string{"GOOGLE_API_KEY", "@google/gemini-cli@latest"},
		},
		{
			name:             "flag overrides repo config",
			repoConfig:       "agent = \"gemini\"\n",
			flags:            []string{"--agent", "codex"},
			expectedContains: []string{"OPENAI_API_KEY"},
		},
		{
			name:             "kilo gets multi-provider guidance",
			flags:            []string{"--agent", "kilo"},
			expectedContains: []string{"ANTHROPIC_API_KEY", "@kilocode/cli@latest", "different model provider", "default for kilo"},
		},
		{
			name:             "kiro has no secret in env block",
			flags:            []string{"--agent", "kiro"},
			expectedContains: []string{"kiro.dev"},
			notContains:      []string{"OPENAI_API_KEY:", "AWS_ACCESS_KEY_ID:"},
		},
		{
			name:             "infers agents from repo CI config",
			repoConfig:       "[ci]\nagents = [\"codex\", \"claude-code\"]\n",
			expectedContains: []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, outPath := setupGhActionTest(t)

			if tt.repoConfig != "" {
				require.NoError(t, os.WriteFile(filepath.Join(repo.Root, ".roborev.toml"), []byte(tt.repoConfig), 0o644), "failed to write repo config")
				_ = repo // Avoid unused variable warning
			}

			cmd := ghActionCmd()
			args := append([]string{}, tt.flags...)
			args = append(args, "--output", outPath)
			cmd.SetArgs(args)

			err := cmd.Execute()

			if tt.expectError {
				require.Error(t, err, "expected error")
				if tt.errorContains != "" {
					require.ErrorContains(t, err, tt.errorContains, "error should contain %q", tt.errorContains)
				}
				return
			}
			require.NoError(t, err, "unexpected error: %v", err)

			if len(tt.expectedContains) > 0 || len(tt.notContains) > 0 {
				contentBytes, err := os.ReadFile(outPath)
				require.NoError(t, err, "failed to read generated file")
				content := string(contentBytes)
				for _, expected := range tt.expectedContains {
					assert.Contains(t, content, expected, "generated file missing expected content: %q", expected)
				}
				for _, bad := range tt.notContains {
					assert.NotContains(t, content, bad, "generated file should not contain: %q", bad)
				}
			}
		})
	}
}

func TestGhActionCmd_ForceOverwrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	_, outPath := setupGhActionTest(t)

	require.NoError(t, os.MkdirAll(filepath.Dir(outPath), 0o755), "failed to create directory")
	require.NoError(t, os.WriteFile(outPath, []byte("existing content"), 0o644), "failed to write existing content")

	// Without --force should fail
	cmd := ghActionCmd()
	cmd.SetArgs([]string{"--output", outPath})
	err := cmd.Execute()
	require.Error(t, err, "expected error without --force flag")

	content, _ := os.ReadFile(outPath)
	assert.Equal(t, "existing content", string(content), "original content should be preserved")

	// With --force should succeed
	cmd2 := ghActionCmd()
	cmd2.SetArgs([]string{"--output", outPath, "--force"})
	require.NoError(t, cmd2.Execute(), "force should succeed")

	content, _ = os.ReadFile(outPath)
	assert.Contains(t, string(content), "name: roborev", "force should have overwritten with workflow")
}

func TestGhActionCmd_NotGitRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	require.NoError(t, os.Chdir(tmpDir), "failed to change directory")
	defer func() {
		require.NoError(t, os.Chdir(origDir), "failed to restore original directory")
	}()

	cmd := ghActionCmd()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	require.Error(t, err, "expected error outside git repo")
	require.ErrorContains(t, err, "not a git repository", "expected 'not a git repository' error, got: %v", err)
}

func TestResolveWorkflowConfig(t *testing.T) {
	tests := []struct {
		name      string
		agentFlag string
		repoCfg   *config.RepoConfig
		globalCfg *config.Config
		want      []string
	}{
		{
			name:      "flag overrides all",
			agentFlag: "codex,gemini",
			repoCfg: &config.RepoConfig{
				Agent: "claude-code",
				CI:    config.RepoCIConfig{Agents: []string{"cursor"}},
			},
			globalCfg: &config.Config{
				DefaultAgent: "droid",
				CI:           config.CIConfig{Agents: []string{"kiro"}},
			},
			want: []string{"codex", "gemini"},
		},
		{
			name: "repo ci agents override repo agent",
			repoCfg: &config.RepoConfig{
				Agent: "claude-code",
				CI:    config.RepoCIConfig{Agents: []string{"gemini"}},
			},
			want: []string{"gemini"},
		},
		{
			name: "repo agent overrides global ci agents",
			repoCfg: &config.RepoConfig{
				Agent: "claude-code",
			},
			globalCfg: &config.Config{
				CI: config.CIConfig{Agents: []string{"gemini"}},
			},
			want: []string{"claude-code"},
		},
		{
			name: "global ci agents override global default agent",
			globalCfg: &config.Config{
				DefaultAgent: "claude-code",
				CI:           config.CIConfig{Agents: []string{"gemini"}},
			},
			want: []string{"gemini"},
		},
		{
			name: "global default agent beats built-in default",
			globalCfg: &config.Config{
				DefaultAgent: "claude-code",
			},
			want: []string{"claude-code"},
		},
		{
			name: "falls back to built-in default",
			want: []string{"codex"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := config.ResolveCIWorkflowAgents(
				tt.agentFlag, tt.repoCfg, tt.globalCfg)
			assert.Equal(t, tt.want, got)
		})
	}
}
