package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/katapublish"
)

func handoffBuilder(_ context.Context, c KataCompletion) ([]katapublish.Request, error) {
	r := katapublish.Request{OperationID: "publish:" + c.ReviewUUID, Endpoint: "https://kata.example", Project: "reviews", ProjectUID: "project-uid", Policy: "comments", ReviewUUID: c.ReviewUUID, Teammate: "reviewer", Create: kata.CreateReq{Title: "Review findings"}}
	for _, s := range c.Snapshots {
		r.Entries = append(r.Entries, katapublish.Entry{Selector: s.Selector, ReviewUUID: s.ReviewUUID.String(), DocumentHash: s.DocumentHash, Index: s.Index, Body: s.Finding.Problem})
	}
	return []katapublish.Request{r}, nil
}

func TestKataHandoffCompletionIsAtomic(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/handoff-atomic", "atomic123")
	env.db.SetKataHandoffBuilder(handoffBuilder)
	_, err := env.db.Exec(`CREATE TRIGGER reject_handoff BEFORE INSERT ON kata_handoffs BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	require.NoError(t, err)
	claimed := claimJob(t, env.db, "worker")
	err = completeReviewFixture(env.db, claimed.ID, "test", "", snapshotDocument)
	require.ErrorContains(t, err, "injected failure")
	j, err := env.db.GetJobByID(claimed.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusRunning, j.Status)
	var count int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM finding_snapshots`).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM reviews`).Scan(&count))
	assert.Zero(t, count)
	_, err = env.db.Exec(`DROP TRIGGER reject_handoff`)
	require.NoError(t, err)
	require.NoError(t, completeReviewFixture(env.db, claimed.ID, "test", "", snapshotDocument))
	got, err := env.db.KataHandoffs(t.Context(), claimed.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Request.Entries, 2)
	snaps, err := env.db.AssignFindingSnapshots(t.Context(), claimed.ID)
	require.NoError(t, err)
	assert.Equal(t, snaps[0].Selector, got[0].Request.Entries[0].Selector)
	// Replacing the review cannot replace its saved publication or snapshots.
	_, err = env.db.Exec(`DELETE FROM reviews WHERE job_id=?`, claimed.ID)
	require.NoError(t, err)
	got, err = env.db.KataHandoffs(t.Context(), claimed.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Nil dereference.", got[0].Request.Entries[0].Body)
}

func TestKataHandoffClaimsAndDurableBackoff(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/handoff-claim", "claim123")
	env.db.SetKataHandoffBuilder(handoffBuilder)
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		a, err := env.db.ClaimKataHandoff(t.Context(), now)
		require.NoError(t, err)
		require.NotNil(t, a)
		b, err := env.db.ClaimKataHandoff(t.Context(), now)
		require.NoError(t, err)
		assert.Nil(t, b)
		a.State.ProjectUID = "project-uid"
		require.NoError(t, env.db.CheckpointKataHandoff(t.Context(), a.OperationID, a.Claim, a.State))
		require.NoError(t, env.db.FinishKataHandoff(t.Context(), *a, errors.New("offline"), now))
		b, err = env.db.ClaimKataHandoff(t.Context(), now)
		require.NoError(t, err)
		assert.Nil(t, b)
		time.Sleep(time.Hour)
		b, err = env.db.ClaimKataHandoff(t.Context(), time.Now())
		require.NoError(t, err)
		require.NotNil(t, b)
		assert.Equal(t, "project-uid", b.State.ProjectUID)
		assert.Equal(t, 2, b.Attempts)
		require.ErrorIs(t, env.db.CheckpointKataHandoff(t.Context(), a.OperationID, a.Claim, a.State), ErrKataClaimLost)
		// Persisted claim expiry permits recovery after an unclean daemon exit.
		time.Sleep(time.Hour)
		c, err := env.db.ClaimKataHandoff(context.Background(), time.Now())
		require.NoError(t, err)
		require.NotNil(t, c)
		assert.NotEqual(t, b.Claim, c.Claim)
	})
}

func TestKataExplicitCaptureIsIdempotent(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/handoff-explicit-idempotent", "old123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	env.db.SetKataHandoffBuilder(handoffBuilder)
	require.NoError(t, env.db.CaptureKataHandoffs(t.Context(), env.job.ID))
	require.NoError(t, env.db.CaptureKataHandoffs(t.Context(), env.job.ID))
	records, err := env.db.KataHandoffs(t.Context(), env.job.ID)
	require.NoError(t, err)
	assert.Len(t, records, 1)
}

func TestKataCaptureConfigurationFailureRetainsReview(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/handoff-config", "cfg123")
	env.db.SetKataHandoffBuilder(func(context.Context, KataCompletion) ([]katapublish.Request, error) {
		return nil, errors.New("bad local config")
	})
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	records, err := env.db.KataHandoffs(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "blocked", records[0].Request.Policy)
	assert.Empty(t, records[0].Request.Create.Body)
	job, err := env.db.GetJobByID(env.job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusDone, job.Status)
}

func TestKataFailureWithInvalidUTF8RetainsOriginalError(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "failure-bytes")
	env.db.SetKataHandoffBuilder(func(ctx context.Context, c KataCompletion) ([]katapublish.Request, error) {
		requests, err := handoffBuilder(ctx, c)
		requests[0].Create.Body = c.Error
		return requests, err
	})
	job := claimJob(t, env.db, "worker")
	diagnostic := "agent stderr: " + string([]byte{0xff, 0xfe})
	updated, err := env.db.FailJob(job.ID, "worker", diagnostic)
	require.NoError(t, err)
	require.True(t, updated)
	current, err := env.db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusFailed, current.Status)
	assert.Equal(t, diagnostic, current.Error)
	records, err := env.db.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "blocked", records[0].Request.Policy)
	assert.Empty(t, records[0].Request.Create.Body)
}

func TestKataExplicitPublishCapturesCurrentRerun(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/handoff-rerun", "rerun123")
	env.db.SetKataHandoffBuilder(handoffBuilder)
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	old, err := env.db.KataHandoffs(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.Len(t, old, 1)
	env.db.SetKataHandoffBuilder(nil)
	_, err = env.db.Exec(`DELETE FROM reviews WHERE job_id=?`, env.job.ID)
	require.NoError(t, err)
	_, err = env.db.Exec(`UPDATE review_jobs SET status='running' WHERE id=?`, env.job.ID)
	require.NoError(t, err)
	require.NoError(t, completeReviewFixture(env.db, env.job.ID, "test", "", snapshotDocument))
	env.db.SetKataHandoffBuilder(handoffBuilder)
	require.NoError(t, env.db.CaptureKataHandoffs(t.Context(), env.job.ID))
	records, err := env.db.KataHandoffs(t.Context(), env.job.ID)
	require.NoError(t, err)
	assert.Len(t, records, 2)
}

func TestKataExplicitCaptureRejectsSameUUIDDocumentChange(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "capture-document-change")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	originalUUID := reviewUUIDOf(t, env.db, env.job.ID)
	changed := strings.Replace(snapshotDocument, "Nil dereference.", "Changed finding.", 1)
	env.db.SetKataHandoffBuilder(func(ctx context.Context, c KataCompletion) ([]katapublish.Request, error) {
		// Sync can replace this document while destination resolution is outside
		// the write transaction, retaining the same review UUID.
		_, err := env.db.Exec(`UPDATE reviews SET structured_output=? WHERE job_id=?`, changed, env.job.ID)
		require.NoError(t, err)
		return handoffBuilder(ctx, c)
	})
	require.ErrorIs(t, env.db.CaptureKataHandoffs(t.Context(), env.job.ID), ErrFindingChanged)
	assert.Equal(t, originalUUID, reviewUUIDOf(t, env.db, env.job.ID))
	records, err := env.db.KataHandoffs(t.Context(), env.job.ID)
	require.NoError(t, err)
	assert.Empty(t, records)
	var count int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM finding_snapshots WHERE job_id=?`, env.job.ID).Scan(&count))
	assert.Zero(t, count, "a rejected capture must not pin a different document")
	env.db.SetKataHandoffBuilder(handoffBuilder)
	require.NoError(t, env.db.CaptureKataHandoffs(t.Context(), env.job.ID))
	records, err = env.db.KataHandoffs(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "Changed finding.", records[0].Request.Entries[0].Body)
}

