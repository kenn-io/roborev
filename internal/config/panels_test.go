package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeGlobalConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func TestReviewConfigParsesAlongsideFlatKeys(t *testing.T) {
	assert := assert.New(t)
	path := writeGlobalConfig(t, `
review_agent = "codex"

[review]
default_panel = "branch_final"
hook_review_panel = "quick"

[review.subagents.default]
agent = "codex"
model = "gpt-5.5"
review_type = "default"

[review.subagents.security]
agent = "codex"
review_type = "security"
reasoning = "thorough"
instructions = "Focus on authz."
allow_failure = true
timeout = "3m"

[review.panels.quick]
members = ["default"]

[review.panels.branch_final]
members = ["default", "security"]
synthesis_agent = "codex"
synthesis_model = "gpt-5.5"
`)
	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)

	// Flat key still works.
	assert.Equal("codex", cfg.ReviewAgent)
	// Table parsed.
	assert.Equal("branch_final", cfg.Review.DefaultPanel)
	assert.Equal("quick", cfg.Review.HookPanel)
	assert.Equal("codex", cfg.Review.Subagents["security"].Agent)
	assert.Equal("security", cfg.Review.Subagents["security"].ReviewType)
	assert.Equal("thorough", cfg.Review.Subagents["security"].Reasoning)
	assert.Equal("Focus on authz.", cfg.Review.Subagents["security"].Instructions)
	assert.True(cfg.Review.Subagents["security"].AllowFailure)
	assert.Equal("3m", cfg.Review.Subagents["security"].Timeout)
	assert.Equal([]string{"default", "security"}, cfg.Review.Panels["branch_final"].Members)
	assert.Equal("codex", cfg.Review.Panels["branch_final"].SynthesisAgent)
	assert.Equal("gpt-5.5", cfg.Review.Panels["branch_final"].SynthesisModel)
}

func TestMergeReviewConfig(t *testing.T) {
	assert := assert.New(t)
	global := ReviewConfig{
		DefaultPanel: "g_default",
		HookPanel:    "g_hook",
		Subagents: map[string]SubagentSpec{
			"default":  {Agent: "codex"},
			"security": {Agent: "codex", ReviewType: "security"},
		},
		Panels: map[string]PanelSpec{
			"quick":  {Members: []string{"default"}},
			"shared": {Members: []string{"default", "security"}},
		},
	}
	repo := ReviewConfig{
		DefaultPanel: "r_default", // repo overrides global scalar
		// HookPanel empty -> inherit global
		Subagents: map[string]SubagentSpec{
			"security": {Agent: "claude-code", ReviewType: "security"}, // overrides global key
			"design":   {Agent: "claude-code", ReviewType: "design"},   // repo-only
		},
		Panels: map[string]PanelSpec{
			"shared": {Members: []string{"design"}}, // overrides global key
		},
	}
	merged := MergeReviewConfig(repo, global)

	assert.Equal("r_default", merged.DefaultPanel)
	assert.Equal("g_hook", merged.HookPanel)
	// Subagents: union, repo wins on conflict.
	assert.Equal("codex", merged.Subagents["default"].Agent)        // global-only
	assert.Equal("claude-code", merged.Subagents["security"].Agent) // repo override
	assert.Equal("claude-code", merged.Subagents["design"].Agent)   // repo-only
	// Panels: union, repo wins on conflict.
	assert.Equal([]string{"default"}, merged.Panels["quick"].Members) // global-only
	assert.Equal([]string{"design"}, merged.Panels["shared"].Members) // repo override
	// Merge does not mutate inputs (size unchanged and no in-place overwrite).
	assert.Equal("g_default", global.DefaultPanel)
	assert.Len(global.Subagents, 2)
	assert.Equal("codex", global.Subagents["security"].Agent)
}

