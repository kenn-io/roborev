package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

const validReviewFlow = `digraph roborev_brief {
  node [shape=box];
  start [shape=oval label="Review findings"];
  check [label="Check sources"];
  back [shape=oval label="Back to roborev_triage"];
  start -> check -> back;
}`

func writeFlowConfig(t *testing.T, dir, flow string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte("[agent_hook]\nreview_flow = '''\n"+flow+"\n'''\n"), 0o600))
}

func TestValidateAgentHookReviewFlow(t *testing.T) {
	for name, flow := range map[string]string{"empty": "", "space": " \n\t", "valid": validReviewFlow, "crlf": strings.ReplaceAll(validReviewFlow, "\n", "\r\n")} {
		t.Run(name, func(t *testing.T) { require.NoError(t, ValidateAgentHookReviewFlow(flow)) })
	}
	for name, flow := range map[string]string{"tilde": "~~~dot\n" + validReviewFlow + "\n~~~", "backticks": "```\n" + validReviewFlow + "\n```", "reserved": strings.Replace(validReviewFlow, "roborev_brief", "roborev_triage", 1), "prose": "Fix every finding."} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, ValidateAgentHookReviewFlow(flow), "agent_hook.review_flow:")
		})
	}
	t.Run("branching box", func(t *testing.T) {
		flow := strings.Replace(
			validReviewFlow,
			"start -> check -> back;",
			"start -> check -> back; check -> check;",
			1,
		)
		require.NotEqual(t, validReviewFlow, flow)
		require.ErrorContains(t, ValidateAgentHookReviewFlow(flow), "only diamonds may branch")
	})
}

func TestLoadRepoConfigReadsAgentHookReviewFlow(t *testing.T) {
	dir := t.TempDir()
	writeFlowConfig(t, dir, validReviewFlow)
	cfg, err := LoadRepoConfig(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, validReviewFlow, strings.TrimSpace(cfg.AgentHook.ReviewFlow))
	writeFlowConfig(t, dir, "not dot")
	_, err = LoadRepoConfig(dir)
	require.ErrorContains(t, err, "agent_hook.review_flow")
}

func TestUnknownRepoAgentHookKeys(t *testing.T) {
	dir := t.TempDir()
	writeFlowConfig(t, dir, validReviewFlow)
	f, err := os.OpenFile(filepath.Join(dir, ".roborev.toml"), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("instruction = 'custom'\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	keys, err := UnknownRepoKeys(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"agent_hook.instruction"}, keys)
}

func newFlowRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	execGit(t, dir, "init", "-b", "main")
	execGit(t, dir, "config", "user.name", "Test")
	execGit(t, dir, "config", "user.email", "test@example.com")
	execGit(t, dir, "commit", "--allow-empty", "-m", "initial")
	return dir
}

func commitFlowConfig(t *testing.T, dir string) {
	t.Helper()
	execGit(t, dir, "add", ".roborev.toml")
	execGit(t, dir, "commit", "-m", "configure")
}

func fetchFlowMain(t *testing.T, dir string) {
	t.Helper()
	execGit(t, dir, "remote", "add", "origin", dir)
	execGit(t, dir, "fetch", "origin")
	execGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
}

func TestLoadAgentHookReviewFlow(t *testing.T) {
	for _, mode := range []string{"no config", "default wins", "feature cannot add", "working fallback", "invalid default", "local main", "shadow ref"} {
		t.Run(mode, func(t *testing.T) {
			dir := newFlowRepo(t)
			if mode == "feature cannot add" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte("review_guidelines = 'Test'\n"), 0o600))
				commitFlowConfig(t, dir)
			}
			if mode == "default wins" || mode == "local main" || mode == "shadow ref" {
				writeFlowConfig(t, dir, validReviewFlow)
				commitFlowConfig(t, dir)
			}
			if mode == "invalid default" {
				writeFlowConfig(t, dir, "not dot")
				commitFlowConfig(t, dir)
			}
			if mode != "local main" {
				fetchFlowMain(t, dir)
			}
			other := strings.Replace(validReviewFlow, "roborev_brief", "roborev_other", 1)
			if mode == "default wins" || mode == "working fallback" || mode == "invalid default" {
				writeFlowConfig(t, dir, other)
			}
			if mode == "feature cannot add" || mode == "shadow ref" {
				branch := "feature"
				if mode == "shadow ref" {
					branch = "origin/main"
				}
				execGit(t, dir, "checkout", "-b", branch)
				writeFlowConfig(t, dir, other)
				commitFlowConfig(t, dir)
			}
			got, err := LoadAgentHookReviewFlow(t.Context(), dir)
			if mode == "invalid default" {
				require.Error(t, err)
				assert.Empty(t, got.Text)
				return
			}
			require.NoError(t, err)
			switch mode {
			case "no config", "feature cannot add":
				assert.Empty(t, got.Text)
			case "working fallback":
				assert.Equal(t, other, got.Text)
				assert.Empty(t, got.Ref)
			case "local main":
				assert.Equal(t, validReviewFlow, got.Text)
				assert.Equal(t, "main", got.Ref)
			default:
				assert.Equal(t, validReviewFlow, got.Text)
				assert.Equal(t, "origin/main", got.Ref)
			}
		})
	}
}

func TestResolveExcludePatternsUsesPinnedDefaultWhenLocalNameShadows(t *testing.T) {
	dir := newFlowRepo(t)
	configPath := filepath.Join(dir, ".roborev.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(`exclude_patterns = ["trusted.lock"]`), 0o600))
	commitFlowConfig(t, dir)
	fetchFlowMain(t, dir)
	execGit(t, dir, "checkout", "-b", "origin/main")
	require.NoError(t, os.WriteFile(configPath, []byte(`exclude_patterns = ["untrusted.lock"]`), 0o600))
	execGit(t, dir, "add", ".roborev.toml")
	execGit(t, dir, "commit", "-m", "shadow remote default")

	got := ResolveExcludePatterns(t.Context(), dir, nil, "")
	assert.Equal(t, []string{"trusted.lock"}, got)
}

func TestLoadAgentHookReviewFlowEmptyPathAndCancellation(t *testing.T) {
	got, err := LoadAgentHookReviewFlow(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, got.Text)
	dir := newFlowRepo(t)
	writeFlowConfig(t, dir, validReviewFlow)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = LoadAgentHookReviewFlow(ctx, dir)
	require.ErrorIs(t, err, context.Canceled)
}

func TestLoadRepoConfigFromRefForAgentHookGuidelinesCancellation(t *testing.T) {
	dir := newFlowRepo(t)
	writeFlowConfig(t, dir, validReviewFlow)
	commitFlowConfig(t, dir)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	cfg, err := LoadRepoConfigFromRefForAgentHookGuidelines(ctx, dir, "HEAD")

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, cfg)
}

func TestAgentHookReviewFlowFenceRejectionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		prefix := hegel.Draw(ht, hegel.Text())
		fence := hegel.Draw(ht, hegel.SampledFrom([]string{"~~~", "```"}))
		require.ErrorContains(ht, ValidateAgentHookReviewFlow(prefix+fence+validReviewFlow), "fence")
	})
}
