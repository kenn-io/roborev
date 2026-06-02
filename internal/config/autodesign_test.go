package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// plantConflictingRepoConfig writes a .roborev.toml in a temp dir, chdirs into
// it, and returns nothing. Its sole purpose is to prove the *FromConfig
// resolvers ignore the working tree: anything they read from the planted file
// would corrupt the assertions that follow (F12).
func plantConflictingRepoConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte(body), 0o644))
	t.Chdir(dir)
}

func TestAutoDesignResolversUseProvidedConfig(t *testing.T) {
	enabled := true
	repoCfg := &config.RepoConfig{AutoDesignReview: config.AutoDesignReviewRepoConfig{Enabled: &enabled}}
	global := &config.Config{}
	// Plant a conflicting working-tree config that must be IGNORED.
	plantConflictingRepoConfig(t, "[auto_design_review]\nenabled = false\n")
	assert.True(t, config.AutoDesignEnabledFromConfig(repoCfg, global))
}

// TestAutoDesignEnabledFromConfigTriState exercises the tri-state precedence
// directly against the passed config: an explicit per-repo value (true or
// false) overrides the global default; an unset per-repo field falls through.
func TestAutoDesignEnabledFromConfigTriState(t *testing.T) {
	assert := assert.New(t)
	on, off := true, false

	// Repo explicit true overrides global-disabled.
	assert.True(config.AutoDesignEnabledFromConfig(
		&config.RepoConfig{AutoDesignReview: config.AutoDesignReviewRepoConfig{Enabled: &on}},
		&config.Config{}))
	// Repo explicit false overrides global-enabled.
	assert.False(config.AutoDesignEnabledFromConfig(
		&config.RepoConfig{AutoDesignReview: config.AutoDesignReviewRepoConfig{Enabled: &off}},
		&config.Config{AutoDesignReview: config.AutoDesignReviewConfig{Enabled: true}}))
	// Repo unset falls through to global.
	assert.True(config.AutoDesignEnabledFromConfig(
		&config.RepoConfig{},
		&config.Config{AutoDesignReview: config.AutoDesignReviewConfig{Enabled: true}}))
	assert.False(config.AutoDesignEnabledFromConfig(&config.RepoConfig{}, &config.Config{}))
	// Nil repoCfg falls through to global.
	assert.True(config.AutoDesignEnabledFromConfig(
		nil, &config.Config{AutoDesignReview: config.AutoDesignReviewConfig{Enabled: true}}))
	assert.False(config.AutoDesignEnabledFromConfig(nil, nil))
}

func TestAutoDesignHeuristicsFromConfigUsesProvidedConfig(t *testing.T) {
	assert := assert.New(t)
	// Repo overrides one scalar, global overrides another; defaults fill the rest.
	repoCfg := &config.RepoConfig{AutoDesignReview: config.AutoDesignReviewRepoConfig{
		LargeFileCount: 3,
	}}
	global := &config.Config{AutoDesignReview: config.AutoDesignReviewConfig{
		MinDiffLines:   1,
		LargeFileCount: 99, // repo must win over this
	}}
	// A conflicting cwd config must be ignored, not overlaid.
	plantConflictingRepoConfig(t, "[auto_design_review]\nlarge_file_count = 777\nmin_diff_lines = 555\n")

	h := config.AutoDesignHeuristicsFromConfig(repoCfg, global)
	assert.Equal(3, h.LargeFileCount, "repo overlay wins over global and defaults")
	assert.Equal(1, h.MinDiffLines, "global overlay applies where repo is unset")
	assert.Equal(config.DefaultAutoDesignHeuristics().LargeDiffLines, h.LargeDiffLines,
		"unset fields keep the default")
}

func TestAutoDesignHeuristicsFromConfigNilsUseDefaults(t *testing.T) {
	assert := assert.New(t)
	assert.Equal(config.DefaultAutoDesignHeuristics(), config.AutoDesignHeuristicsFromConfig(nil, nil))
}

func TestDesignAgentFromConfigUsesProvidedConfig(t *testing.T) {
	assert := assert.New(t)
	// Pin a design-workflow agent and model on the passed repo config via the
	// TOML-tagged fields, and prove they flow through with no availability check.
	repoCfg := &config.RepoConfig{DesignAgent: "claude-code", DesignModel: "sonnet"}
	// A conflicting cwd config must be ignored.
	plantConflictingRepoConfig(t, "design_agent = \"evil\"\ndesign_model = \"evil-model\"\n")

	agent, model := config.DesignAgentFromConfig(repoCfg, &config.Config{})
	assert.Equal("claude-code", agent)
	assert.Equal("sonnet", model)
}

// TestDesignAgentFromConfigExplicitAgentSkipsGenericModel mirrors the panel
// member nuance: an explicit design agent inherits only a workflow-specific
// design model, never the generic global default_model paired with a different
// default agent. No design_model is configured, so the model stays empty.
func TestDesignAgentFromConfigExplicitAgentSkipsGenericModel(t *testing.T) {
	assert := assert.New(t)
	repoCfg := &config.RepoConfig{DesignAgent: "claude-code"}
	global := &config.Config{DefaultAgent: "codex", DefaultModel: "generic-default"}

	agent, model := config.DesignAgentFromConfig(repoCfg, global)
	assert.Equal("claude-code", agent)
	assert.NotEqual("generic-default", model, "pinned agent must not inherit foreign generic model")
	assert.Empty(model)
}

// TestResolveAutoDesignEnabledDelegates confirms the public repoPath-taking
// resolver still behaves after being rewritten to delegate: with an empty repo
// (no .roborev.toml) it falls through to the global value.
func TestResolveAutoDesignEnabledDelegates(t *testing.T) {
	assert := assert.New(t)
	repoDir := t.TempDir()
	assert.True(config.ResolveAutoDesignEnabled(repoDir,
		&config.Config{AutoDesignReview: config.AutoDesignReviewConfig{Enabled: true}}))
	assert.False(config.ResolveAutoDesignEnabled(repoDir, &config.Config{}))
}