func TestMergedReviewConfigEmptyRepoPathIgnoresCwd(t *testing.T) {
	// A .roborev.toml in the process working directory must not leak into a
	// global-only (empty repoPath) merge. Regression test: MergedReviewConfig
	// previously called LoadRepoConfig(""), which stats ".roborev.toml" in CWD.
	assert := assert.New(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"),
		[]byte("[review]\n\n[review.panels.cwd_only]\nmembers = [\"x\"]\n"), 0o644))
	t.Chdir(dir)

	global := &Config{Review: ReviewConfig{
		Panels: map[string]PanelSpec{"global_only": {Members: []string{"x"}}},
	}}
	merged := MergedReviewConfig("", global)

	_, leaked := merged.Panels["cwd_only"]
	assert.False(leaked, "empty repoPath must not read .roborev.toml from the CWD")
	_, hasGlobal := merged.Panels["global_only"]
	assert.True(hasGlobal, "global panels should be present")
}

func TestReviewConfigValidate(t *testing.T) {
	assert := assert.New(t)

	valid := ReviewConfig{
		DefaultPanel: "quick",
		Subagents:    map[string]SubagentSpec{"default": {Agent: "codex"}},
		Panels:       map[string]PanelSpec{"quick": {Members: []string{"default"}}},
	}
	assert.NoError(valid.Validate())

	// Empty config is valid (single-agent default).
	assert.NoError(ReviewConfig{}.Validate())

	bad := ReviewConfig{
		DefaultPanel: "missing_panel",
		HookPanel:    "empty_panel",
		Subagents:    map[string]SubagentSpec{"default": {Agent: "codex"}},
		Panels: map[string]PanelSpec{
			"empty_panel": {Members: nil},
			"typo_panel":  {Members: []string{"default", "secrity"}},
		},
	}
	err := bad.Validate()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(msg, `default_panel "missing_panel" is not a defined panel`)
	assert.Contains(msg, `panel "empty_panel" has no members`)
	assert.Contains(msg, `panel "typo_panel" references undefined subagent "secrity"`)
	// hook_review_panel "empty_panel" IS defined (just empty), so no "not a defined panel" error for it.
	assert.NotContains(msg, `hook_review_panel "empty_panel" is not a defined panel`)
}

func TestSelectPanelName(t *testing.T) {
	assert := assert.New(t)
	merged := ReviewConfig{DefaultPanel: "branch_final", HookPanel: "quick"}

	// Explicit request wins in any context.
	assert.Equal("security_only", SelectPanelName("security_only", "", merged))
	assert.Equal("security_only", SelectPanelName("security_only", "post_commit", merged))

	// "none" forces single-agent anywhere.
	assert.Empty(SelectPanelName(PanelNone, "", merged))
	assert.Empty(SelectPanelName(PanelNone, "post_commit", merged))

	// Foreground (empty source) uses default_panel.
	assert.Equal("branch_final", SelectPanelName("", "", merged))
	// Automatic (non-empty source) uses hook_review_panel; default_panel is never consulted.
	assert.Equal("quick", SelectPanelName("", "post_commit", merged))

	// Unset defaults -> single-agent.
	empty := ReviewConfig{}
	assert.Empty(SelectPanelName("", "", empty))
	assert.Empty(SelectPanelName("", "post_commit", empty))
}