func TestKataExplicitCaptureAcceptsEquivalentDocumentEncoding(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "capture-document-encoding")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	env.db.SetKataHandoffBuilder(func(ctx context.Context, c KataCompletion) ([]katapublish.Request, error) {
		_, err := env.db.Exec(`UPDATE reviews SET structured_output=? WHERE job_id=?`, "\n  "+snapshotDocument+"\n", env.job.ID)
		require.NoError(t, err)
		return handoffBuilder(ctx, c)
	})
	require.NoError(t, env.db.CaptureKataHandoffs(t.Context(), env.job.ID))
	records, err := env.db.KataHandoffs(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.Len(t, records, 1)
	for _, entry := range records[0].Request.Entries {
		snapshot, err := env.db.FindingSnapshot(t.Context(), entry.Selector)
		require.NoError(t, err)
		assert.Equal(t, entry.Body, snapshot.Finding.Problem)
	}
}

func TestKataConcurrentClaimsChooseOneWorker(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/handoff-concurrent", "concurrent123")
	env.db.SetKataHandoffBuilder(handoffBuilder)
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	start := make(chan struct{})
	type result struct {
		handoff *KataHandoff
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			h, err := env.db.ClaimKataHandoff(context.Background(), time.Now())
			results <- result{h, err}
		}()
	}
	close(start)
	var claimed []*KataHandoff
	for range 2 {
		r := <-results
		require.NoError(t, r.err)
		if r.handoff != nil {
			claimed = append(claimed, r.handoff)
		}
	}
	require.Len(t, claimed, 1)
	assert.Equal(t, 1, claimed[0].Attempts)
}

