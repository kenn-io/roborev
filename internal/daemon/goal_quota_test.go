package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

func TestGoalQuotaFailureCoolsAgent(t *testing.T) {
	c := newWorkerTestContext(t, 1)
	registerGoalReviewPi(t, c.Pool.cfgGetter.Config())
	// Inject a safe schema runner error and quota classification to exercise
	// cooldown handling without invoking a CLI.
	c.Pool.classify = func(_ string, message string) agent.LimitClassification {
		if strings.Contains(message, "test agent configured to fail") {
			return agent.LimitClassification{Kind: agent.LimitKindQuota}
		}
		return agent.LimitClassification{}
	}
	c.Pool.goalReviewRunner = goalReviewTestRunner(func(context.Context, goalreview.Snapshot, prompt.SnapshotResult) ([]goalreview.Finding, error) {
		return nil, &goalreview.ExecutionError{Err: errors.New("test agent configured to fail")}
	})
	c.Pool.cfgGetter.Config().DefaultBackupAgent = ""
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
	job, err := c.DB.EnqueueJob(storage.EnqueueOpts{RepoID: c.Repo.ID, Agent: "pi", GitRef: snapshot.ID(), JobType: storage.JobTypeGoalReview, ReviewType: "goal", Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true})
	require.NoError(t, err)
	claimed, err := c.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	c.Pool.processJob(testWorkerID, claimed)
	after, err := c.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.True(t, c.Pool.isAgentCoolingDown("pi"), "provider quota failure must put goal-review agent into cooldown")
	assert.Equal(t, storage.JobStatusFailed, after.Status, "quota failures should skip pointless retries when no backup is available")
}

func TestGoalQuotaFailureSkipsUnsupportedBackup(t *testing.T) {
	c := newWorkerTestContext(t, 1)
	registerGoalReviewPi(t, c.Pool.cfgGetter.Config())
	c.Pool.cfgGetter.Config().ReviewBackupAgent = "test"
	c.Pool.classify = func(_ string, message string) agent.LimitClassification {
		if strings.Contains(message, "test agent configured to fail") {
			return agent.LimitClassification{Kind: agent.LimitKindQuota}
		}
		return agent.LimitClassification{}
	}
	c.Pool.goalReviewRunner = goalReviewTestRunner(func(context.Context, goalreview.Snapshot, prompt.SnapshotResult) ([]goalreview.Finding, error) {
		return nil, &goalreview.ExecutionError{Err: errors.New("test agent configured to fail")}
	})
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
	job, err := c.DB.EnqueueJob(storage.EnqueueOpts{RepoID: c.Repo.ID, Agent: "pi", GitRef: snapshot.ID(), JobType: storage.JobTypeGoalReview, ReviewType: "goal", Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true})
	require.NoError(t, err)
	claimed, err := c.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	c.Pool.processJob(testWorkerID, claimed)

	after, err := c.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusFailed, after.Status)
	assert.Equal(t, "pi", after.Agent, "goal review must not fail over to an agent outside its allowlist")
}