func TestResolvePanel(t *testing.T) {
	assert := assert.New(t)
	global := &Config{
		// Workflow fallback the member resolution should pick up when a
		// subagent omits agent/model.
		ReviewAgent: "codex",
		ReviewModel: "gpt-5.5",
		Review: ReviewConfig{
			Subagents: map[string]SubagentSpec{
				"default": {ReviewType: "default"}, // omits agent/model -> workflow fallback
				"security": {
					Agent:        "claude-code",
					Model:        "sonnet",
					ReviewType:   "security",
					Reasoning:    "thorough",
					Instructions: "Focus on authz.",
					AllowFailure: true,
					Timeout:      "3m",
				},
			},
			Panels: map[string]PanelSpec{
				"branch_final": {
					Members:        []string{"default", "security"},
					SynthesisAgent: "codex",
					SynthesisModel: "gpt-5.5",
				},
				"defaults_only": {Members: []string{"default"}},
				"empty":         {Members: nil},
				"typo":          {Members: []string{"missing"}},
			},
		},
	}

	members, synth, err := ResolvePanel("branch_final", "", global)
	require.NoError(t, err)
	require.Len(t, members, 2)

	// Member 0: default — agent/model from the review-workflow fallback.
	// Reasoning, omitted on the spec, inherits the review default (thorough).
	assert.Equal("default", members[0].Name)
	assert.Equal(0, members[0].Index)
	assert.Equal("codex", members[0].Agent)
	assert.Equal("gpt-5.5", members[0].Model)
	assert.Equal("default", members[0].ReviewType)
	assert.Equal("thorough", members[0].Reasoning)

	// Member 1: security — explicit fields preserved, review_type canonical.
	assert.Equal("security", members[1].Name)
	assert.Equal(1, members[1].Index)
	assert.Equal("claude-code", members[1].Agent)
	assert.Equal("sonnet", members[1].Model)
	assert.Equal("security", members[1].ReviewType)
	assert.Equal("thorough", members[1].Reasoning)
	assert.Equal("Focus on authz.", members[1].Instructions)
	assert.True(members[1].AllowFailure)
	assert.Equal("3m", members[1].Timeout)

	// Synthesis from the panel's explicit fields; reasoning is the fix default.
	assert.Equal("codex", synth.Agent)
	assert.Equal("gpt-5.5", synth.Model)
	assert.Equal("standard", synth.Reasoning)

	// Synthesis fallback to the fix workflow when the panel omits it.
	global.FixAgent = "codex"
	global.FixModel = "gpt-5.5-fix"
	_, synth2, err := ResolvePanel("defaults_only", "", global)
	require.NoError(t, err)
	assert.Equal("codex", synth2.Agent)
	assert.Equal("gpt-5.5-fix", synth2.Model)
	assert.Equal("standard", synth2.Reasoning)

	// Errors.
	_, _, err = ResolvePanel("does_not_exist", "", global)
	require.ErrorContains(t, err, `panel "does_not_exist" is not defined`)
	_, _, err = ResolvePanel("empty", "", global)
	require.ErrorContains(t, err, `panel "empty" has no members`)
	_, _, err = ResolvePanel("typo", "", global)
	require.ErrorContains(t, err, `references undefined subagent "missing"`)
}

func TestResolvePanelRejectsInvalidSubagentTimeout(t *testing.T) {
	global := &Config{Review: ReviewConfig{
		Subagents: map[string]SubagentSpec{
			"bad": {Agent: "codex", Timeout: "0"},
		},
		Panels: map[string]PanelSpec{"p": {Members: []string{"bad"}}},
	}}

	_, _, err := ResolvePanel("p", "", global)
	require.ErrorContains(t, err, `subagent "bad": invalid timeout`)
}

// TestResolvePanelExplicitAgentSkipsGenericModel verifies A3: a member that
// pins an explicit agent but omits the model inherits only a workflow-specific
// model, never the generic global default_model (which is paired with a
// different default agent). The design workflow has no design_model here, so
// the omitted-model resolution must yield "" rather than the foreign generic.
func TestResolvePanelExplicitAgentSkipsGenericModel(t *testing.T) {
	assert := assert.New(t)
	global := &Config{
		// Generic defaults paired with a different default agent. A member that
		// pins claude-code must NOT pick up this codex-paired generic model.
		DefaultAgent: "codex",
		DefaultModel: "generic-default",
		Review: ReviewConfig{
			Subagents: map[string]SubagentSpec{
				// Explicit agent, omitted model, design review type. No
				// design_model is configured, so ResolveWorkflowModel yields "".
				"design": {Agent: "claude-code", ReviewType: "design"},
			},
			Panels: map[string]PanelSpec{
				"p": {Members: []string{"design"}},
			},
		},
	}

	members, _, err := ResolvePanel("p", "", global)
	require.NoError(t, err)
	require.Len(t, members, 1)

	m := members[0]
	assert.Equal("claude-code", m.Agent)
	assert.Equal("design", m.ReviewType)
	// The pinned agent must not inherit the foreign generic default_model.
	assert.NotEqual("generic-default", m.Model)
	// No workflow-specific design_model exists, so the model stays empty.
	assert.Empty(m.Model)
}

