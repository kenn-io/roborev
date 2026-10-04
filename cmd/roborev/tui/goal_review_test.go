package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestGoalReviewCommitMessage(t *testing.T) {
	t.Parallel()
	m := newModel(localhostEndpoint, withExternalIODisabled())
	job := storage.ReviewJob{
		ID: 1, JobType: storage.JobTypeGoalReview,
		GitRef: strings.Repeat("a", 64), RepoPath: t.TempDir(),
	}
	msg := m.fetchCommitMsg(&job)()
	result, ok := msg.(commitMsgMsg)
	require.True(t, ok)
	assert.Equal(t, job.ID, result.jobID)
	require.EqualError(t, result.err, "no commit message for goal reviews")
	assert.Empty(t, result.content)
}

func TestGoalReviewBranchDisplay(t *testing.T) {
	t.Parallel()
	m := newModel(localhostEndpoint, withExternalIODisabled())
	job := storage.ReviewJob{
		ID: 1, JobType: storage.JobTypeGoalReview,
		GitRef: strings.Repeat("a", 64), RepoPath: t.TempDir(),
	}
	assert := assert.New(t)
	assert.Empty(m.getBranchForJob(job))
	assert.Empty(reviewBranchName(&job))
	assert.Empty(detachedBranchLabel(job))
	branch, persist := backfillBranchValue(job, nil)
	assert.Equal(branchNone, branch)
	assert.True(persist)

	job.Branch = "feature"
	assert.Equal("feature", m.getBranchForJob(job))
	assert.Equal("feature", reviewBranchName(&job))
}

func TestGoalReviewRerunAgentConfigPolicy(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, tt := range []struct {
		name           string
		source         string
		mainConfig     string
		worktreeConfig string
		removeWorktree bool
		wantError      bool
	}{
		{"watcher ignores malformed worktree", "goal_watch", "", "[invalid", false, false},
		{"manual uses worktree", "", "[invalid", "", false, false},
		{"gate uses worktree", "goal_gate", "[invalid", "", false, false},
		{"manual rejects malformed worktree", "", "", "[invalid", false, true},
		{"gate rejects malformed worktree", "goal_gate", "", "[invalid", false, true},
		{"manual removed worktree", "", "", "[invalid", true, false},
		{"gate removed worktree", "goal_gate", "", "[invalid", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := testutil.NewTestRepoWithCommit(t)
			worktree := filepath.Join(t.TempDir(), "worktree")
			repo.Run("worktree", "add", "--detach", worktree, "HEAD")
			repo.WriteFile(".roborev.toml", tt.mainConfig)
			require.NoError(t, os.WriteFile(filepath.Join(worktree, ".roborev.toml"), []byte(tt.worktreeConfig), 0o600))
			if tt.removeWorktree {
				repo.Run("worktree", "remove", "--force", worktree)
			}
			m := newModel(localhostEndpoint, withExternalIODisabled())
			m.globalCfg = config.DefaultConfig()
			m.globalCfg.ClaudeCodeCmd = executable
			job := storage.ReviewJob{
				Agent: "pi", JobType: storage.JobTypeGoalReview, ReviewType: config.ReviewTypeGoal,
				RepoPath: repo.Path(), WorktreePath: worktree, Source: tt.source,
			}

			options, err := m.availableRerunAgents(&job)
			if tt.wantError {
				require.Error(t, err)
				assert.Empty(t, options)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{"claude-code"}, options)
		})
	}
}
