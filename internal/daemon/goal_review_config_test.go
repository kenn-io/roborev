package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

func TestGoalWorkerUsesConfigurationForItsTrigger(t *testing.T) {
	for _, tc := range []struct {
		name, source       string
		mainMax, linkedMax int
		removed, badMain   bool
		wantSnapshotDir    string
		wantTimeout        time.Duration
	}{
		{name: "manual", mainMax: 1, linkedMax: 100000, wantTimeout: 7 * time.Minute},
		{name: "gate", source: "goal_gate", mainMax: 100000, linkedMax: 1, wantSnapshotDir: ".linked-snapshots", wantTimeout: 7 * time.Minute},
		{name: "watcher", source: "goal_watch", mainMax: 1, linkedMax: 100000, wantSnapshotDir: ".main-snapshots", wantTimeout: 3 * time.Minute},
		{name: "removed worktree", removed: true, mainMax: 1, linkedMax: 100000, wantSnapshotDir: ".main-snapshots", wantTimeout: 3 * time.Minute},
		{name: "malformed main config", badMain: true, source: "goal_gate", mainMax: 1, linkedMax: 1, wantSnapshotDir: ".linked-snapshots", wantTimeout: 7 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newWorkerTestContext(t, 1)
			registerGoalReviewPi(t, c.Pool.cfgGetter.Config())
			c.Pool.cfgGetter.Config().DefaultMaxPromptSize = 1
			linked := filepath.Join(t.TempDir(), "linked")
			c.GitRepo.RunGit("worktree", "add", "--detach", linked)
			mainConfig := fmt.Sprintf("job_timeout_minutes = 3\nmax_prompt_size = %d\nsnapshot_dir = '.main-snapshots'\n", tc.mainMax)
			if tc.badMain {
				mainConfig = "malformed ["
			}
			require.NoError(t, os.WriteFile(filepath.Join(c.TmpDir, ".roborev.toml"), []byte(mainConfig), 0o600))
			linkedConfig := fmt.Sprintf("job_timeout_minutes = 7\nmax_prompt_size = %d\nsnapshot_dir = '.linked-snapshots'\n", tc.linkedMax)
			require.NoError(t, os.WriteFile(filepath.Join(linked, ".roborev.toml"), []byte(linkedConfig), 0o600))
			executionRoot := linked
			if tc.removed {
				c.GitRepo.RunGit("worktree", "remove", "--force", linked)
				executionRoot = c.TmpDir
			}
			snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Captured intent\n"}}}
			fullPrompt := goalreview.BuildPrompt(snapshot)
			job, err := c.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: c.Repo.ID, Agent: "pi", GitRef: snapshot.ID(), ReviewType: "goal",
				JobType: storage.JobTypeGoalReview, Prompt: fullPrompt, PromptPrebuilt: true,
				WorktreePath: linked, Source: tc.source,
			})
			require.NoError(t, err)
			claimed, err := c.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			invoked := false
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				started := time.Now()
				c.Pool.goalReviewRunner = goalReviewTestRunner(func(ctx context.Context, _ goalreview.Snapshot, prepared prompt.SnapshotResult) ([]goalreview.Finding, error) {
					invoked = true
					deadline, ok := ctx.Deadline()
					require.True(t, ok)
					assert.Equal(started.Add(tc.wantTimeout), deadline)
					if tc.wantSnapshotDir == "" {
						assert.Empty(prepared.FilePath)
						assert.Equal(fullPrompt, prepared.Prompt)
					} else {
						require.NotEmpty(t, prepared.FilePath)
						assert.Equal(filepath.Join(executionRoot, tc.wantSnapshotDir), filepath.Dir(filepath.Dir(prepared.FilePath)))
						contents, err := os.ReadFile(prepared.FilePath)
						require.NoError(t, err)
						assert.Equal(fullPrompt, string(contents))
					}
					return nil, nil
				})
				c.Pool.processJob(testWorkerID, claimed)
			})
			stored, err := c.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusDone, stored.Status, stored.Error)
			assert.True(t, invoked)
		})
	}
}
