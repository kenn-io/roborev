//go:build integration

package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type remapContext struct {
	server  *Server
	db      *storage.DB
	repo    *testutil.TestRepo
	oldSHA  string
	patchID string
	jobID   int64
}

func newRemapFixture(t *testing.T) *remapContext {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	return &remapContext{
		server: server,
		db:     db,
		repo:   repo,
	}
}

func (ctx *remapContext) seedCompletedReview(t *testing.T) {
	t.Helper()

	dbRepo, err := ctx.db.GetOrCreateRepo(ctx.repo.Path())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo: %v", err)
	}

	ctx.oldSHA = ctx.repo.HeadSHA()
	ctx.patchID = gitpkg.GetPatchID(ctx.repo.Path(), ctx.oldSHA)
	if ctx.patchID == "" {
		require.Condition(t, func() bool {
			return false
		}, "expected non-empty patch-id for feature commit")
	}

	// Create commit record
	commit, err := ctx.db.GetOrCreateCommit(
		dbRepo.ID, ctx.oldSHA, "Test", "setup message", time.Now(),
	)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit: %v", err)
	}

	// Enqueue job
	job, err := ctx.db.EnqueueJob(storage.EnqueueOpts{
		RepoID:   dbRepo.ID,
		CommitID: commit.ID,
		GitRef:   ctx.oldSHA,
		Agent:    "test",
		PatchID:  ctx.patchID,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false

			// Claim and complete job
		}, "EnqueueJob: %v", err)
	}

	_, err = ctx.db.ClaimJob("worker-int")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob: %v", err)
	}
	err = ctx.db.CompleteJob(job.ID, "test", "prompt", "LGTM - no issues found")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob: %v", err)
	}

	ctx.jobID = job.ID
}

type remapResponse struct {
	Remapped int `json:"remapped"`
}

func executeRemapRequest(t *testing.T, ctx *remapContext, newSHA string) remapResponse {
	t.Helper()

	info, err := gitpkg.GetCommitInfo(ctx.repo.Path(), newSHA)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetCommitInfo: %v", err)
	}

	// POST /api/remap
	reqData := RemapRequest{
		RepoPath: ctx.repo.Path(),
		Mappings: []RemapMapping{
			{
				OldSHA:    ctx.oldSHA,
				NewSHA:    newSHA,
				PatchID:   ctx.patchID,
				Author:    info.Author,
				Subject:   info.Subject,
				Timestamp: info.Timestamp.Format(time.RFC3339),
			},
		},
	}
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/remap", reqData)
	w := httptest.NewRecorder()
	ctx.server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		require.Condition(t, func() bool {
			return false
		}, "expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result remapResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		require.Condition(t, func() bool {
			return false
		}, err)
	}
	return result
}

func assertJobUpdated(t *testing.T, db *storage.DB, jobID int64, expectedSHA string) {
	t.Helper()
	updatedJob, err := db.GetJobByID(jobID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetJobByID: %v", err)
	}
	if updatedJob.GitRef != expectedSHA {
		assert.Condition(t, func() bool {
			return false
		}, "expected git_ref=%s, got %s", expectedSHA, updatedJob.GitRef)
	}
}

func assertReviewReachable(t *testing.T, db *storage.DB, newSHA string) {
	t.Helper()
	review, err := db.GetReviewByCommitSHA(newSHA)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetReviewByCommitSHA(%s): %v", newSHA, err)
	}
	if review == nil {
		require.Condition(t, func() bool {
			return false
		}, "review should be reachable via new SHA after remap")
	}
	if !strings.Contains(review.Output, "LGTM") {
		assert.Condition(t, func() bool {
			return false
		}, "unexpected review output: %s", review.Output)
	}
}

// TestRemapAfterRebase exercises the rebase remap flow by calling
// the HTTP handler directly with real git operations. This validates
// the full DB flow (patch-id matching, commit creation, git_ref
// update) without requiring a running daemon. End-to-end testing
// through the actual post-rewrite hook → CLI → daemon path is
// covered by manual testing (see design doc verification section).
func TestRemapAfterRebase(t *testing.T) {
	fixture := newRemapFixture(t)

	// 1. Setup Git state directly
	fixture.repo.CommitFile("base.txt", "base content", "initial commit")
	// Create feature branch with a commit
	fixture.repo.Run("checkout", "-b", "feature")
	fixture.repo.CommitFile("feature.txt", "feature content", "add feature")

	// 2. Seed the database based on the current Git state
	fixture.seedCompletedReview(t)

	// Advance main so rebase has work to do
	fixture.repo.Run("checkout", "main")
	fixture.repo.CommitFile("main2.txt", "more main", "advance main")

	// Rebase feature onto main
	fixture.repo.Run("checkout", "feature")
	fixture.repo.Run("rebase", "main")
	newSHA := fixture.repo.HeadSHA()

	if fixture.oldSHA == newSHA {
		require.Condition(t, func() bool {
			return false
		}, "SHAs should differ after rebase")
	}

	newPatchID := gitpkg.GetPatchID(fixture.repo.Path(), newSHA)
	if fixture.patchID != newPatchID {
		require.Condition(t, func() bool {
			return false
		}, "patch-ids should match after clean rebase: %s != %s",
			fixture.patchID, newPatchID)
	}

	// Act
	response := executeRemapRequest(t, fixture, newSHA)
	if response.Remapped != 1 {
		require.Condition(t, func() bool {
			return false
		}, "expected remapped=1, got %d", response.Remapped)
	}

	// Assert
	assertJobUpdated(t, fixture.db, fixture.jobID, newSHA)
	assertReviewReachable(t, fixture.db, newSHA)
}

// TestRemapAfterAmendMessageOnly exercises the message-only amend flow.
func TestRemapAfterAmendMessageOnly(t *testing.T) {
	fixture := newRemapFixture(t)

	// 1. Setup Git state directly
	fixture.repo.CommitFile("file.txt", "content", "original message")

	// 2. Seed the database based on the current Git state
	fixture.seedCompletedReview(t)

	// Amend message only
	fixture.repo.Run("commit", "--amend", "-m", "amended message")
	newSHA := fixture.repo.HeadSHA()

	if fixture.oldSHA == newSHA {
		require.Condition(t, func() bool {
			return false
		}, "SHAs should differ after amend")
	}

	newPatchID := gitpkg.GetPatchID(fixture.repo.Path(), newSHA)
	if fixture.patchID != newPatchID {
		require.Condition(t, func() bool {
			return false
		}, "patch-ids should match for message-only amend: %s != %s",
			fixture.patchID, newPatchID)
	}

	// Act
	response := executeRemapRequest(t, fixture, newSHA)
	if response.Remapped != 1 {
		require.Condition(t, func() bool {
			return false
		}, "expected remapped=1, got %d", response.Remapped)
	}

	// Assert
	assertJobUpdated(t, fixture.db, fixture.jobID, newSHA)
	assertReviewReachable(t, fixture.db, newSHA)

	// Old SHA should no longer find the review
	oldReview, err := fixture.db.GetReviewByCommitSHA(fixture.oldSHA)
	if err == nil && oldReview != nil {
		assert.Condition(t, func() bool {
			return false
		}, "old SHA should not find the review after remap")
	}
}