func TestKataFailedCaptureRejectsRunChangedDuringResolution(t *testing.T) {
	t.Parallel()
	for _, transition := range []string{"requeue", "rerun-failed"} {
		t.Run(transition, func(t *testing.T) {
			env := setupJobEnv(t, t.TempDir(), "capture-race")
			claimJob(t, env.db, "worker")
			updated, err := env.db.FailJob(env.job.ID, "worker", "original failure")
			require.NoError(t, err)
			require.True(t, updated)
			env.db.SetKataHandoffBuilder(func(_ context.Context, c KataCompletion) ([]katapublish.Request, error) {
				require.NoError(t, env.db.ReenqueueJob(c.Job.ID, ReenqueueOpts{}))
				if transition == "rerun-failed" {
					claimJob(t, env.db, "worker")
					env.db.SetKataHandoffBuilder(nil)
					updated, err := env.db.FailJob(c.Job.ID, "worker", "new failure")
					require.NoError(t, err)
					require.True(t, updated)
				}
				return handoffBuilder(t.Context(), c)
			})
			require.ErrorIs(t, env.db.CaptureKataHandoffs(t.Context(), env.job.ID), ErrFindingChanged)
			records, err := env.db.KataHandoffs(t.Context(), env.job.ID)
			require.NoError(t, err)
			assert.Empty(t, records)
		})
	}
}

