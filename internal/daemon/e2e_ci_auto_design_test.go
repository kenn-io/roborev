package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// ciAutoDesignE2E is the CI-poller counterpart of autoDesignE2E.
// It wires a real git repo + DB + CIPoller so the tests can call
// maybeDispatchAutoDesignForCI against real commits and assert the
// resulting batch attachments.
type ciAutoDesignE2E struct {
	t      *testing.T
	repo   *testutil.TestRepo
	db     *storage.DB
	poller *CIPoller
	row    *storage.Repo
}

func newCIAutoDesignE2E(t *testing.T) *ciAutoDesignE2E {
	t.Helper()
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)

	repo := testutil.NewTestRepoWithCommit(t)

	// Opt-in per-repo config.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"),
		[]byte(`agent = "test"

[auto_design_review]
enabled = true
`), 0o644))
	repo.RunGit("add", ".roborev.toml")
	repo.RunGit("commit", "-m", "chore: enable auto design review")

	db := testutil.OpenTestDB(t)
	row, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)

	cfg := config.DefaultConfig()
	cfg.CI.Enabled = true
	poller := NewCIPoller(db, NewStaticConfig(cfg), nil)

	return &ciAutoDesignE2E{t: t, repo: repo, db: db, poller: poller, row: row}
}

// createBatchFor simulates what processPR does before calling
// maybeDispatchAutoDesignForCI: a batch row exists for the PR head.
// Returns the batch id the auto-design dispatch should attach to.
func (c *ciAutoDesignE2E) createBatchFor(headSHA string, totalJobs int) *storage.CIPRBatch {
	c.t.Helper()
	batch, _, err := c.db.CreateCIBatch("acme/api", 42, headSHA, totalJobs)
	require.NoError(c.t, err)
	return batch
}

func TestE2ECIAutoDesign_HeuristicTrigger_AttachesToBatch(t *testing.T) {
	c := newCIAutoDesignE2E(t)

	// A migration commit — path-triggers the heuristic.
	sha := c.repo.CommitFile("db/migrations/009_users.sql",
		"CREATE TABLE users(id INT);\n",
		"feat: add users table")

	// Existing matrix jobs would already be linked; start with total=0
	// so we can verify the auto-design attach bumps it to 1.
	batch := c.createBatchFor(sha, 0)

	require.NoError(t, c.poller.maybeDispatchAutoDesignForCI(
		context.Background(), c.row, sha, batch.ID))

	// The auto-design row exists and is marked auto_design.
	got := c.waitForAutoDesign(sha, func(*storage.ReviewJob) bool { return true })
	require.NotNil(t, got)
	assert := assert.New(t)
	assert.Equal("review", got.JobType)
	assert.Equal("design", got.ReviewType)
	assert.Equal("auto_design", got.Source)

	// Batch attach: total_jobs bumped to 1, linked via ci_pr_batch_jobs.
	var total, linked int
	require.NoError(t, c.db.QueryRow(`SELECT total_jobs FROM ci_pr_batches WHERE id = ?`, batch.ID).Scan(&total))
	assert.Equal(1, total)
	require.NoError(t, c.db.QueryRow(`
		SELECT COUNT(*) FROM ci_pr_batch_jobs WHERE batch_id = ? AND job_id = ?
	`, batch.ID, got.ID).Scan(&linked))
	assert.Equal(1, linked)
}

func TestE2ECIAutoDesign_HeuristicSkip_AttachesSkippedRow(t *testing.T) {
	c := newCIAutoDesignE2E(t)

	// Docs commit — heuristic skip path. Multi-line so it clears
	// MinDiffLines (otherwise "trivial" would fire first).
	var body strings.Builder
	for range 25 {
		body.WriteString("notes\n")
	}
	sha := c.repo.CommitFile("README.md", body.String(), "update readme notes")

	batch := c.createBatchFor(sha, 0)

	require.NoError(t, c.poller.maybeDispatchAutoDesignForCI(
		context.Background(), c.row, sha, batch.ID))

	got := c.waitForAutoDesign(sha, func(*storage.ReviewJob) bool { return true })
	require.NotNil(t, got)
	assert := assert.New(t)
	// Skipped rows still attach so reconciliation doesn't finish early.
	assert.Equal(storage.JobStatusSkipped, got.Status)
	assert.Contains(got.SkipReason, "doc/test-only")

	var total int
	require.NoError(t, c.db.QueryRow(`SELECT total_jobs FROM ci_pr_batches WHERE id = ?`, batch.ID).Scan(&total))
	assert.Equal(1, total, "skipped auto_design rows must count toward total_jobs")
}