// TestResolvePanelExplicitSynthesisAgentSkipsGenericModel verifies the same A3
// pairing rule for synthesis: a panel that pins synthesis_agent but omits
// synthesis_model inherits only a workflow-specific fix model, never the generic
// global default_model paired with a different default agent. No fix_model is
// configured here, so the omitted-model resolution must yield "".
func TestResolvePanelExplicitSynthesisAgentSkipsGenericModel(t *testing.T) {
	assert := assert.New(t)
	global := &Config{
		DefaultAgent: "codex",
		DefaultModel: "generic-default",
		Review: ReviewConfig{
			Subagents: map[string]SubagentSpec{
				"default": {ReviewType: "default"},
			},
			Panels: map[string]PanelSpec{
				// Explicit synthesis agent, omitted synthesis model.
				"p": {Members: []string{"default"}, SynthesisAgent: "claude-code"},
			},
		},
	}

	_, synth, err := ResolvePanel("p", "", global)
	require.NoError(t, err)

	assert.Equal("claude-code", synth.Agent)
	// The pinned synthesis agent must not inherit the foreign generic default_model.
	assert.NotEqual("generic-default", synth.Model)
	// No workflow-specific fix_model exists, so the model stays empty.
	assert.Empty(synth.Model)
}

// TestResolveSynthesisBackupPassThrough verifies F7: synthesis_backup_agent /
// synthesis_backup_model are passed straight through to SynthesisSpec.BackupAgent
// / BackupModel with no resolution or fallback, via BOTH ResolvePanel and
// ResolveCIPanel. A panel that omits them yields empty backup fields.
func TestResolveSynthesisBackupPassThrough(t *testing.T) {
	assert := assert.New(t)
	makeConfig := func() *Config {
		return &Config{
			ReviewAgent: "codex",
			Review: ReviewConfig{
				Subagents: map[string]SubagentSpec{"default": {ReviewType: "default"}},
				Panels: map[string]PanelSpec{
					"with_backup": {
						Members:              []string{"default"},
						SynthesisAgent:       "codex",
						SynthesisBackupAgent: "claude-code",
						SynthesisBackupModel: "opus",
					},
					"no_backup": {Members: []string{"default"}, SynthesisAgent: "codex"},
				},
			},
		}
	}

	// ResolvePanel: explicit backup fields pass through verbatim.
	_, synth, err := ResolvePanel("with_backup", "", makeConfig())
	require.NoError(t, err)
	assert.Equal("claude-code", synth.BackupAgent)
	assert.Equal("opus", synth.BackupModel)

	// ResolvePanel: omitted backup fields yield empty backups (no fallback).
	_, synthNone, err := ResolvePanel("no_backup", "", makeConfig())
	require.NoError(t, err)
	assert.Empty(synthNone.BackupAgent)
	assert.Empty(synthNone.BackupModel)

	// ResolveCIPanel: same pass-through from the passed repo config.
	repoCfg := &RepoConfig{Review: makeConfig().Review}
	_, ciSynth, err := ResolveCIPanel("with_backup", repoCfg, &Config{})
	require.NoError(t, err)
	assert.Equal("claude-code", ciSynth.BackupAgent)
	assert.Equal("opus", ciSynth.BackupModel)

	_, ciSynthNone, err := ResolveCIPanel("no_backup", repoCfg, &Config{})
	require.NoError(t, err)
	assert.Empty(ciSynthNone.BackupAgent)
	assert.Empty(ciSynthNone.BackupModel)
}

