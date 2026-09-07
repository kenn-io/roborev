package config

import (
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
