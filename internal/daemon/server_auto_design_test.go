package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func newAutoDesignTestServer(t *testing.T) (*Server, *storage.Repo) {
	t.Helper()
	db, repoPath := testutil.OpenTestDBWithDir(t)
	testutil.InitTestGitRepo(t, repoPath)

	cfg := config.DefaultConfig()
	srv := NewServer(db, cfg, "")

	repo, err := db.GetOrCreateRepo(repoPath)
	require.NoError(t, err)
	return srv, repo
}

func enableAutoDesignReviewForRepo(t *testing.T, repoPath string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, ".roborev.toml"),
		[]byte(`[auto_design_review]
enabled = true
`), 0o644))
}

func TestMaybeDispatchAutoDesign_HeuristicTrigger(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)
	enableAutoDesignReviewForRepo(t, repo.RootPath)

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "feedcafe", "Author", "refactor: rework auth", time.Now())
	require.NoError(t, err)

	// Use a message-pattern trigger so we don't depend on git ChangedFiles
	// (which requires a real SHA in the test repo).
	diff := "+x\n+y\n+a\n+b\n+c\n+d\n+e\n+f\n+g\n+h\n+i\n+j\n+k\n+l\n"
	parent := &storage.ReviewJob{
		ID:            999,
		RepoID:        repo.ID,
		CommitID:      &commit.ID,
		GitRef:        "feedcafe",
		Agent:         "test",
		JobType:       storage.JobTypeReview,
		ReviewType:    "",
		Status:        storage.JobStatusQueued,
		EnqueuedAt:    time.Now(),
		RepoPath:      repo.RootPath,
		CommitSubject: "refactor: rework auth",
		DiffContent:   &diff,
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	queued, err := srv.db.ListJobsByStatus(repo.ID, storage.JobStatusQueued)
	require.NoError(t, err)
	var found *storage.ReviewJob
	for i := range queued {
		j := queued[i]
		if j.GitRef == "feedcafe" && j.ReviewType == "design" && j.Source == "auto_design" {
			found = &j
			break
		}
	}
	require.NotNil(t, found, "expected an auto_design design row")
	assert.Equal(t, "review", found.JobType, "direct heuristic trigger enqueues a design review, not a classify")
}

func TestMaybeDispatchAutoDesign_HeuristicSkip_TrivialDiff(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)
	enableAutoDesignReviewForRepo(t, repo.RootPath)

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "beefc0de", "Author", "fix: tiny", time.Now())
	require.NoError(t, err)
	tiny := "+x\n"
	parent := &storage.ReviewJob{
		ID:            999,
		RepoID:        repo.ID,
		CommitID:      &commit.ID,
		GitRef:        "beefc0de",
		Agent:         "test",
		JobType:       storage.JobTypeReview,
		Status:        storage.JobStatusQueued,
		EnqueuedAt:    time.Now(),
		RepoPath:      repo.RootPath,
		CommitSubject: "fix: tiny",
		DiffContent:   &tiny,
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	skipped, err := srv.db.ListJobsByStatus(repo.ID, storage.JobStatusSkipped)
	require.NoError(t, err)
	var found *storage.ReviewJob
	for i := range skipped {
		j := skipped[i]
		if j.GitRef == "beefc0de" && j.ReviewType == "design" && j.Source == "auto_design" {
			found = &j
			break
		}
	}
	require.NotNil(t, found, "expected a skipped auto_design design row")
	assert.Contains(t, found.SkipReason, "trivial")
}

func TestAutoDesignMetrics_RecordHeuristic(t *testing.T) {
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)

	srv, repo := newAutoDesignTestServer(t)
	enableAutoDesignReviewForRepo(t, repo.RootPath)

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "feedcafe", "Author", "refactor: rework", time.Now())
	require.NoError(t, err)
	bigDiff := "+x\n+y\n+a\n+b\n+c\n+d\n+e\n+f\n+g\n+h\n+i\n+j\n+k\n+l\n"
	parent := &storage.ReviewJob{
		RepoID: repo.ID, CommitID: &commit.ID, GitRef: "feedcafe",
		JobType: storage.JobTypeReview, Status: storage.JobStatusQueued,
		EnqueuedAt: time.Now(), RepoPath: repo.RootPath,
		CommitSubject: "refactor: rework", DiffContent: &bigDiff,
	}
	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	snap := AutoDesignMetricsSnapshot()
	assert.EqualValues(t, 1, snap.TriggeredHeuristic)
	assert.EqualValues(t, 0, snap.SkippedHeuristic)
}

func TestAutoDesignStatusForResponse_DisabledOmitted(t *testing.T) {
	ResetAutoDesignMetricsForTest()
	srv, _ := newAutoDesignTestServer(t)
	// No repo enables auto_design_review.
	got := srv.autoDesignStatusForResponse()
	assert.Nil(t, got)
}

func TestAutoDesignStatusForResponse_EnabledRepoSurfaces(t *testing.T) {
	ResetAutoDesignMetricsForTest()
	srv, repo := newAutoDesignTestServer(t)
	enableAutoDesignReviewForRepo(t, repo.RootPath)

	got := srv.autoDesignStatusForResponse()
	require.NotNil(t, got)
	assert.True(t, got.Enabled)
}

func TestMaybeDispatchAutoDesign_InvalidConfig_NoRow(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)
	// Enable auto-design but poison the heuristics with an invalid glob.
	// An unvalidated typo previously silently inserted a skipped row with
	// reason "auto-design: heuristic error" — this confirms the new
	// validation path logs and no-ops instead.
	require.NoError(t, os.WriteFile(filepath.Join(repo.RootPath, ".roborev.toml"),
		[]byte("[auto_design_review]\nenabled = true\ntrigger_paths = [\"[\"]\n"), 0o644))

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "deadbeef", "Author", "feat: x", time.Now())
	require.NoError(t, err)
	d := "+x\n+y\n+a\n+b\n+c\n+d\n+e\n+f\n+g\n+h\n+i\n+j\n"
	parent := &storage.ReviewJob{
		RepoID: repo.ID, CommitID: &commit.ID, GitRef: "deadbeef",
		JobType: storage.JobTypeReview, Status: storage.JobStatusQueued,
		EnqueuedAt: time.Now(), RepoPath: repo.RootPath,
		CommitSubject: "feat: x", DiffContent: &d,
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	for _, s := range []storage.JobStatus{storage.JobStatusQueued, storage.JobStatusSkipped} {
		jobs, err := srv.db.ListJobsByStatus(repo.ID, s)
		require.NoError(t, err)
		for _, j := range jobs {
			assert.NotEqual(t, "auto_design", j.Source,
				"invalid config must not create an auto_design row (status=%s, reason=%q)",
				s, j.SkipReason)
		}
	}
}

func TestMaybeDispatchAutoDesign_Disabled_NoOp(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)
	// Don't enable.

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "abc", "Author", "feat: x", time.Now())
	require.NoError(t, err)
	d := "+x\n"
	parent := &storage.ReviewJob{
		RepoID: repo.ID, CommitID: &commit.ID, GitRef: "abc",
		JobType: storage.JobTypeReview, Status: storage.JobStatusQueued,
		EnqueuedAt: time.Now(), RepoPath: repo.RootPath,
		CommitSubject: "feat: x", DiffContent: &d,
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	for _, s := range []storage.JobStatus{storage.JobStatusQueued, storage.JobStatusSkipped} {
		jobs, err := srv.db.ListJobsByStatus(repo.ID, s)
		require.NoError(t, err)
		for _, j := range jobs {
			assert.NotEqual(t, "auto_design", j.Source, "no auto-design rows should exist when disabled")
		}
	}
}
