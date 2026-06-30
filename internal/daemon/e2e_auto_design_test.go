package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// autoDesignE2E wraps a real git repo, a DB, and a Server for
// end-to-end testing of the auto-design path. Mirrors what a user
// would do in a throwaway repo (git init, enable auto-design in
// .roborev.toml, commit, enqueue).
type autoDesignE2E struct {
	t    *testing.T
	repo *testutil.TestRepo
	db   *storage.DB
	srv  *Server
	row  *storage.Repo // the registered repo row
}

func newAutoDesignE2E(t *testing.T) *autoDesignE2E {
	t.Helper()
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)

	// A real git repo on disk, with an initial commit so later commits
	// resolve against a non-empty base.
	repo := testutil.NewTestRepoWithCommit(t)

	// Per-repo .roborev.toml — this is how a user opts in per the user
	// docs. Pin agent="test" so the primary review doesn't try to spin
	// up a real CLI. Commit it separately so subsequent `git add .`
	// doesn't sweep it into the tests' commits (which would poison
	// path-based heuristics).
	repo.CommitFile(".roborev.toml", `agent = "test"

[auto_design_review]
enabled = true
`, "chore: enable auto design review")

	db, _ := testutil.OpenTestDBWithDir(t)
	row, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)

	cfg := config.DefaultConfig()
	cfg.MaxWorkers = 1
	srv := NewServer(db, cfg, "")

	return &autoDesignE2E{t: t, repo: repo, db: db, srv: srv, row: row}
}

func (e *autoDesignE2E) startWorkers() {
	e.t.Helper()
	e.srv.workerPool.Start()
	e.t.Cleanup(func() { e.srv.workerPool.Stop() })
}

func (e *autoDesignE2E) completeParentReview(job *storage.ReviewJob) {
	e.t.Helper()
	_, err := e.db.Exec(`UPDATE review_jobs SET status = ? WHERE id = ?`, storage.JobStatusDone, job.ID)
	require.NoError(e.t, err)
}

// enqueueReviewFor creates a primary review job the way the HTTP
// enqueue handler does and runs maybeDispatchAutoDesign against it.
// Returns the primary job so callers can assert on it if needed.
func (e *autoDesignE2E) enqueueReviewFor(sha, subject string) *storage.ReviewJob {
	e.t.Helper()
	commit, err := e.db.GetOrCreateCommit(e.row.ID, sha, "Author", subject, time.Now())
	require.NoError(e.t, err)
	job, err := e.db.EnqueueJob(storage.EnqueueOpts{
		RepoID:   e.row.ID,
		CommitID: commit.ID,
		GitRef:   sha,
		Agent:    "test",
	})
	require.NoError(e.t, err)
	job.RepoPath = e.row.RootPath
	job.CommitSubject = subject

	// Run the auto-design dispatch the way server.go does it after the
	// primary job is enqueued.
	require.NoError(e.t, e.srv.maybeDispatchAutoDesign(context.Background(), job))
	return job
}

// eventuallyFindAutoDesign polls for the auto-design outcome row
// (source='auto_design', review_type='design', matching git_ref).
// Returns the first matching row found; caller is responsible for
// waiting for further state transitions via eventuallyAutoDesignMatches.
func (e *autoDesignE2E) eventuallyFindAutoDesign(sha string) *storage.ReviewJob {
	e.t.Helper()
	row := e.waitForAutoDesign(sha, func(*storage.ReviewJob) bool { return true })
	if row == nil {
		e.t.Fatalf("no auto_design row for %s within deadline", sha)
	}
	return row
}

// eventuallyAutoDesignMatches polls until the auto_design row for sha
// satisfies pred, or the deadline fires.
func (e *autoDesignE2E) eventuallyAutoDesignMatches(sha, desc string, pred func(*storage.ReviewJob) bool) *storage.ReviewJob {
	e.t.Helper()
	row := e.waitForAutoDesign(sha, pred)
	if row == nil {
		e.t.Fatalf("auto_design row for %s never matched %s within deadline", sha, desc)
	}
	return row
}

