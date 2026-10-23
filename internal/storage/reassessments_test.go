package storage

import (
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/katapublish"
)

func TestReassessmentRejectsChangedDocumentWithSameReviewUUID(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/reassessment-document-enqueue", "abc123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	findings, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	_, err = env.db.Exec(`UPDATE reviews SET structured_output=? WHERE job_id=?`, strings.ReplaceAll(snapshotDocument, "Nil dereference.", "Different problem."), env.job.ID)
	require.NoError(t, err)
	_, err = env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, Agent: "test", Prompt: "reassess", Source: JobSourceRebuttal, ParentJobID: env.job.ID, Reassessment: &ReassessmentContext{OriginalReviewUUID: findings[0].ReviewUUID, Finding: findings[0], Selector: findings[0].Selector, Requester: "reader", Explanation: "Evidence for the selected finding."}})
	require.ErrorIs(t, err, ErrFindingChanged)
	comments, err := env.db.GetCommentsForJob(env.job.ID)
	require.NoError(t, err)
	assert.Empty(t, comments)
}

func TestReassessmentCompletionGuardsCanonicalDocument(t *testing.T) {
	t.Parallel()
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "equivalent encoding", true: "changed finding"}[changed], func(t *testing.T) {
			env := setupJobEnv(t, "/tmp/reassessment-document-complete", "abc123")
			completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
			findings, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
			require.NoError(t, err)
			job, err := env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, Agent: "test", Prompt: "reassess", Source: JobSourceRebuttal, ParentJobID: env.job.ID, Reassessment: &ReassessmentContext{OriginalReviewUUID: findings[0].ReviewUUID, Finding: findings[0], Selector: findings[0].Selector, Requester: "reader", Explanation: "Evidence for the selected finding."}})
			require.NoError(t, err)
			claimJob(t, env.db, "worker-1")
			document := "\n" + snapshotDocument + "\n"
			if changed {
				document = strings.ReplaceAll(snapshotDocument, "Nil dereference.", "Different problem.")
			}
			_, err = env.db.Exec(`UPDATE reviews SET structured_output=? WHERE job_id=?`, document, env.job.ID)
			require.NoError(t, err)
			require.NoError(t, env.db.CompleteJob(job.ID, "test", `{"outcome":"confirmed","explanation":"The original finding still demonstrates the missing guard."}`))
			result, err := env.db.Reassessment(t.Context(), job.ID)
			require.NoError(t, err)
			require.NotNil(t, result.Assessment, "preserve the assessment even when its parent changed")
			comments, err := env.db.GetCommentsForJob(env.job.ID)
			require.NoError(t, err)
			want := 2
			if changed {
				want = 1
			}
			assert.Len(t, comments, want)
		})
	}
}

func TestReassessmentExcerptRejectsChangedReviewContent(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/reassessment-excerpt-content", "abc123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	review, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	hash, err := ReassessmentReviewHash(review)
	require.NoError(t, err)
	_, err = env.db.Exec(`UPDATE reviews SET structured_output=? WHERE job_id=?`, strings.ReplaceAll(snapshotDocument, "Nil dereference.", "Different problem."), env.job.ID)
	require.NoError(t, err)
	_, err = env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, Agent: "test", Prompt: "reassess", Source: JobSourceRebuttal, ParentJobID: env.job.ID, Reassessment: &ReassessmentContext{OriginalReviewUUID: *review.UUID, OriginalDocumentHash: hash, Excerpt: review.Output, Requester: "reader", Explanation: "Evidence for the original excerpt."}})
	require.ErrorIs(t, err, ErrFindingChanged)
}

func TestReassessmentContextAndAssessmentAreDurable(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/reassessment", "abc123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	snapshots, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	original := reviewUUIDOf(t, env.db, env.job.ID)
	capture := &ReassessmentContext{OriginalReviewUUID: original, OriginalDocumentHash: snapshots[0].DocumentHash, Finding: snapshots[0], Selector: snapshots[0].Selector, Requester: "reader", Explanation: "The caller supplies counter evidence.", Policy: "summary"}
	job, err := env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, Agent: "test", Prompt: "reassess", JobType: JobTypeTask, Source: JobSourceRebuttal, ParentJobID: env.job.ID, Reassessment: capture})
	require.NoError(t, err)
	got, err := env.db.Reassessment(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, *capture, got.Context)
	claimed := claimJob(t, env.db, "worker-1")
	require.Equal(t, job.ID, claimed.ID)
	output := `{"outcome":"refuted","explanation":"The guard prevents this path from receiving a nil pointer."}`
	require.NoError(t, env.db.CompleteJob(job.ID, "test", output))
	require.NoError(t, env.db.CompleteJob(job.ID, "test", output), "duplicate completion is a no-op")
	got, err = env.db.Reassessment(t.Context(), job.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Assessment)
	assert.Equal(t, "refuted", got.Assessment.Outcome)
	responses, err := env.db.GetCommentsForJob(env.job.ID)
	require.NoError(t, err)
	require.Len(t, responses, 2, "request and validated assessment commit once")
	assert.Contains(t, responses[1].Response, "refuted")
	review, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	assert.False(t, review.Closed)
	assert.Equal(t, &original, review.UUID)
}

