package storage

import (
	"encoding/json/v2"
	"sync"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/kataresponse"
)

func TestKataResponseConcurrentRetriesAndLostAcknowledgement(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/response", "abc123")
	env.db.SetMaxOpenConns(1)
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	findings, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	intent := kataresponse.Request{JobID: env.job.ID, OperationID: uuid.New().String(), FindingID: findings[0].Selector, Kind: "refute", Body: "The guard prevents a nil value from reaching this code."} //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
	fingerprint, err := intent.Fingerprint()
	require.NoError(t, err)
	capture := katapublish.Request{OperationID: intent.OperationID, Endpoint: "https://kata.example", Project: "reviews", ProjectUID: "project-uid", IssueUID: "issue-uid", ReviewUUID: findings[0].ReviewUUID.String(), Policy: "comments", Teammate: "reviewer", Responses: []katapublish.Response{{OperationID: intent.OperationID, Fingerprint: fingerprint, Body: intent.Body, Kind: intent.Kind, Target: "root-uid", Teammate: "reviewer"}}} //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { _, err := env.db.EnqueueKataResponse(t.Context(), intent, capture); assert.NoError(t, err) })
	}
	wg.Wait()
	resumed, err := env.db.ResumeKataResponse(t.Context(), intent)
	require.NoError(t, err)
	require.NotNil(t, resumed)
	assert.Equal(t, "pending", resumed.State)
	assert.Equal(t, "refute", resumed.Kind)
	intent.Body += " Changed evidence."
	_, err = env.db.ResumeKataResponse(t.Context(), intent)
	require.ErrorIs(t, err, ErrResponseChanged)
	_, err = env.db.EnqueueKataResponse(t.Context(), intent, capture)
	require.ErrorIs(t, err, ErrResponseChanged)
	handoffs, err := env.db.KataHandoffs(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.Len(t, handoffs, 1)
	assert.Equal(t, capture.IssueUID, handoffs[0].State.IssueUID)
	assert.Equal(t, capture.ProjectUID, handoffs[0].State.ProjectUID)
	assert.Equal(t, capture.Project+"#"+capture.IssueUID, handoffs[0].State.IssueRef)
}

func TestKataResponseStatusesScopeFrozenIssueAndPreserveOpaqueKeys(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/response-status", "abc123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	findings, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	for _, key := range []string{"Reply-Key", "reply-key"} {
		intent := kataresponse.Request{OperationID: key, FindingID: findings[0].Selector, Kind: "reply", Body: "confidential-sentinel: local evidence remains private."}
		fingerprint, err := intent.Fingerprint()
		require.NoError(t, err)
		capture := katapublish.Request{OperationID: key, Endpoint: "https://kata.example", Project: "reviews", ProjectUID: "project-uid", IssueUID: "issue-" + key, Policy: "summary", Teammate: "reviewer", Entries: []katapublish.Entry{{Selector: findings[0].Selector}}, Responses: []katapublish.Response{{OperationID: key, Fingerprint: fingerprint, Body: "Evidence is available locally.", Kind: "reply", Target: "root-uid", Teammate: "reviewer"}}}
		status, err := env.db.EnqueueKataResponse(t.Context(), intent, capture)
		require.NoError(t, err)
		assert.Equal(t, key, status.OperationID)
		assert.Equal(t, env.job.ID, status.JobID)
		resumed, err := env.db.ResumeKataResponse(t.Context(), intent)
		require.NoError(t, err)
		require.NotNil(t, resumed)
		assert.Equal(t, status, *resumed)
	}
	all, err := env.db.KataResponseStatuses(t.Context(), env.job.ID, "")
	require.NoError(t, err)
	require.Len(t, all, 2)
	filtered, err := env.db.KataResponseStatuses(t.Context(), env.job.ID, "issue-Reply-Key")
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	assert.Equal(t, "Reply-Key", filtered[0].OperationID)
	assert.Equal(t, "pending", filtered[0].State)
	empty, err := env.db.KataResponseStatuses(t.Context(), env.job.ID, "different-issue")
	require.NoError(t, err)
	assert.Empty(t, empty)
	raw, err := json.Marshal(all)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "confidential-sentinel")
	assert.NotContains(t, string(raw), "job_id")
	assert.NotContains(t, string(raw), "endpoint")
}

func TestDeleteRepoRemovesKataResponseAndReassessmentData(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/response-delete", "abc123")
	// Foreign-key enforcement is connection-local. Use one connection so the
	// test deterministically exercises the explicit cleanup path.
	env.db.SetMaxOpenConns(1)
	_, err := env.db.Exec(`PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	findings, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	intent := kataresponse.Request{OperationID: "delete-response", FindingID: findings[0].Selector, Kind: "reply", Body: "Evidence for the original finding."}
	fingerprint, err := intent.Fingerprint()
	require.NoError(t, err)
	capture := katapublish.Request{OperationID: intent.OperationID, Endpoint: "https://kata.example", Project: "reviews", ProjectUID: "project", IssueUID: "issue", Policy: "comments", Entries: []katapublish.Entry{{Selector: findings[0].Selector}}, Responses: []katapublish.Response{{OperationID: intent.OperationID, Fingerprint: fingerprint, Body: intent.Body, Kind: intent.Kind, Target: "root"}}} //nolint:forbidigo // Synthetic provider target.
	_, err = env.db.EnqueueKataResponse(t.Context(), intent, capture)
	require.NoError(t, err)
	_, err = env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, Agent: "test", Prompt: "reassess", Source: JobSourceRebuttal, ParentJobID: env.job.ID, Reassessment: &ReassessmentContext{OriginalReviewUUID: findings[0].ReviewUUID, Requester: "reader", Explanation: "Evidence for reassessment."}})
	require.NoError(t, err)
	for _, table := range []string{"kata_responses", "reassessments"} {
		var count int
		require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&count))
		require.Equal(t, 1, count, "seed %s", table)
	}
	require.NoError(t, env.db.DeleteRepo(env.repo.ID, true))
	for _, table := range []string{"kata_responses", "reassessments"} {
		var count int
		require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&count))
		assert.Zero(t, count, "%s must leave with the repository", table)
	}
}
