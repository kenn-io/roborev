package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

func TestGoalFailoverUsesConfigurationForItsTrigger(t *testing.T) {
	for _, tc := range []struct {
		name, source, mainBackup, linkedBackup, wantModel string
		removed                                           bool
	}{
		{name: "manual worktree backup", linkedBackup: "claude-code", wantModel: "linked-model"},
		{name: "gate worktree model", source: "goal_gate", mainBackup: "claude-code", linkedBackup: "claude-code", wantModel: "linked-model"},
		{name: "watcher main backup", source: "goal_watch", mainBackup: "claude-code", wantModel: "main-model"},
		{name: "removed worktree", mainBackup: "claude-code", linkedBackup: "claude-code", removed: true, wantModel: "main-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			c := newWorkerTestContext(t, 1)
			cfg := c.Pool.cfgGetter.Config()
			registerGoalReviewPi(t, cfg)
			executable, err := os.Executable()
			require.NoError(t, err)
			cfg.ClaudeCodeCmd = executable
			linked := filepath.Join(t.TempDir(), "linked")
			c.GitRepo.RunGit("worktree", "add", "--detach", linked)
			mainConfig := fmt.Sprintf("review_backup_agent = %q\nreview_backup_model = 'main-model'\n", tc.mainBackup)
			linkedConfig := fmt.Sprintf("review_backup_agent = %q\nreview_backup_model = 'linked-model'\n", tc.linkedBackup)
			require.NoError(t, os.WriteFile(filepath.Join(c.TmpDir, ".roborev.toml"), []byte(mainConfig), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(linked, ".roborev.toml"), []byte(linkedConfig), 0o600))
			if tc.removed {
				c.GitRepo.RunGit("worktree", "remove", "--force", linked)
			}
			c.Pool.classify = func(string, string) agent.LimitClassification {
				return agent.LimitClassification{Kind: agent.LimitKindQuota}
			}
			var invoked []string
			c.Pool.goalReviewRunner = func(_ context.Context, a agent.Agent, _ string, _ goalreview.Snapshot, _ prompt.SnapshotResult, _ io.Writer) ([]goalreview.Finding, error) {
				invoked = append(invoked, a.Name())
				if a.Name() == "pi" {
					return nil, &goalreview.ExecutionError{Err: errors.New("provider quota exhausted")}
				}
				claude, ok := a.(*agent.ClaudeAgent)
				require.True(t, ok)
				assert.Equal(tc.wantModel, claude.Model)
				return nil, nil
			}
			snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
			job, err := c.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: c.Repo.ID, Agent: "pi", GitRef: snapshot.ID(), ReviewType: "goal", JobType: storage.JobTypeGoalReview,
				Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true, WorktreePath: linked, Source: tc.source,
			})
			require.NoError(t, err)
			claimed, err := c.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			c.Pool.processJob(testWorkerID, claimed)
			after, err := c.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			require.Equal(t, storage.JobStatusQueued, after.Status, after.Error)
			assert.Equal("claude-code", after.Agent)
			assert.Equal(tc.wantModel, after.Model)
			claimed, err = c.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			c.Pool.processJob(testWorkerID, claimed)
			completed, err := c.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(storage.JobStatusDone, completed.Status, completed.Error)
			assert.Equal([]string{"pi", "claude-code"}, invoked)
		})
	}
}