func TestReassessmentEnqueueGuardsReviewVersion(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/reassessment-guard", "abc123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	_, err := env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, Agent: "test", Prompt: "reassess", Source: JobSourceRebuttal, ParentJobID: env.job.ID, Reassessment: &ReassessmentContext{OriginalReviewUUID: uuid.New()}})
	require.ErrorIs(t, err, ErrReviewChanged)
	var count int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM review_jobs WHERE source=?`, JobSourceRebuttal).Scan(&count))
	assert.Zero(t, count)
}

func TestReassessmentCompletionAfterParentRerun(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/reassessment-rerun", "abc123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	original := reviewUUIDOf(t, env.db, env.job.ID)
	job, err := env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, Agent: "test", Prompt: "reassess", JobType: JobTypeTask, Source: JobSourceRebuttal, ParentJobID: env.job.ID, Reassessment: &ReassessmentContext{OriginalReviewUUID: original, Requester: "reader", Explanation: "Request evidence", Excerpt: "original finding"}})
	require.NoError(t, err)
	claimJob(t, env.db, "worker-1")
	replacement := uuid.New()
	_, err = env.db.Exec(`UPDATE reviews SET uuid=? WHERE job_id=?`, replacement, env.job.ID)
	require.NoError(t, err)
	require.NoError(t, env.db.CompleteJob(job.ID, "test", `{"outcome":"confirmed","explanation":"The original snapshot still demonstrates a missing guard."}`))
	result, err := env.db.Reassessment(t.Context(), job.ID)
	require.NoError(t, err)
	require.NotNil(t, result.Assessment)
	assert.Equal(t, original, result.Context.OriginalReviewUUID)
	comments, err := env.db.GetCommentsForJob(env.job.ID)
	require.NoError(t, err)
	assert.Len(t, comments, 1, "never annotate the replacement review")
}

func TestReassessmentInvalidCompletionRollsBack(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/reassessment-invalid", "abc123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	job, err := env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, Agent: "test", Prompt: "reassess", JobType: JobTypeTask, Source: JobSourceRebuttal, ParentJobID: env.job.ID, Reassessment: &ReassessmentContext{OriginalReviewUUID: reviewUUIDOf(t, env.db, env.job.ID), Requester: "reader", Explanation: "Request evidence", Excerpt: "original finding"}})
	require.NoError(t, err)
	claimJob(t, env.db, "worker-1")
	require.Error(t, env.db.CompleteJob(job.ID, "test", `{"outcome":"confirmed","explanation":""}`))
	stored, err := env.db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusRunning, stored.Status)
	result, err := env.db.Reassessment(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Nil(t, result.Assessment)
}

func TestReassessmentFailureRollsBackAnnotationAndDelivery(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/reassessment-atomic", "abc123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	findings, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	delivery := &katapublish.Request{Endpoint: "https://kata.example", Project: "reviews", ProjectUID: "project", IssueUID: "issue", Policy: "comments", ReviewUUID: findings[0].ReviewUUID.String(), Teammate: "roborev-test", Responses: []katapublish.Response{{Root: "root", Target: "root", Teammate: "roborev-test"}}} //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
	job, err := env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, Agent: "test", Prompt: "reassess", Source: JobSourceRebuttal, ParentJobID: env.job.ID, Reassessment: &ReassessmentContext{OriginalReviewUUID: findings[0].ReviewUUID, Finding: findings[0], Selector: findings[0].Selector, Requester: "reader", Explanation: "Request evidence", Policy: "comments", Delivery: delivery}})
	require.NoError(t, err)
	claimJob(t, env.db, "worker-1")
	_, err = env.db.Exec(`CREATE TRIGGER reject_response_delivery BEFORE INSERT ON kata_handoffs BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
	require.NoError(t, err)
	output := `{"outcome":"refuted","explanation":"The caller validates the selected argument before the path."}`
	require.Error(t, env.db.CompleteJob(job.ID, "test", output))
	capture, err := env.db.Reassessment(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Nil(t, capture.Assessment)
	comments, err := env.db.GetCommentsForJob(env.job.ID)
	require.NoError(t, err)
	assert.Len(t, comments, 1)
	stored, err := env.db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusRunning, stored.Status)
	_, err = env.db.Exec(`DROP TRIGGER reject_response_delivery`)
	require.NoError(t, err)
	require.NoError(t, env.db.CompleteJob(job.ID, "test", output))
	handoffs, err := env.db.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Len(t, handoffs, 1)
}

func TestReassessmentSurvivesDatabaseRestart(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/reassessment-restart", "abc123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	original := reviewUUIDOf(t, env.db, env.job.ID)
	job, err := env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, Agent: "test", Prompt: "reassess", Source: JobSourceRebuttal, ParentJobID: env.job.ID, Reassessment: &ReassessmentContext{OriginalReviewUUID: original, Requester: "reader", Explanation: "Request evidence", Excerpt: "Original snapshot"}})
	require.NoError(t, err)
	var sequence int
	var name, path string
	require.NoError(t, env.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path))
	require.NoError(t, env.db.Close())
	restarted, err := Open(path)
	require.NoError(t, err)
	defer restarted.Close()
	claimJob(t, restarted, "worker-1")
	require.NoError(t, restarted.CompleteJob(job.ID, "test", `{"outcome":"confirmed","explanation":"The selected path still omits the guard."}`))
	result, err := restarted.Reassessment(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, original, result.Context.OriginalReviewUUID)
	require.NotNil(t, result.Assessment)
	fetched, err := restarted.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, &original, fetched.OriginalReviewUUID)
}