func (e *autoDesignE2E) waitForAutoDesign(sha string, pred func(*storage.ReviewJob) bool) *storage.ReviewJob {
	e.t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		for _, st := range []storage.JobStatus{
			storage.JobStatusQueued,
			storage.JobStatusRunning,
			storage.JobStatusDone,
			storage.JobStatusFailed,
			storage.JobStatusSkipped,
		} {
			jobs, err := e.db.ListJobsByStatus(e.row.ID, st)
			require.NoError(e.t, err)
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

func TestE2EAutoDesign_HeuristicTrigger_Migration(t *testing.T) {
	e := newAutoDesignE2E(t)

	sha := e.repo.CommitFile("db/migrations/001_users.sql",
		"CREATE TABLE users (id INT);\n",
		"feat: add users table")

	e.enqueueReviewFor(sha, "feat: add users table")

	got := e.eventuallyFindAutoDesign(sha)
	assert := assert.New(t)
	assert.Equal("review", got.JobType, "heuristic trigger produces a design review, not a classify row")
	assert.NotEqual(storage.JobStatusSkipped, got.Status, "migration path must not skip")
	assert.Empty(got.SkipReason, "trigger path must not carry a skip reason")

	snap := AutoDesignMetricsSnapshot()
	assert.EqualValues(1, snap.TriggeredHeuristic)
	assert.EqualValues(0, snap.SkippedHeuristic)
}

func TestE2EAutoDesign_HeuristicTrigger_MessageRegex(t *testing.T) {
	e := newAutoDesignE2E(t)

	// Non-path, non-size trigger — commit subject matches the refactor
	// regex. Diff needs to be above MinDiffLines so the skip-path
	// "trivial diff" doesn't fire first.
	var body strings.Builder
	for range 20 {
		body.WriteString("line\n")
	}
	sha := e.repo.CommitFile("src/auth.go", "package auth\n\n"+body.String(),
		"refactor: rework auth layer")

	e.enqueueReviewFor(sha, "refactor: rework auth layer")

	got := e.eventuallyFindAutoDesign(sha)
	assert := assert.New(t)
	assert.Equal("review", got.JobType)
	assert.NotEqual(storage.JobStatusSkipped, got.Status)
	assert.Contains(got.Source, "auto_design")
}

func TestE2EAutoDesign_HeuristicSkip_DocsOnly(t *testing.T) {
	e := newAutoDesignE2E(t)

	// docs-only files, but use a commit subject that won't match
	// skip_message_patterns (^(docs|test|style|chore)(\(.+\))?:).
	// Otherwise the commit-subject regex short-circuits before we
	// reach the path-based "doc/test-only" branch.
	var body strings.Builder
	for range 20 {
		body.WriteString("## heading\n")
	}
	sha := e.repo.CommitFiles(map[string]string{
		"README.md":    body.String(),
		"CHANGELOG.md": body.String(),
	}, "update readme and changelog")

	e.enqueueReviewFor(sha, "update readme and changelog")

	got := e.eventuallyFindAutoDesign(sha)
	assert := assert.New(t)
	assert.Equal(storage.JobStatusSkipped, got.Status)
	assert.Equal("review", got.JobType, "status=skipped rows still have job_type='review'")
	assert.NotEmpty(got.SkipReason)
	assert.Contains(got.SkipReason, "doc/test-only")

	snap := AutoDesignMetricsSnapshot()
	assert.EqualValues(1, snap.SkippedHeuristic)
}

func TestE2EAutoDesign_HeuristicSkip_ConventionalPrefix(t *testing.T) {
	// Separate coverage for the skip_message_patterns branch (docs:,
	// chore:, etc.) — asserting that a conventional-prefix commit
	// skips with the "conventional marker" reason.
	e := newAutoDesignE2E(t)

	var body strings.Builder
	for range 20 {
		body.WriteString("line\n")
	}
	sha := e.repo.CommitFile("src/dep.go", "package main\n\n"+body.String(),
		"chore: bump go.mod")

	e.enqueueReviewFor(sha, "chore: bump go.mod")

	got := e.eventuallyFindAutoDesign(sha)
	assert := assert.New(t)
	assert.Equal(storage.JobStatusSkipped, got.Status)
	assert.Contains(got.SkipReason, "conventional marker")
}

func TestE2EAutoDesign_HeuristicSkip_TrivialDiff(t *testing.T) {
	e := newAutoDesignE2E(t)

	sha := e.repo.CommitFile("a.txt", "x\n", "fix: oneliner")

	e.enqueueReviewFor(sha, "fix: oneliner")

	got := e.eventuallyFindAutoDesign(sha)
	assert := assert.New(t)
	assert.Equal(storage.JobStatusSkipped, got.Status)
	assert.Contains(got.SkipReason, "trivial")
}

func TestE2EAutoDesign_ClassifierPath_PromotesToDesignReview(t *testing.T) {
	e := newAutoDesignE2E(t)

	// Ambiguous change: not doc-only, not a migration, not a trigger
	// message, moderate size — hits the classifier fallback.
	var body strings.Builder
	for range 40 {
		body.WriteString("\tline\n")
	}
	sha := e.repo.CommitFile("src/helper.go", "package src\n\nfunc Helper() {\n"+body.String()+"}\n",
		"feat: small helper")

	// Pin the classifier verdict so we don't need a real agent CLI.
	SetTestClassifierVerdict(true, "new package detected")
	t.Cleanup(func() { SetTestClassifierVerdict(false, "") })

	parent := e.enqueueReviewFor(sha, "feat: small helper")
	e.completeParentReview(parent)
	e.startWorkers()

	// Wait for the worker to promote the classify row in place to a
	// real review row. The initial row appears immediately with
	// job_type='classify'; the promotion happens in a worker goroutine.
	got := e.eventuallyAutoDesignMatches(sha, "job_type='review' (promoted)",
		func(j *storage.ReviewJob) bool { return j.JobType == storage.JobTypeReview })
	assert := assert.New(t)
	assert.NotEqual(storage.JobStatusSkipped, got.Status,
		"yes verdict must produce a runnable design row")
	assert.Equal("auto_design", got.Source)

	// Exactly one auto_design row for this commit — a bug would have
	// left both a classify and a design row behind.
	var rowCount int
	require.NoError(t, e.db.QueryRow(`
		SELECT COUNT(*) FROM review_jobs rj
		JOIN commits c ON rj.commit_id = c.id
		WHERE rj.repo_id = ? AND c.sha = ? AND rj.source = 'auto_design'
	`, e.row.ID, sha).Scan(&rowCount))
	assert.Equal(1, rowCount)

	// Counters: the promotion increments TriggeredClassifier.
	// Wait briefly since the worker path runs in a goroutine.
	var snap storage.AutoDesignStatus
	for range 60 {
		snap = AutoDesignMetricsSnapshot()
		if snap.TriggeredClassifier > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	assert.EqualValues(1, snap.TriggeredClassifier)
}

func TestE2EAutoDesign_ClassifierPath_SkipsAmbiguous(t *testing.T) {
	e := newAutoDesignE2E(t)

	var body strings.Builder
	for range 40 {
		body.WriteString("\tfoo()\n")
	}
	sha := e.repo.CommitFile("src/rename.go", "package src\n\nfunc a() {\n"+body.String()+"}\n",
		"feat: rename var")

	SetTestClassifierVerdict(false, "local rename only")
	t.Cleanup(func() { SetTestClassifierVerdict(false, "") })

	parent := e.enqueueReviewFor(sha, "feat: rename var")
	e.completeParentReview(parent)
	e.startWorkers()

	// Wait for the worker to transition the classify row to skipped.
	got := e.eventuallyAutoDesignMatches(sha, "status=skipped",
		func(j *storage.ReviewJob) bool { return j.Status == storage.JobStatusSkipped })
	assert := assert.New(t)
	assert.Equal(storage.JobStatusSkipped, got.Status)
	assert.Equal("review", got.JobType)
	assert.Contains(got.SkipReason, "local rename")

	// Counter: classifier-no bumps SkippedClassifier. Race with the
	// worker goroutine — poll briefly.
	var snap storage.AutoDesignStatus
	for range 60 {
		snap = AutoDesignMetricsSnapshot()
		if snap.SkippedClassifier > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	assert.EqualValues(1, snap.SkippedClassifier)
	assert.EqualValues(0, snap.ClassifierFailed)
}

func TestE2EAutoDesign_LargeDiff_Trigger(t *testing.T) {
	e := newAutoDesignE2E(t)

	// >= LargeDiffLines (500) so the heuristic triggers on size alone,
	// independent of paths and messages. Use a neutral file name and
	// subject so neither trigger_paths nor trigger_message_patterns
	// short-circuits first.
	var body strings.Builder
	for range 600 {
		body.WriteString("x\n")
	}
	sha := e.repo.CommitFile("src/big.go", "package src\n\n"+body.String(),
		"feat: expand helper")

	e.enqueueReviewFor(sha, "feat: expand helper")

	got := e.eventuallyFindAutoDesign(sha)
	assert := assert.New(t)
	assert.Equal("review", got.JobType)
	assert.NotEqual(storage.JobStatusSkipped, got.Status)
	assert.Equal("auto_design", got.Source)
	assert.EqualValues(1, AutoDesignMetricsSnapshot().TriggeredHeuristic)
}

func TestE2EAutoDesign_LargeFileCount_Trigger(t *testing.T) {
	e := newAutoDesignE2E(t)

	// >= LargeFileCount (10) so the heuristic triggers on file count
	// alone. Keep the per-file diff small so the LargeDiffLines
	// trigger doesn't fire first (which would still be a pass but
	// wouldn't exercise the file-count branch).
	files := make(map[string]string, 12)
	for i := range 12 {
		files[fmt.Sprintf("src/pkg%02d.go", i)] = fmt.Sprintf("package pkg%02d\n", i)
	}
	sha := e.repo.CommitFiles(files, "feat: spread across packages")

	e.enqueueReviewFor(sha, "feat: spread across packages")

	got := e.eventuallyFindAutoDesign(sha)
	assert := assert.New(t)
	assert.Equal("review", got.JobType)
	assert.NotEqual(storage.JobStatusSkipped, got.Status)
	assert.Equal("auto_design", got.Source)
}

func TestE2EAutoDesign_Dedup_SecondDispatchNoOp(t *testing.T) {
	e := newAutoDesignE2E(t)

	sha := e.repo.CommitFile("db/migrations/007_foo.sql",
		"CREATE TABLE foo(id INT);\n",
		"feat: add foo table")

	// First dispatch — produces the auto_design row.
	e.enqueueReviewFor(sha, "feat: add foo table")
	_ = e.eventuallyFindAutoDesign(sha)

	// Second dispatch for the same commit — the HasAutoDesignSlotForCommit
	// early-return short-circuits, and the partial unique index would
	// reject a duplicate anyway. Either way: exactly one auto_design row
	// ends up in the db.
	e.enqueueReviewFor(sha, "feat: add foo table")

	var n int
	require.NoError(t, e.db.QueryRow(`
		SELECT COUNT(*) FROM review_jobs rj
		JOIN commits c ON rj.commit_id = c.id
		WHERE rj.repo_id = ? AND c.sha = ?
		  AND rj.review_type = 'design' AND rj.source = 'auto_design'
	`, e.row.ID, sha).Scan(&n))
	assert.Equal(t, 1, n, "dedup must cap auto_design rows at one per commit")

	// Metrics: the second dispatch short-circuits before the heuristic
	// fires, so TriggeredHeuristic still shows 1, not 2.
	assert.EqualValues(t, 1, AutoDesignMetricsSnapshot().TriggeredHeuristic)
}

func TestE2EAutoDesign_ClassifierFailed_MarksSkipped(t *testing.T) {
	// Classifier config fails (no classify_agent registered as a
	// SchemaAgent with the sentinel name "no-such-agent"). The worker
	// converts the classify row to status=skipped via
	// completeClassifyAsSkip and bumps ClassifierFailed.
	e := newAutoDesignE2E(t)

	// Force an ambiguous commit that reaches the classifier fallback.
	var body strings.Builder
	for range 40 {
		body.WriteString("\tdo()\n")
	}
	sha := e.repo.CommitFile("src/noop.go", "package src\n\nfunc a() {\n"+body.String()+"}\n",
		"feat: tweak")

	// Override classify_agent to an unknown name so agent.Get fails.
	require.NoError(t, os.WriteFile(filepath.Join(e.repo.Path(), ".roborev.toml"),
		[]byte(`agent = "test"
classify_agent = "no-such-agent"

[auto_design_review]
enabled = true
classifier_timeout_seconds = 1
`), 0o644))

	// Don't set test verdict — let the real path fire, which will hit
	// config.ResolveClassifyAgent's validator and fail.
	parent := e.enqueueReviewFor(sha, "feat: tweak")
	e.completeParentReview(parent)
	e.startWorkers()

	got := e.eventuallyAutoDesignMatches(sha, "status=skipped (classifier failed)",
		func(j *storage.ReviewJob) bool { return j.Status == storage.JobStatusSkipped })
	assert := assert.New(t)
	assert.Equal("review", got.JobType)
	assert.NotEmpty(got.SkipReason)

	// Counter: ClassifierFailed bumps. Poll for the goroutine.
	var snap storage.AutoDesignStatus
	for range 80 {
		snap = AutoDesignMetricsSnapshot()
		if snap.ClassifierFailed > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	assert.EqualValues(1, snap.ClassifierFailed)
}

// e2eAutoDesignAgentCols returns the (agent, model) pair persisted on
// the auto_design row for sha. Used to verify callers resolve and pass
// the real design-workflow agent/model through EnqueueAutoDesignJob /
// PromoteClassifyToDesignReview — otherwise the sentinel "auto-design"
// would end up on the row and the worker would fail to run it.
func e2eAutoDesignAgentCols(t *testing.T, db *storage.DB, repoID int64, sha string) (string, string) {
	t.Helper()
	var ag, model string
	require.NoError(t, db.QueryRow(`
		SELECT COALESCE(rj.agent,''), COALESCE(rj.model,'')
		FROM review_jobs rj JOIN commits c ON rj.commit_id = c.id
		WHERE rj.repo_id = ? AND c.sha = ?
		  AND rj.review_type = 'design' AND rj.source = 'auto_design'
		LIMIT 1
	`, repoID, sha).Scan(&ag, &model))
	return ag, model
}

func TestE2EAutoDesign_HeuristicTriggerPersistsAgentModel(t *testing.T) {
	// Configure a design agent + model; heuristic trigger path must
	// enqueue with those values, not with the AutoDesignAgentSentinel.
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)

	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile(".roborev.toml", `agent = "test"
design_agent = "test"
design_model = "mock-design-model"

[auto_design_review]
enabled = true
`, "chore: enable auto design review")

	db, _ := testutil.OpenTestDBWithDir(t)
	row, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	cfg := config.DefaultConfig()
	srv := NewServer(db, cfg, "")

	sha := repo.CommitFile("db/migrations/001.sql",
		"CREATE TABLE t(id INT);\n",
		"feat: t table")

	commit, err := db.GetOrCreateCommit(row.ID, sha, "Author", "feat: t table", time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: row.ID, CommitID: commit.ID, GitRef: sha, Agent: "test",
	})
	require.NoError(t, err)
	job.RepoPath = row.RootPath
	job.CommitSubject = "feat: t table"

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), job))

	ag, model := e2eAutoDesignAgentCols(t, db, row.ID, sha)
	assert := assert.New(t)
	assert.Equal("test", ag, "auto_design row must persist the resolved design agent, not the sentinel")
	assert.Equal("mock-design-model", model, "auto_design row must persist the resolved design model")
}