func TestKataFailureTransitionRejectsRunChangedDuringResolution(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "failure-race")
	job := claimJob(t, env.db, "worker")
	env.db.SetKataHandoffBuilder(func(_ context.Context, c KataCompletion) ([]katapublish.Request, error) {
		env.db.SetKataHandoffBuilder(nil)
		updated, err := env.db.FailJob(c.Job.ID, "worker", "newer terminal result")
		require.NoError(t, err)
		require.True(t, updated)
		require.NoError(t, env.db.ReenqueueJob(c.Job.ID, ReenqueueOpts{}))
		claimJob(t, env.db, "worker") // The same worker can own the next run.
		return handoffBuilder(t.Context(), c)
	})
	updated, err := env.db.FailJob(job.ID, "worker", "stale failure")
	require.NoError(t, err)
	assert.False(t, updated)
	current, err := env.db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusRunning, current.Status)
	records, err := env.db.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestKataFailedBackfillAndBlockedFallbackFollowCurrentRun(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "failed-backfill")
	valid := func(ctx context.Context, c KataCompletion) ([]katapublish.Request, error) {
		r, err := handoffBuilder(ctx, c)
		r[0].OperationID = "publication:" + c.Job.EnqueuedAt.UTC().Format(time.RFC3339Nano)
		r[0].Create.Body = c.Error
		return r, err
	}
	var original []KataHandoff
	for i, diagnostic := range []string{"first failure", "second failure"} {
		env.db.SetKataHandoffBuilder(func(context.Context, KataCompletion) ([]katapublish.Request, error) {
			return nil, errors.New("invalid hook configuration")
		})
		claimJob(t, env.db, "worker")
		updated, err := env.db.FailJob(env.job.ID, "worker", diagnostic)
		require.NoError(t, err)
		require.True(t, updated)
		records, err := env.db.KataHandoffs(t.Context(), env.job.ID)
		require.NoError(t, err)
		require.Len(t, records, i*2+1, "each failed run needs its own blocked diagnostic")
		env.db.SetKataHandoffBuilder(valid)
		require.NoError(t, env.db.CaptureKataHandoffs(t.Context(), env.job.ID))
		require.NoError(t, env.db.CaptureKataHandoffs(t.Context(), env.job.ID))
		records, err = env.db.KataHandoffs(t.Context(), env.job.ID)
		require.NoError(t, err)
		require.Len(t, records, (i+1)*2, "old failed publications cannot mask this run")
		var bodies []string
		for _, r := range records {
			if r.Request.Policy != "blocked" {
				bodies = append(bodies, r.Request.Create.Body)
			}
		}
		assert.Contains(t, bodies, diagnostic)
		if i == 0 {
			original = records
			require.NoError(t, env.db.ReenqueueJob(env.job.ID, ReenqueueOpts{}))
		} else {
			for _, old := range original {
				assert.Contains(t, records, old)
			}
		}
	}
}

func TestKataFailureFingerprintConflictRollsBackTerminalTransition(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "failure-conflict")
	env.db.SetKataHandoffBuilder(func(ctx context.Context, c KataCompletion) ([]katapublish.Request, error) {
		r, err := handoffBuilder(ctx, c) // Deliberately violates identity across runs.
		r[0].Create.Body = c.Error
		return r, err
	})
	claimJob(t, env.db, "worker")
	updated, err := env.db.FailJob(env.job.ID, "worker", "first failure")
	require.NoError(t, err)
	require.True(t, updated)
	old, err := env.db.KataHandoffs(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.NoError(t, env.db.ReenqueueJob(env.job.ID, ReenqueueOpts{}))
	claimJob(t, env.db, "worker")
	updated, err = env.db.FailJob(env.job.ID, "worker", "changed failure")
	require.ErrorContains(t, err, "fingerprint mismatch")
	assert.False(t, updated)
	job, err := env.db.GetJobByID(env.job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusRunning, job.Status)
	records, err := env.db.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, old, records)
}

func TestDeleteRepoRemovesKataHandoffsWithoutForeignKeys(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/handoff-deletion", "delete123")
	env.db.SetKataHandoffBuilder(handoffBuilder)
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	env.db.SetMaxOpenConns(1)
	_, err := env.db.Exec(`PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	require.NoError(t, env.db.DeleteRepo(env.job.RepoID, true))
	var count int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM kata_handoffs`).Scan(&count))
	assert.Zero(t, count)
}
