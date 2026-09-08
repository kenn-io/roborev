package config

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectPanelOverrides(t *testing.T) {
	for _, tt := range []struct {
		name      string
		override  bool
		synthesis string
	}{
		{name: "defaults"},
		{name: "members", override: true},
		{name: "synthesis", synthesis: "project-synthesis"},
		{name: "members and synthesis", override: true, synthesis: "project-synthesis"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			var cfg Config
			_, err := toml.Decode(fmt.Sprintf(`
default_agent = "codex"
default_model = "global-model"
[projects."example.com/team/project-a"]
review_model = "project-model"
override_panel_models = %t
synthesis_model = %q
review_reasoning = "medium"
override_panel_reasoning = %t
synthesis_reasoning = "low"
[review.subagents.general]
agent = "codex"
model = "member-model"
reasoning = "high"
[review.subagents.security-check]
agent = "codex"
model = "security-member-model"
reasoning = "high"
review_type = "security"
[review.panels.panel-a]
members = ["general", "security-check"]
synthesis_agent = "codex"
synthesis_model = "synthesis-model"
`, tt.override, tt.synthesis, tt.override), &cfg)
			require.NoError(t, err)
			root := t.TempDir()
			execGit(t, root, "init")
			execGit(t, root, "remote", "add", "origin", "https://example.com/team/project-a.git")
			for _, ci := range []bool{false, true} {
				var members []ResolvedMember
				var synth SynthesisSpec
				if ci {
					members, synth, err = ResolveCIPanel("panel-a", nil, cfg.ForRepo(root))
				} else {
					members, synth, err = ResolvePanel("panel-a", root, &cfg)
				}
				require.NoError(t, err)
				require.Len(t, members, 2)
				if tt.override {
					assert.Equal("project-model", members[0].Model)
					assert.Equal("project-model", members[1].Model)
				} else {
					assert.Equal("member-model", members[0].Model)
					assert.Equal("security-member-model", members[1].Model)
				}
				wantSynthesis := "synthesis-model"
				if tt.synthesis != "" {
					wantSynthesis = tt.synthesis
				}
				assert.Equal(wantSynthesis, synth.Model)
				assert.Equal("low", synth.Reasoning)
				wantReasoning := "high"
				if tt.override {
					wantReasoning = "medium"
				}
				for _, member := range members {
					assert.Equal(wantReasoning, member.Reasoning)
				}
			}
		})
	}
}

func TestProjectReviewModelWorktrees(t *testing.T) {
	var cfg Config
	_, err := toml.Decode(`
review_model = "default-review"
review_model_thorough = "default-thorough"
fix_model = "default-fix"
[projects."example.com/team/project-a"]
review_model = "project-review"
display_name = "Project A"
`, &cfg)
	require.NoError(t, err)

	for _, bare := range []bool{false, true} {
		t.Run(map[bool]string{false: "regular", true: "bare"}[bare], func(t *testing.T) {
			assert := assert.New(t)
			root := t.TempDir()
			execGit(t, root, "init")
			execGit(t, root, "config", "user.name", "Test User")
			execGit(t, root, "config", "user.email", "test@example.com")
			execGit(t, root, "commit", "--allow-empty", "-m", "initial")
			if bare {
				bareRoot := filepath.Join(t.TempDir(), "project.git")
				execGit(t, root, "clone", "--bare", root, bareRoot)
				root = bareRoot
				execGit(t, root, "remote", "remove", "origin")
			}
			execGit(t, root, "remote", "add", "origin", "git@example.com:team/project-a.git")
			for _, branch := range []string{"feature-a", "feature-b"} {
				wt := filepath.Join(t.TempDir(), branch)
				execGit(t, root, "worktree", "add", "-b", branch, wt)
				assert.Equal("project-review", ResolveModelForWorkflow("", wt, &cfg, "review", "thorough"))
				assert.Equal("Project A", GetDisplayName(wt, &cfg))
				assert.Equal("cli-review", ResolveModelForWorkflow("cli-review", wt, &cfg, "review", "thorough"))
				assert.Equal("default-fix", ResolveModelForWorkflow("", wt, &cfg, "fix", "standard"))
				writeRepoConfigStr(t, wt, "review_model = \"repo-review\"\ndisplay_name = \"Local name\"")
				assert.Equal("repo-review", ResolveModelForWorkflow("", wt, &cfg, "review", "thorough"))
				assert.Equal("Local name", GetDisplayName(wt, &cfg))
			}
			execGit(t, root, "remote", "set-url", "origin", "https://example.com/team/project-a.git")
			assert.Equal("project-review", ResolveModelForWorkflow("", root, &cfg, "review", "fast"))
			execGit(t, root, "remote", "set-url", "origin", "https://example.com/other/project-a.git")
			assert.Equal("default-thorough", ResolveModelForWorkflow("", root, &cfg, "review", "thorough"))
		})
	}
}

