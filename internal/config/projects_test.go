package config

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectPanelModelOverride(t *testing.T) {
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
[review.subagents.general]
agent = "codex"
model = "member-model"
[review.subagents.security-check]
agent = "codex"
model = "security-member-model"
review_type = "security"
[review.panels.panel-a]
members = ["general", "security-check"]
synthesis_agent = "codex"
synthesis_model = "synthesis-model"
`, tt.override, tt.synthesis), &cfg)
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