func TestE2ECIAutoDesign_ExistingRow_AttachedToNewBatch(t *testing.T) {
	c := newCIAutoDesignE2E(t)

	sha := c.repo.CommitFile("db/migrations/010_orders.sql",
		"CREATE TABLE orders(id INT);\n",
		"feat: add orders table")

	// First batch (simulating first CI poll).
	batch1 := c.createBatchFor(sha, 0)
	require.NoError(t, c.poller.maybeDispatchAutoDesignForCI(
		context.Background(), c.row, sha, batch1.ID))
	got1 := c.waitForAutoDesign(sha, func(*storage.ReviewJob) bool { return true })
	require.NotNil(t, got1)

	// Second batch (simulating a later CI poll on the same head SHA —
	// e.g., after superseded-batch cleanup). HasAutoDesignSlotForCommit
	// returns true, so the early-return path fires. But the existing
	// auto_design row must still get attached to the new batch;
	// otherwise batch2 would finish synthesis without waiting for the
	// design review.
	batch2 := c.createBatchFor(sha+"-b", 0) // different head_sha to sidestep the batch uniqueness constraint
	// Use a different head_sha via a helper: CreateCIBatch is keyed on
	// (github_repo, pr_number, head_sha). We re-use the same repo+PR
	// combo for batch2 by pointing it at a new head_sha just for this
	// test. The auto_design row lookup keys on the CURRENT head, so
	// we need to run against sha, not sha+"-b". Use sha for the
	// dispatch — batch2 isn't strictly second "for this commit" in
	// the CI sense, but it exercises the attach-existing path.
	_ = batch2 // not used

	// Instead: run the dispatch against a fresh batch for the SAME sha
	// via a second CreateCIBatch call with an incremented pr_number.
	batch2b, _, err := c.db.CreateCIBatch("acme/api", 43, sha, 0)
	require.NoError(t, err)
	require.NoError(t, c.poller.maybeDispatchAutoDesignForCI(
		context.Background(), c.row, sha, batch2b.ID))

	// The same auto_design row (got1.ID) must be linked to both
	// batches, and total_jobs on batch2b must be 1.
	var total2 int
	require.NoError(t, c.db.QueryRow(`SELECT total_jobs FROM ci_pr_batches WHERE id = ?`, batch2b.ID).Scan(&total2))
	assert.Equal(t, 1, total2)

	var linked2 int
	require.NoError(t, c.db.QueryRow(`
		SELECT COUNT(*) FROM ci_pr_batch_jobs WHERE batch_id = ? AND job_id = ?
	`, batch2b.ID, got1.ID).Scan(&linked2))
	assert.Equal(t, 1, linked2, "existing auto_design row must attach to the new batch")

	// Exactly one auto_design row exists for this commit — the second
	// dispatch must NOT create a duplicate.
	var rowCount int
	require.NoError(t, c.db.QueryRow(`
		SELECT COUNT(*) FROM review_jobs rj JOIN commits c ON rj.commit_id = c.id
		WHERE rj.repo_id = ? AND c.sha = ? AND rj.source = 'auto_design'
	`, c.row.ID, sha).Scan(&rowCount))
	assert.Equal(t, 1, rowCount)
}

func TestE2ECIAutoDesign_DisabledNoOp(t *testing.T) {
	// Set up a CI e2e but overwrite .roborev.toml to disable the
	// feature. No auto_design row must appear; the batch total stays
	// where it was.
	c := newCIAutoDesignE2E(t)
	require.NoError(t, os.WriteFile(filepath.Join(c.repo.Path(), ".roborev.toml"),
		[]byte(`agent = "test"

[auto_design_review]
enabled = false
`), 0o644))
	c.repo.RunGit("add", ".roborev.toml")
	c.repo.RunGit("commit", "-m", "chore: disable auto design review")

	sha := c.repo.CommitFile("db/migrations/099.sql", "SELECT 1;\n", "feat: migration")

	batch := c.createBatchFor(sha, 0)
	require.NoError(t, c.poller.maybeDispatchAutoDesignForCI(
		context.Background(), c.row, sha, batch.ID))

	// No auto_design row in any state.
	for _, st := range []storage.JobStatus{
		storage.JobStatusQueued, storage.JobStatusRunning,
		storage.JobStatusDone, storage.JobStatusFailed,
		storage.JobStatusSkipped,
	} {
		jobs, err := c.db.ListJobsByStatus(c.row.ID, st)
		require.NoError(t, err)
		for _, j := range jobs {
			assert.NotEqual(t, "auto_design", j.Source)
		}
	}
	// total_jobs unchanged.
	var total int
	require.NoError(t, c.db.QueryRow(`SELECT total_jobs FROM ci_pr_batches WHERE id = ?`, batch.ID).Scan(&total))
	assert.Equal(t, 0, total)
}

// waitForAutoDesign mirrors the helper on autoDesignE2E.
func (c *ciAutoDesignE2E) waitForAutoDesign(sha string, pred func(*storage.ReviewJob) bool) *storage.ReviewJob {
	c.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, st := range []storage.JobStatus{
			storage.JobStatusQueued,
			storage.JobStatusRunning,
			storage.JobStatusDone,
			storage.JobStatusFailed,
			storage.JobStatusSkipped,
		} {
			jobs, err := c.db.ListJobsByStatus(c.row.ID, st)
			require.NoError(c.t, err)
			for i := range jobs {
				j := jobs[i]
				if j.GitRef == sha && j.ReviewType == "design" && j.Source == "auto_design" && pred(&j) {
					return &j
				}
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestListCommitsInRange_MultiCommit(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := repo.HeadSHA()

	var expected []string
	for i := range 3 {
		sha := repo.CommitFile(fmt.Sprintf("f%d.txt", i),
			fmt.Sprintf("file %d\n", i),
			fmt.Sprintf("feat: file %d", i))
		expected = append(expected, sha)
	}
	head := expected[len(expected)-1]

	got, err := listCommitsInRange(repo.Path(), base, head)
	require.NoError(t, err)
	assert.Equal(t, expected, got, "listCommitsInRange must return oldest-first SHAs in base..head")
}

func TestListCommitsInRange_EmptyRangeFallsBackToHead(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.HeadSHA()

	// base == head → empty range
	got, err := listCommitsInRange(repo.Path(), sha, sha)
	require.NoError(t, err)
	assert.Equal(t, []string{sha}, got, "empty range must fall back to head SHA")
}