func TestProjectRemoteIdentity(t *testing.T) {
	for _, remote := range []string{
		"git@example.com:team/project-a.git",
		"ssh://git@EXAMPLE.COM/team/project-a.git",
		"https://example.com/team/project-a/",
		"https://user:password@example.com/team/project-a.git",
	} {
		assert.Equal(t, "example.com/team/project-a", projectRemoteIdentity(remote), remote)
	}
	for _, remote := range []string{"", "/tmp/project-a", "file:///tmp/project-a", "../project-a"} {
		assert.Empty(t, projectRemoteIdentity(remote), remote)
	}
}

func TestProjectPanelOverrideRequiresModel(t *testing.T) {
	for _, model := range []string{"", "   ", "project-model"} {
		cfg := DefaultConfig()
		cfg.Projects = map[string]ProjectConfig{
			"example.com/team/project-a": {OverridePanelModels: true, ReviewModel: model},
		}
		dir := t.TempDir()
		writeTestFile(t, dir, "config.toml", fmt.Sprintf(`
[projects."example.com/team/project-a"]
override_panel_models = true
review_model = %q
`, model))
		_, loadErr := LoadGlobalFrom(filepath.Join(dir, "config.toml"))
		if model == "project-model" {
			require.NoError(t, cfg.Validate())
			require.NoError(t, loadErr)
		} else {
			require.ErrorContains(t, cfg.Validate(), "override_panel_models requires review_model")
			require.ErrorContains(t, loadErr, "override_panel_models requires review_model")
		}
	}
}

func TestProjectReviewReasoning(t *testing.T) {
	root := t.TempDir()
	execGit(t, root, "init")
	execGit(t, root, "remote", "add", "origin", "https://example.com/team/project-a.git")
	var cfg Config
	_, err := toml.Decode(`
review_reasoning = "maximum"
[projects."example.com/team/project-a"]
review_reasoning = "medium"
`, &cfg)
	require.NoError(t, err)
	for _, explicit := range []string{"", "high"} {
		want := "medium"
		if explicit != "" {
			want = explicit
		}
		got, err := ResolveReviewReasoning(explicit, root, &cfg)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		got, err = ResolveCIReviewReasoningForType(explicit, nil, cfg.ForRepo(root), "security")
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	writeRepoConfigStr(t, root, "review_reasoning = \"fast\"")
	got, err := ResolveReviewReasoning("", root, &cfg)
	require.NoError(t, err)
	assert.Equal(t, "fast", got)
	execGit(t, root, "remote", "set-url", "origin", "https://example.com/team/project-b.git")
	got, err = ResolveReviewReasoningFromConfig("", nil, cfg.ForRepo(root))
	require.NoError(t, err)
	assert.Equal(t, "maximum", got)
	got, err = ResolveCIReasoning("", nil, cfg.ForRepo(root))
	require.NoError(t, err)
	assert.Equal(t, "thorough", got)
}

func TestProjectReasoningValidation(t *testing.T) {
	for _, tt := range []struct{ setting, wantError string }{
		{`review_reasoning = "medium"`, ""},
		{`synthesis_reasoning = "high"`, ""},
		{`review_reasoning = "invalid"`, "review_reasoning"},
		{`synthesis_reasoning = "invalid"`, "synthesis_reasoning"},
		{`override_panel_reasoning = true`, "override_panel_reasoning requires review_reasoning"},
	} {
		t.Run(tt.setting, func(t *testing.T) {
			content := "[projects.\"example.com/team/project-a\"]\n" + tt.setting
			dir := t.TempDir()
			writeTestFile(t, dir, "config.toml", content)
			var cfg Config
			_, err := toml.Decode(content, &cfg)
			require.NoError(t, err)
			_, loadErr := LoadGlobalFrom(filepath.Join(dir, "config.toml"))
			if tt.wantError == "" {
				require.NoError(t, cfg.Validate())
				require.NoError(t, loadErr)
			} else {
				require.ErrorContains(t, cfg.Validate(), tt.wantError)
				require.ErrorContains(t, loadErr, tt.wantError)
			}
		})
	}
}

func TestProjectReasoningSelectsPanelModels(t *testing.T) {
	root := t.TempDir()
	execGit(t, root, "init")
	execGit(t, root, "remote", "add", "origin", "https://example.com/team/project-a.git")
	cfg := &Config{
		DefaultAgent: "test", ReviewModelMedium: "review-medium", ReviewModelHigh: "review-high",
		FixModelMedium: "fix-medium",
		Projects: map[string]ProjectConfig{"example.com/team/project-a": {
			ReviewReasoning: "medium", OverridePanelReasoning: true, SynthesisReasoning: "medium",
		}},
		Review: ReviewConfig{
			Subagents: map[string]SubagentSpec{"member": {Reasoning: "high"}},
			Panels:    map[string]PanelSpec{"panel-a": {Members: []string{"member"}}},
		},
	}
	members, synth, err := ResolvePanel("panel-a", root, cfg)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, "medium", members[0].Reasoning)
	assert.Equal(t, "review-medium", members[0].Model)
	assert.Equal(t, "medium", synth.Reasoning)
	assert.Equal(t, "fix-medium", synth.Model)
}