func TestE2EAutoDesign_HTTP_ExplicitDesignBypasses(t *testing.T) {
	// An explicit `roborev review --type design` must bypass the
	// auto-design path — the caller already asked for design, so the
	// HTTP handler's gate skips maybeDispatchAutoDesign. Only the one
	// explicitly-typed design row should exist (source NULL, not
	// "auto_design").
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)

	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "repo")
	testutil.InitTestGitRepo(t, repoDir)

	// Enable auto-design in the test repo so if the bypass WEREN'T in
	// place this test would fail by observing an auto_design row.
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".roborev.toml"),
		[]byte("[auto_design_review]\nenabled = true\n"), 0o644))

	sha := testutil.GetHeadSHA(t, repoDir)

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath:   repoDir,
		GitRef:     sha,
		Agent:      "test",
		ReviewType: "design",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// The only row for this commit should be the explicit design review
	// the caller asked for — source empty, NOT auto_design. Sweep EVERY
	// state (including skipped and failed) so a mistaken dispatch that
	// inserted a skipped row can't sneak past the sweep.
	dbRepo, err := db.GetOrCreateRepo(repoDir)
	require.NoError(t, err)
	var autoCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM review_jobs WHERE repo_id = ? AND source = 'auto_design'
	`, dbRepo.ID).Scan(&autoCount))
	assert.Equal(t, 0, autoCount,
		"explicit --type design must bypass auto-design path (including skipped rows)")
	assert.EqualValues(t, 0, AutoDesignMetricsSnapshot().TriggeredHeuristic)
}

func TestE2EAutoDesign_HTTP_RangeReviewBypasses(t *testing.T) {
	// Range refs (base..head) must NOT be routed through auto-design:
	// maybeDispatchAutoDesign's diff acquisition doesn't handle range
	// refs, so a range-typed review would always fall into MinDiffLines
	// and silently record a bogus "trivial" skip. The handler gate
	// excludes range. Regression guard so a future refactor that
	// re-adds JobTypeRange to the gate without fixing diff acquisition
	// fails this test.
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)

	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "repo")
	repo := testutil.InitTestGitRepo(t, repoDir)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".roborev.toml"),
		[]byte("[auto_design_review]\nenabled = true\n"), 0o644))
	base := repo.HeadSHA()

	// Add a real second commit so the range is non-degenerate. Using
	// base..head where base == head would pass even if a future change
	// specifically short-circuits empty ranges — we want a genuine
	// base..head spanning at least one commit.
	head := repo.CommitFile("a.txt", "hello\n", "feat: add a.txt")
	require.NotEqual(t, base, head)

	// base..head range ref — default review_type so the handler's
	// auto-design gate would otherwise consider it.
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repoDir,
		GitRef:   base + ".." + head,
		Agent:    "test",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	dbRepo, err := db.GetOrCreateRepo(repoDir)
	require.NoError(t, err)
	var autoCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM review_jobs WHERE repo_id = ? AND source = 'auto_design'
	`, dbRepo.ID).Scan(&autoCount))
	assert.Equal(t, 0, autoCount,
		"range jobs must bypass auto-design (no auto_design row, including skipped)")
	assert.EqualValues(t, 0, AutoDesignMetricsSnapshot().SkippedHeuristic,
		"range jobs must not bump heuristic counters")
}