func TestResolveCIPanelUsesProvidedConfigNotWorkingTree(t *testing.T) {
	repoCfg := &RepoConfig{Review: ReviewConfig{
		Panels:    map[string]PanelSpec{"ci": {Members: []string{"a"}, SynthesisAgent: "codex"}},
		Subagents: map[string]SubagentSpec{"a": {ReviewType: "review"}},
	}}
	global := &Config{}
	planted := filepath.Join(t.TempDir(), ".roborev.toml")
	require.NoError(t, os.WriteFile(planted,
		[]byte("[review.panels.ci]\nmembers = [\"evil\"]\n"), 0o644))
	// Run from the planted dir to prove the working tree is ignored.
	t.Chdir(filepath.Dir(planted))
	members, synth, err := ResolveCIPanel("ci", repoCfg, global)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, "a", members[0].Name)
	assert.Equal(t, "codex", synth.Agent)
}

func TestResolveCIPanelUndefinedPanelErrors(t *testing.T) {
	// Error parity with ResolvePanel: a panel name that is not defined in the
	// passed config is a hard error. The matrix-fallback path (no panel name ->
	// agents x review_types) is NOT implemented here; it lives in C2.
	repoCfg := &RepoConfig{Review: ReviewConfig{
		Panels:    map[string]PanelSpec{"ci": {Members: []string{"a"}}},
		Subagents: map[string]SubagentSpec{"a": {ReviewType: "review"}},
	}}
	_, _, err := ResolveCIPanel("missing", repoCfg, &Config{})
	require.ErrorContains(t, err, `panel "missing" is not defined`)

	// No-members and undefined-subagent parity.
	noMembers := &RepoConfig{Review: ReviewConfig{
		Panels: map[string]PanelSpec{"ci": {Members: nil}},
	}}
	_, _, err = ResolveCIPanel("ci", noMembers, &Config{})
	require.ErrorContains(t, err, `panel "ci" has no members`)

	badRef := &RepoConfig{Review: ReviewConfig{
		Panels: map[string]PanelSpec{"ci": {Members: []string{"ghost"}}},
	}}
	_, _, err = ResolveCIPanel("ci", badRef, &Config{})
	require.ErrorContains(t, err, `references undefined subagent "ghost"`)
}

func TestResolveCIPanelMemberInheritsReviewReasoning(t *testing.T) {
	// §5.3: a member whose SubagentSpec omits reasoning inherits the review
	// reasoning resolved from the PASSED repoCfg/globalCfg, proving resolution
	// is config-driven. "fast" is a non-default review reasoning (the default
	// is "thorough"), so seeing it on the member proves it flowed from config.
	assert := assert.New(t)
	repoCfg := &RepoConfig{
		ReviewReasoning: "fast",
		Review: ReviewConfig{
			Panels:    map[string]PanelSpec{"ci": {Members: []string{"a"}}},
			Subagents: map[string]SubagentSpec{"a": {ReviewType: "review"}},
		},
	}
	members, _, err := ResolveCIPanel("ci", repoCfg, &Config{})
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal("fast", members[0].Reasoning)

	// With no repo override, the global review reasoning flows through instead.
	repoCfg.ReviewReasoning = ""
	global := &Config{ReviewReasoning: "medium"}
	members, _, err = ResolveCIPanel("ci", repoCfg, global)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal("medium", members[0].Reasoning)
}

func TestResolveCIPanelReasoningOverridePolicyUsesPassedConfig(t *testing.T) {
	// The FromConfig reasoning resolver validates the repo override field on the
	// PASSED repoCfg, not the working tree: when a member pins an explicit
	// (valid) reasoning AND the passed repoCfg carries an invalid review_reasoning
	// override, resolution errors. A conflicting valid .roborev.toml in cwd must
	// not rescue it.
	repoCfg := &RepoConfig{
		ReviewReasoning: "bogus", // invalid override on the passed config
		Review: ReviewConfig{
			Panels:    map[string]PanelSpec{"ci": {Members: []string{"a"}}},
			Subagents: map[string]SubagentSpec{"a": {ReviewType: "review", Reasoning: "fast"}},
		},
	}
	planted := filepath.Join(t.TempDir(), ".roborev.toml")
	require.NoError(t, os.WriteFile(planted,
		[]byte("review_reasoning = \"thorough\"\n"), 0o644))
	t.Chdir(filepath.Dir(planted))

	_, _, err := ResolveCIPanel("ci", repoCfg, &Config{})
	require.Error(t, err)
	require.ErrorContains(t, err, "invalid reasoning level")
}