func TestProjectReasoningExperimentPrecedence(t *testing.T) {
	for _, reasoning := range []string{"", "high"} {
		cfg := &Config{
			project: ProjectConfig{ReviewReasoning: "medium"},
			Experiments: map[string]ExperimentDefinition{"reasoning-a": {
				Enabled: new(true), Ratio: new(1.0),
				Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
				Config:    map[string]any{"review_reasoning": reasoning},
			}},
		}
		selection, err := SelectReviewExperiment(ExperimentSelectionInput{
			Workflow: ExperimentWorkflowReview,
			Subject:  ExperimentSubject{Repository: "example.com/team/project-a", Branch: "feature"},
			Global:   cfg, Repo: &RepoConfig{}, RawRepo: map[string]any{},
		})
		require.NoError(t, err)
		got, err := ResolveReviewReasoningFromConfig("", selection.RepoConfig, cfg)
		require.NoError(t, err)
		want := reasoning
		if want == "" {
			want = "thorough"
		}
		assert.Equal(t, want, got)
	}
}

func TestProjectCIReasoningExperimentReset(t *testing.T) {
	cfg := &Config{
		project: ProjectConfig{ReviewReasoning: "medium"},
		Experiments: map[string]ExperimentDefinition{"reasoning-a": {
			Enabled: new(true), Ratio: new(1.0),
			Workflows: []ExperimentWorkflow{ExperimentWorkflowCI},
			Config:    map[string]any{"ci": map[string]any{"reasoning": ""}},
		}},
	}
	selection, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowCI,
		Subject:  ExperimentSubject{Repository: "example.com/team/project-a", Branch: "feature"},
		Global:   cfg, Repo: &RepoConfig{}, RawRepo: map[string]any{},
	})
	require.NoError(t, err)
	got, err := ResolveCIReasoning("", selection.RepoConfig, cfg)
	require.NoError(t, err)
	assert.Equal(t, "thorough", got)
	got, err = ResolveCIReviewReasoningForType("", selection.RepoConfig, cfg, "security")
	require.NoError(t, err)
	assert.Equal(t, "thorough", got)
	cfg.project.OverridePanelReasoning = true
	got, err = ResolveCIReasoning("", selection.RepoConfig, cfg)
	require.NoError(t, err)
	assert.Equal(t, "medium", got)
	got, err = ResolveCIReasoning("high", selection.RepoConfig, cfg)
	require.NoError(t, err)
	assert.Equal(t, "high", got)
}