func TestE2EAutoDesign_HTTP_SecurityReviewBypasses(t *testing.T) {
	// Non-default review_type (security) must also bypass auto-design.
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)

	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "repo")
	testutil.InitTestGitRepo(t, repoDir)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".roborev.toml"),
		[]byte("[auto_design_review]\nenabled = true\n"), 0o644))
	sha := testutil.GetHeadSHA(t, repoDir)

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath:   repoDir,
		GitRef:     sha,
		Agent:      "test",
		ReviewType: "security",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	dbRepo, err := db.GetOrCreateRepo(repoDir)
	require.NoError(t, err)
	var autoCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM review_jobs WHERE repo_id = ? AND source = 'auto_design'
	`, dbRepo.ID).Scan(&autoCount))
	assert.Equal(t, 0, autoCount,
		"non-default review_type must bypass auto-design (including skipped rows)")
}

func TestE2EAutoDesign_Disabled_NoOp(t *testing.T) {
	// Repo exists and has commits but .roborev.toml doesn't enable
	// auto-design. No auto_design row should appear for any commit.
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)

	repo := testutil.NewTestRepoWithCommit(t)
	db, _ := testutil.OpenTestDBWithDir(t)
	row, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	cfg := config.DefaultConfig()
	srv := NewServer(db, cfg, "")

	// Deliberately no .roborev.toml — the feature is off everywhere.
	sha := repo.CommitFile("db/migrations/001.sql", "CREATE TABLE x(id INT);",
		"feat: add x table") // this WOULD trigger if enabled

	commit, err := db.GetOrCreateCommit(row.ID, sha, "Author", "feat: add x table", time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: row.ID, CommitID: commit.ID, GitRef: sha, Agent: "test",
	})
	require.NoError(t, err)
	job.RepoPath = row.RootPath
	job.CommitSubject = "feat: add x table"

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), job))

	// Sweep every status — no auto_design rows must exist for this repo.
	for _, st := range []storage.JobStatus{
		storage.JobStatusQueued, storage.JobStatusRunning,
		storage.JobStatusDone, storage.JobStatusFailed,
		storage.JobStatusSkipped,
	} {
		jobs, err := db.ListJobsByStatus(row.ID, st)
		require.NoError(t, err)
		for _, j := range jobs {
			assert.NotEqual(t, "auto_design", j.Source, "no auto_design rows allowed when feature disabled")
		}
	}
	// Counters don't tick for disabled.
	snap := AutoDesignMetricsSnapshot()
	assert.EqualValues(t, 0, snap.TriggeredHeuristic)
	assert.EqualValues(t, 0, snap.SkippedHeuristic)
}
