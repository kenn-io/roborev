package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validReviewFlow = `digraph roborev_brief {
  node [shape=box];
  start [shape=oval label="Review findings"];
  check [label="Check sources"];
  back [shape=oval label="Back to roborev_triage"];
  start -> check -> back;
}`

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
	writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\n"+validReviewFlow+"\n'''\n")
	cfg, err := LoadRepoConfig(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, validReviewFlow, strings.TrimSpace(cfg.AgentHook.ReviewFlow))
	writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\nnot dot\n'''\n")
	_, err = LoadRepoConfig(dir)
	require.ErrorContains(t, err, "agent_hook.review_flow")
}

func TestLoadAgentHookReviewFlowNormalizesCRLF(t *testing.T) {
	dir := newFlowRepo(t)
	crlfFlow := strings.ReplaceAll(validReviewFlow, "\n", "\r\n")
	writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\n"+crlfFlow+"\n'''\n")
	commitFlowConfig(t, dir)
	fetchFlowMain(t, dir)

	got, err := LoadAgentHookReviewFlow(t.Context(), dir)

	require.NoError(t, err)
	assert.Equal(t, validReviewFlow, got.Text)
}

func TestLoadRepoConfigForAgentHookGuidelinesIgnoresWrongTypedReviewFlow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "number", value: "42"},
		{name: "array", value: "[1]"},
		{name: "table", value: "{x = 1}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeRepoConfigStr(t, dir, "review_guidelines = 'Keep the reminder.'\n[agent_hook]\nreview_flow = "+tc.value+"\n")

			cfg, err := LoadRepoConfigForAgentHookGuidelines(dir)

			require.NoError(t, err)
			require.NotNil(t, cfg)
			assert.Equal(t, "Keep the reminder.", cfg.ReviewGuidelines)
			assert.Empty(t, cfg.AgentHook.ReviewFlow)
			_, err = LoadRepoConfig(dir)
			require.Error(t, err)
		})
	}
}

func TestLoadRepoConfigFromRefForAgentHookGuidelinesIgnoresWrongTypedReviewFlow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "number", value: "42"},
		{name: "array", value: "[1]"},
		{name: "table", value: "{x = 1}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newFlowRepo(t)
			writeRepoConfigStr(t, dir, "review_guidelines = 'Keep the reminder.'\n[agent_hook]\nreview_flow = "+tc.value+"\n")
			commitFlowConfig(t, dir)
			fetchFlowMain(t, dir)
			ref := "refs/remotes/origin/main"

			cfg, err := LoadRepoConfigFromRefForAgentHookGuidelines(t.Context(), dir, ref)

			require.NoError(t, err)
			require.NotNil(t, cfg)
			assert.Equal(t, "Keep the reminder.", cfg.ReviewGuidelines)
			assert.Empty(t, cfg.AgentHook.ReviewFlow)
			_, err = LoadRepoConfigFromRef(dir, ref)
			require.Error(t, err)
			_, err = LoadAgentHookReviewFlow(t.Context(), dir)
			require.Error(t, err)
		})
	}
}

func TestLoadRepoConfigForAgentHookGuidelinesKeepsOtherMalformedConfigInvalid(t *testing.T) {
	dir := t.TempDir()
	writeRepoConfigStr(t, dir, "review_guidelines = 'Keep the reminder.'\nagent_hook = 42\n")

	_, err := LoadRepoConfigForAgentHookGuidelines(dir)

	require.Error(t, err)
}

func TestUnknownRepoAgentHookKeys(t *testing.T) {
	dir := t.TempDir()
	writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\n"+validReviewFlow+"\n'''\n")
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
				writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\n"+validReviewFlow+"\n'''\n")
				commitFlowConfig(t, dir)
			}
			if mode == "invalid default" {
				writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\nnot dot\n'''\n")
				commitFlowConfig(t, dir)
			}
			if mode != "local main" {
				fetchFlowMain(t, dir)
			}
			other := strings.Replace(validReviewFlow, "roborev_brief", "roborev_other", 1)
			if mode == "default wins" || mode == "working fallback" || mode == "invalid default" {
				writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\n"+other+"\n'''\n")
			}
			if mode == "feature cannot add" || mode == "shadow ref" {
				branch := "feature"
				if mode == "shadow ref" {
					branch = "origin/main"
				}
				execGit(t, dir, "checkout", "-b", branch)
				writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\n"+other+"\n'''\n")
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

func TestLoadAgentHookReviewFlowPreservesLocalOriginPrefixedDefault(t *testing.T) {
	dir := newFlowRepo(t)
	writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\n"+validReviewFlow+"\n'''\n")
	commitFlowConfig(t, dir)
	trustedRevision := execGit(t, dir, "rev-parse", "HEAD")
	execGit(t, dir, "branch", "origin/main")

	execGit(t, dir, "checkout", "-b", "unrelated")
	unrelatedFlow := strings.Replace(validReviewFlow, "roborev_brief", "roborev_unrelated", 1)
	writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\n"+unrelatedFlow+"\n'''\n")
	commitFlowConfig(t, dir)
	unrelatedRevision := execGit(t, dir, "rev-parse", "HEAD")
	execGit(t, dir, "update-ref", "refs/remotes/origin/main", unrelatedRevision)
	execGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/origin/main")

	ref, revision, err := ResolveDefaultBranchRevision(t.Context(), dir)
	require.NoError(t, err)
	assert.Equal(t, "origin/main", ref)
	assert.Equal(t, trustedRevision, revision)

	flow, err := LoadAgentHookReviewFlow(t.Context(), dir)
	require.NoError(t, err)
	assert.Equal(t, validReviewFlow, flow.Text)
}

func TestLoadAgentHookReviewFlowEmptyPathAndCancellation(t *testing.T) {
	got, err := LoadAgentHookReviewFlow(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, got.Text)
	dir := newFlowRepo(t)
	writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\n"+validReviewFlow+"\n'''\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = LoadAgentHookReviewFlow(ctx, dir)
	require.ErrorIs(t, err, context.Canceled)
}

func TestLoadRepoConfigFromRefForAgentHookGuidelinesCancellation(t *testing.T) {
	dir := newFlowRepo(t)
	writeRepoConfigStr(t, dir, "[agent_hook]\nreview_flow = '''\n"+validReviewFlow+"\n'''\n")
	commitFlowConfig(t, dir)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	cfg, err := LoadRepoConfigFromRefForAgentHookGuidelines(ctx, dir, "HEAD")

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, cfg)
}

func TestAgentHookReviewFlowFenceRejectionProperty(t *testing.T) {
	property := func(prefix string, backticks bool) bool {
		fence := "~~~"
		if backticks {
			fence = "```"
		}
		return assert.ErrorContains(t, ValidateAgentHookReviewFlow(prefix+fence+validReviewFlow), "fence")
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}