func TestResolveCISynthesis(t *testing.T) {
	assert := assert.New(t)

	globalCI := &Config{
		FixAgent: "gemini",
		FixModel: "gemini-fix",
		CI: CIConfig{
			SynthesisAgent:       "codex",
			SynthesisModel:       "gpt-5.5",
			SynthesisBackupAgent: "claude-code",
		},
	}
	ciSynth, err := ResolveCISynthesis("thorough", &RepoConfig{}, globalCI)
	require.NoError(t, err)
	assert.Equal("codex", ciSynth.Agent)
	assert.Equal("gpt-5.5", ciSynth.Model)
	assert.Equal("thorough", ciSynth.Reasoning)
	assert.Equal("claude-code", ciSynth.BackupAgent)

	// Agent/model resolve from the fix workflow off the passed config; the
	// reasoning is overridden to the supplied CI reasoning, not the fix default.
	repoCfg := &RepoConfig{FixAgent: "claude-code", FixModel: "opus-fix"}
	synth, err := ResolveCISynthesis("fast", repoCfg, &Config{})
	require.NoError(t, err)
	assert.Equal("claude-code", synth.Agent)
	assert.Equal("opus-fix", synth.Model)
	assert.Equal("fast", synth.Reasoning)
	// The matrix path expresses no synthesis backup.
	assert.Empty(synth.BackupAgent)
	assert.Empty(synth.BackupModel)

	// The fix workflow may also come from the global config.
	globalCfg := &Config{FixAgent: "gemini", FixModel: "gemini-fix"}
	gSynth, err := ResolveCISynthesis("thorough", &RepoConfig{}, globalCfg)
	require.NoError(t, err)
	assert.Equal("gemini", gSynth.Agent)
	assert.Equal("gemini-fix", gSynth.Model)
	assert.Equal("thorough", gSynth.Reasoning)

	// F1: a conflicting working-tree .roborev.toml in cwd is ignored; the result
	// comes from the passed config, not the planted file.
	planted := filepath.Join(t.TempDir(), ".roborev.toml")
	require.NoError(t, os.WriteFile(planted,
		[]byte("fix_agent = \"evil\"\nfix_model = \"evil-model\"\n"), 0o644))
	t.Chdir(filepath.Dir(planted))
	f1Synth, err := ResolveCISynthesis("fast", repoCfg, &Config{})
	require.NoError(t, err)
	assert.Equal("claude-code", f1Synth.Agent)
	assert.Equal("opus-fix", f1Synth.Model)

	// Nil repoCfg must not panic.
	nilSynth, err := ResolveCISynthesis("thorough", nil, &Config{})
	require.NoError(t, err)
	assert.Equal("thorough", nilSynth.Reasoning)
}

func TestWorkflowForReviewType(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("review", WorkflowForReviewType("default"))
	assert.Equal("review", WorkflowForReviewType(""))
	assert.Equal("security", WorkflowForReviewType("security"))
	assert.Equal("design", WorkflowForReviewType("design"))
	assert.Equal("lookahead", WorkflowForReviewType("lookahead"))
}

func TestResolvePanelRejectsInvalidReviewType(t *testing.T) {
	global := &Config{Review: ReviewConfig{
		Subagents: map[string]SubagentSpec{"bad": {ReviewType: "bogus"}},
		Panels:    map[string]PanelSpec{"p": {Members: []string{"bad"}}},
	}}
	_, _, err := ResolvePanel("p", "", global)
	assert.ErrorContains(t, err, "invalid review_type")
}
