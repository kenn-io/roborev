package daemon

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	googlegithub "github.com/google/go-github/v93/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

const branchReviewRepo = "acme/api"

type branchStatus struct {
	SHA, State, Description string
}

// branchReviewHarness polls a real clone of a local origin repository. Only
// GitHub writes, PR listing, and the review agent are fakes.
type branchReviewHarness struct {
	origin *testutil.TestRepo
	clone  string
	db     *storage.DB
	cfg    *config.Config
	poller *CIPoller
	pool   *WorkerPool
	// broadcaster carries the worker's review events, as hooks receive them.
	broadcaster Broadcaster
	mu          sync.Mutex
	statuses    []branchStatus
	findings    bool
	fail        bool
	reviewed    []string
}

func newBranchReviewHarness(t *testing.T) *branchReviewHarness {
	t.Helper()
	testenv.SetDataDir(t)
	h := &branchReviewHarness{origin: testutil.NewGitRepo(t)}
	h.origin.CommitFile("app.txt", "start\n", "initial")

	clone := filepath.Join(t.TempDir(), "clone")
	h.clone = clone
	out, err := exec.CommandContext(t.Context(), "git", "clone", "--quiet", h.origin.Path(), clone).CombinedOutput()
	require.NoError(t, err, "%s", out)

	h.db = testutil.OpenTestDB(t)
	_, err = h.db.GetOrCreateRepo(clone, "git@github.com:"+branchReviewRepo+".git")
	require.NoError(t, err)

	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "branch-reviewer", ReviewFn: func(_ context.Context, _, gitRef, _ string, _ io.Writer) (string, error) {
		h.mu.Lock()
		h.reviewed = append(h.reviewed, gitRef)
		findings, fail := h.findings, h.fail
		h.mu.Unlock()
		if fail {
			return "", errors.New("agent unavailable")
		}
		if findings {
			return string(testutil.ReviewFixtureJSON("## Findings\n- [P1] Missing validation")), nil
		}
		return string(testutil.ReviewFixtureJSON("No issues found.")), nil
	}})

	h.cfg = config.DefaultConfig()
	h.cfg.CI.Enabled = true
	h.cfg.CI.ReviewDefaultBranch = true
	h.cfg.CI.Agents = []string{"branch-reviewer"}
	h.cfg.CI.ReviewTypes = []string{"default"}
	h.cfg.CI.SynthesisAgent = "branch-reviewer"
	h.poller = NewCIPoller(h.db, NewStaticConfig(h.cfg), nil)
	stubCIPollerGitHubSideEffects(h.poller)
	h.poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
	h.poller.agentResolverFn = func(name string) (string, error) { return name, nil }
	h.poller.buildReviewPromptFn = func(_ context.Context, _, gitRef string, _ int64, _ int, _, _, _, _ string, _ prompt.PullRequest, _ *config.RepoConfig, _ string, _ *config.Config) (string, error) {
		return "Review " + gitRef, nil
	}
	h.poller.setBranchStatusFn = func(_, sha, state, description string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.statuses = append(h.statuses, branchStatus{SHA: sha, State: state, Description: description})
		return nil
	}
	h.broadcaster = NewBroadcaster()
	h.pool = NewWorkerPool(h.db, NewStaticConfig(h.cfg), 1, h.broadcaster, nil, nil)
	h.pool.retryBackoff = 0
	return h
}

func (h *branchReviewHarness) poll(t *testing.T) {
	t.Helper()
	require.NoError(t, h.poller.pollRepo(t.Context(), branchReviewRepo, h.cfg))
}

// runJobs executes queued reviews, members before the synthesis they release.
func (h *branchReviewHarness) runJobs(t *testing.T) {
	t.Helper()
	for {
		job, err := h.db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		if job == nil {
			return
		}
		h.pool.processJob(testWorkerID, job)
	}
}

func (h *branchReviewHarness) synthesisJobs(t *testing.T) []storage.ReviewJob {
	t.Helper()
	jobs, err := h.db.ListJobs("", "", 100, 0)
	require.NoError(t, err)
	var synth []storage.ReviewJob
	for _, job := range jobs {
		if job.IsSynthesisJob() {
			synth = append(synth, job)
		}
	}
	return synth
}

func (h *branchReviewHarness) takeStatuses() []branchStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	statuses := h.statuses
	h.statuses = nil
	return statuses
}

func TestCIDefaultBranchReviewsEachLandedCommitOnce(t *testing.T) {
	assert := assert.New(t)
	h := newBranchReviewHarness(t)
	start := h.origin.HeadSHA()

	h.poll(t)
	assert.Empty(h.synthesisJobs(t), "enabling reviews must not review existing history")

	first := h.origin.CommitFile("app.txt", "first\n", "first change")
	second := h.origin.CommitFile("app.txt", "second\n", "second change")
	h.poll(t)
	h.poll(t)

	runs := h.synthesisJobs(t)
	require.Len(t, runs, 2, "each landed commit gets exactly one review")
	refs := []string{runs[0].GitRef, runs[1].GitRef}
	assert.ElementsMatch([]string{start + ".." + first, first + ".." + second}, refs)
	for _, run := range runs {
		assert.Equal(storage.JobSourceCIBranch, run.Source)
		assert.Equal("main", run.Branch)
		assert.Equal("main", run.HookBranch())
	}
	assert.Equal([]branchStatus{
		{SHA: first, State: "pending", Description: "Review in progress"},
		{SHA: second, State: "pending", Description: "Review in progress"},
	}, h.takeStatuses())

	h.runJobs(t)
	h.poll(t) // a lost completion event is recovered by the next poll
	assert.ElementsMatch([]branchStatus{
		{SHA: first, State: "success", Description: "No issues found"},
		{SHA: second, State: "success", Description: "No issues found"},
	}, h.takeStatuses())
	h.poll(t)
	assert.Empty(h.takeStatuses(), "a published result is not sent again")
}

func TestCIDefaultBranchReviewsMergeAgainstFirstParent(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	before := h.origin.HeadSHA()
	h.origin.CheckoutNewBranch("feature")
	h.origin.CommitFile("feature.txt", "one\n", "feature one")
	h.origin.CommitFile("feature.txt", "two\n", "feature two")
	h.origin.CheckoutBranch("main")
	h.origin.Run("merge", "--no-ff", "-m", "merge feature", "feature")
	merge := h.origin.HeadSHA()

	h.poll(t)
	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1, "the merged branch is one change to the default branch")
	assert.Equal(t, before+".."+merge, runs[0].GitRef)
}

func TestCIDefaultBranchReviewStatusReportsFindings(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.findings = true
	h.poll(t)
	landed := h.origin.CommitFile("app.txt", "changed\n", "change")
	h.poll(t)
	h.takeStatuses()
	h.runJobs(t)

	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1)
	h.poller.handleReviewCompleted(Event{Type: "review.completed", JobID: runs[0].ID})
	assert.Equal(t, []branchStatus{{SHA: landed, State: "success", Description: "Issues found"}}, h.takeStatuses())
}

func TestCIDefaultBranchReviewsSkipRewrittenHistory(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.origin.CommitFile("app.txt", "reviewed\n", "reviewed")
	h.poll(t)
	h.origin.Run("reset", "--hard", "HEAD~1")
	rewritten := h.origin.CommitFile("app.txt", "rewritten\n", "rewritten")

	h.poll(t)
	assert.Empty(t, h.synthesisJobs(t), "commits after a rewrite cannot be told apart from reviewed ones")
	cursor, err := h.db.GetCIBranchCursor(t.Context(), branchReviewRepo)
	require.NoError(t, err)
	require.NotNil(t, cursor)
	assert.Equal(t, rewritten, cursor.HeadSHA)

	next := h.origin.CommitFile("app.txt", "next\n", "next")
	h.poll(t)
	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1)
	assert.Equal(t, rewritten+".."+next, runs[0].GitRef)
}

func TestCIDefaultBranchReviewsRespectRepositoryOptOut(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	h.origin.CommitFile(".roborev.toml", "[ci]\nreview_default_branch = false\n", "opt out")
	h.origin.CommitFile("app.txt", "skipped\n", "skipped")
	h.poll(t)
	assert.Empty(t, h.synthesisJobs(t))

	skipped := h.origin.HeadSHA()
	optIn := h.origin.CommitFile(".roborev.toml", "[ci]\nreview_default_branch = true\n", "opt in")
	h.poll(t)
	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1, "commits landed while opted out stay unreviewed")
	assert.Equal(t, skipped+".."+optIn, runs[0].GitRef)
}

func TestCIDefaultBranchReviewsRecordInvalidConfiguration(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	landed := h.origin.CommitFile(".roborev.toml", "[ci]\ncomment_max_findings = -1\n", "invalid config")
	h.poll(t)
	assert.Empty(t, h.synthesisJobs(t))
	assert.Equal(t, []branchStatus{{
		SHA: landed, State: "error",
		Description: "Review could not be queued; run roborev config validate",
	}}, h.takeStatuses())
	cursor, err := h.db.GetCIBranchCursor(t.Context(), branchReviewRepo)
	require.NoError(t, err)
	assert.Equal(t, landed, cursor.HeadSHA, "a commit that cannot be reviewed does not block later commits")
}

func TestCIDefaultBranchReviewFailureReportsError(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.fail = true
	h.poll(t)
	landed := h.origin.CommitFile("app.txt", "changed\n", "change")
	h.poll(t)
	h.takeStatuses()
	h.runJobs(t)
	h.poll(t)

	statuses := h.takeStatuses()
	require.Len(t, statuses, 1)
	assert.Equal(t, landed, statuses[0].SHA)
	assert.Equal(t, "error", statuses[0].State, "a review that did not run is not reported as clean")
}

func TestCIDefaultBranchReviewsWaitForQuietHoursToEnd(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	start := h.origin.HeadSHA()
	landed := h.origin.CommitFile("app.txt", "changed\n", "change")
	now := time.Now()
	h.poller.nowFn = func() time.Time { return now }
	h.poller.quietHours = quietWindow(t, now, "1h", true)
	h.poll(t)
	assert.Empty(t, h.synthesisJobs(t))

	h.poller.quietHours = nil
	h.poll(t)
	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1, "commits that landed during quiet hours are reviewed afterward")
	assert.Equal(t, start+".."+landed, runs[0].GitRef)
}

func TestCIDefaultBranchReviewEventsIdentifyLandedCommitResult(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.findings = true
	h.poll(t)
	h.origin.CommitFile("app.txt", "changed\n", "change")
	h.poll(t)
	_, events := h.broadcaster.Subscribe("")
	h.runJobs(t)

	roles := map[string]string{}
	for len(events) > 0 {
		event := <-events
		if event.Type != "review.completed" {
			continue
		}
		assert.Equal(t, storage.JobSourceCIBranch, event.Source,
			"hooks can select landed-commit reviews by source")
		roles[event.PanelRole] = event.Verdict
	}
	assert.Equal(t, map[string]string{
		storage.PanelRoleMember:    "F",
		storage.PanelRoleSynthesis: "F",
	}, roles)
}

func TestCIDefaultBranchReviewsRestartAfterBeingTurnedOff(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.cfg.CI.Repos = []string{branchReviewRepo}
	h.poller.poll(t.Context())
	h.cfg.CI.ReviewDefaultBranch = false
	h.origin.CommitFile("app.txt", "while off\n", "landed while off")
	// GitHub is unreachable for as long as the reviews are off.
	h.poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) {
		return nil, errors.New("connection refused")
	}
	h.poller.poll(t.Context())
	h.poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
	h.cfg.CI.ReviewDefaultBranch = true
	h.poller.poll(t.Context())
	assert.Empty(t, h.synthesisJobs(t), "turning reviews back on does not review what landed while off")

	start := h.origin.HeadSHA()
	landed := h.origin.CommitFile("app.txt", "after\n", "landed after")
	h.poller.poll(t.Context())
	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1)
	assert.Equal(t, start+".."+landed, runs[0].GitRef)
}

func TestCIDefaultBranchReviewsResetWhenLastReviewIsOnlyMerged(t *testing.T) {
	h := newBranchReviewHarness(t)
	base := h.origin.HeadSHA()
	h.origin.CommitFile("app.txt", "reviewed\n", "reviewed on main")
	h.poll(t)
	// Rebuild main so the reviewed commit survives only as a merged branch:
	// its first-parent history never contained that commit.
	h.origin.Run("checkout", "-q", "-b", "other", base)
	h.origin.CommitFile("other.txt", "other\n", "other line")
	h.origin.Run("merge", "-q", "--no-ff", "-m", "merge old main", "main")
	merged := h.origin.HeadSHA()
	h.origin.Run("checkout", "-q", "main")
	h.origin.Run("reset", "-q", "--hard", merged)

	h.poll(t)
	assert.Empty(t, h.synthesisJobs(t), "a merge's second parent is not a review base")
	cursor, err := h.db.GetCIBranchCursor(t.Context(), branchReviewRepo)
	require.NoError(t, err)
	assert.Equal(t, merged, cursor.HeadSHA)
}

func TestCIDefaultBranchCursorSurvivesCanceledHistoryCheck(t *testing.T) {
	h := newBranchReviewHarness(t)
	reviewed := h.origin.HeadSHA()
	h.poll(t)
	h.origin.CommitFile("app.txt", "landed\n", "landed")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := landedCommits(canceled, h.origin.Path(), reviewed, h.origin.HeadSHA())
	require.Error(t, err, "a canceled check is not evidence that history was rewritten")

	// Cancel after the poll has fetched and resolved the new head, so the
	// poll stops before it decides what the landed commit needs.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.poller.loadRepoConfigFn = func(repoPath string) (ciRepoConfigSource, error) {
		cancel()
		return loadCIRepoConfig(repoPath)
	}
	require.Error(t, h.poller.pollRepo(ctx, branchReviewRepo, h.cfg))
	cursor, err := h.db.GetCIBranchCursor(t.Context(), branchReviewRepo)
	require.NoError(t, err)
	assert.Equal(t, reviewed, cursor.HeadSHA, "the landed commit is still waiting for review")
	assert.Empty(t, h.synthesisJobs(t))
}

func TestCIDefaultBranchReviewsHonorUncommittedOptOut(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.clone, ".roborev.toml"),
		[]byte("[ci]\nreview_default_branch = false\n"), 0o644))
	landed := h.origin.CommitFile("app.txt", "changed\n", "change")
	h.poll(t)

	assert.Empty(t, h.synthesisJobs(t), "the poller checkout's configuration opts the repository out")
	cursor, err := h.db.GetCIBranchCursor(t.Context(), branchReviewRepo)
	require.NoError(t, err)
	assert.Equal(t, landed, cursor.HeadSHA, "the skipped commit is accounted for")
}

func TestCIDefaultBranchReviewsSkipCommitsLandedWhileOptedOutBetweenPolls(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	h.origin.CommitFile(".roborev.toml", "[ci]\nreview_default_branch = false\n", "opt out")
	skipped := h.origin.CommitFile("app.txt", "skipped\n", "skipped")
	optIn := h.origin.CommitFile(".roborev.toml", "[ci]\nreview_default_branch = true\n", "opt in")
	h.poll(t)

	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1, "only the commit that turned reviews back on is reviewed")
	assert.Equal(t, skipped+".."+optIn, runs[0].GitRef)
}

func TestCIDefaultBranchPendingStatusPrecedesReviewJobs(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	var jobsAtPending []int
	h.poller.setBranchStatusFn = func(_, _, state, _ string) error {
		if state == "pending" {
			jobs, err := h.db.ListJobs("", "", 100, 0)
			require.NoError(t, err)
			jobsAtPending = append(jobsAtPending, len(jobs))
		}
		return nil
	}
	h.origin.CommitFile("app.txt", "changed\n", "change")
	h.poll(t)
	assert.Equal(t, []int{0}, jobsAtPending,
		"pending is published before any job can finish and publish its result")
}

func TestCIDefaultBranchReviewsApplyBatchTimeout(t *testing.T) {
	h := newBranchReviewHarness(t)
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "branch-reviewer-2"})
	h.cfg.CI.Agents = []string{"branch-reviewer", "branch-reviewer-2"}
	h.cfg.CI.BatchTimeout = "1m"
	h.poll(t)
	h.origin.CommitFile("app.txt", "changed\n", "change")
	h.poll(t)

	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1)
	members, err := h.db.GetPanelMemberReviews(*runs[0].PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 2)
	done, hung := members[0].JobID, members[1].JobID
	_, err = h.db.ExecContext(t.Context(), `UPDATE review_jobs SET status = 'running',
		started_at = datetime('now', '-2 hours') WHERE id IN (?, ?)`, done, hung)
	require.NoError(t, err)
	clean := testutil.ReviewFixtureJSON("No issues found.")
	require.NoError(t, h.db.CompleteJobResult(done, "branch-reviewer", storage.ReviewCompletion{
		Output: string(clean), Verdict: storage.VerdictPass, StructuredOutput: jsontext.Value(clean),
	}))

	h.poll(t)
	job, err := h.db.GetJobByID(hung)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusCanceled, job.Status, "a reviewer past ci.batch_timeout no longer holds the result")
}

func TestCIDefaultBranchReviewFilesOneKataIssueWhenEveryReviewerFails(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.fail = true
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "branch-reviewer-2", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return "", errors.New("agent unavailable")
	}})
	h.cfg.CI.Agents = []string{"branch-reviewer", "branch-reviewer-2"}
	h.cfg.Hooks = []config.HookConfig{{
		Event: "review.*", Type: "kata", Project: "p", Sources: []string{storage.JobSourceCIBranch},
	}}
	hooks := NewHookRunner(NewStaticConfig(h.cfg), h.broadcaster, nil)
	t.Cleanup(hooks.Stop)
	fake := &katatest.FakeClient{CreateResult: kata.CreateResult{ShortID: "new1"}}
	hooks.newKataClient = func(string) kata.Client { return fake }

	h.poll(t)
	h.origin.CommitFile("app.txt", "changed\n", "change")
	h.poll(t)
	h.runJobs(t)
	hooks.WaitUntilIdle()

	require.Len(t, fake.CreateReqs, 1, "one failed panel opens one issue, not one per reviewer or none")
	assert.Contains(t, fake.CreateReqs[0].Labels, "review-failed")
}

func TestCIDefaultBranchReviewsRestartWhenLastReviewedCommitIsMissing(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	// A clone that was replaced no longer has the commit the poller reviewed
	// last. No repository contains this one.
	require.NoError(t, h.db.SetCIBranchCursor(t.Context(), branchReviewRepo, "main",
		"0123456789012345678901234567890123456789"))
	head := h.origin.CommitFile("app.txt", "changed\n", "change")

	h.poll(t)
	assert.Empty(t, h.synthesisJobs(t), "commits after an unknown position cannot be told apart from reviewed ones")
	cursor, err := h.db.GetCIBranchCursor(t.Context(), branchReviewRepo)
	require.NoError(t, err)
	require.NotNil(t, cursor)
	assert.Equal(t, head, cursor.HeadSHA)

	next := h.origin.CommitFile("app.txt", "next\n", "next")
	h.poll(t)
	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1)
	assert.Equal(t, head+".."+next, runs[0].GitRef)
}

func TestCIDefaultBranchReviewsFollowRenamedDefaultBranch(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	h.origin.Run("branch", "-m", "main", "trunk")
	renamed := h.origin.CommitFile("app.txt", "renamed\n", "landed after the rename")

	h.poll(t)
	cursor, err := h.db.GetCIBranchCursor(t.Context(), branchReviewRepo)
	require.NoError(t, err)
	require.NotNil(t, cursor)
	assert.Equal(t, storage.CIBranchCursor{GithubRepo: branchReviewRepo, Branch: "trunk", HeadSHA: renamed}, *cursor)

	next := h.origin.CommitFile("app.txt", "next\n", "next")
	h.poll(t)
	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1)
	assert.Equal(t, renamed+".."+next, runs[0].GitRef)
	assert.Equal(t, "trunk", runs[0].HookBranch())
}

func TestCIDefaultBranchReviewsFollowNewDefaultBranchWhenOldOneRemains(t *testing.T) {
	assert := assert.New(t)
	h := newBranchReviewHarness(t)
	h.poll(t)
	// The origin's checked-out branch is its default. main stays in place.
	h.origin.CheckoutNewBranch("develop")
	switched := h.origin.CommitFile("app.txt", "develop\n", "first commit on the new default")

	h.poll(t)
	assert.Empty(h.synthesisJobs(t), "a new default branch starts from its head")
	cursor, err := h.db.GetCIBranchCursor(t.Context(), branchReviewRepo)
	require.NoError(t, err)
	require.NotNil(t, cursor)
	assert.Equal(storage.CIBranchCursor{GithubRepo: branchReviewRepo, Branch: "develop", HeadSHA: switched}, *cursor)

	h.origin.CheckoutBranch("main")
	h.origin.CommitFile("old.txt", "old\n", "commit on the former default")
	h.origin.CheckoutBranch("develop")
	landed := h.origin.CommitFile("app.txt", "next\n", "next commit on the new default")
	h.poll(t)
	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1, "only the new default branch is reviewed")
	assert.Equal(switched+".."+landed, runs[0].GitRef)
	assert.Equal("develop", runs[0].HookBranch())
}

func TestCIDefaultBranchReviewsRestartAfterRepoLeavesThePoller(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.cfg.CI.Repos = []string{branchReviewRepo}
	h.poller.poll(t.Context())
	h.cfg.CI.Repos = nil
	h.origin.CommitFile("app.txt", "while away\n", "landed while not polled")
	h.poller.poll(t.Context())
	h.cfg.CI.Repos = []string{branchReviewRepo}
	h.poller.poll(t.Context())
	assert.Empty(t, h.synthesisJobs(t), "a returning repo does not review what landed while it was not polled")

	start := h.origin.HeadSHA()
	landed := h.origin.CommitFile("app.txt", "after\n", "landed after")
	h.poller.poll(t.Context())
	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1)
	assert.Equal(t, start+".."+landed, runs[0].GitRef)
}

func TestCIDefaultBranchReviewsAssignExperimentsPerCommit(t *testing.T) {
	h := newBranchReviewHarness(t)
	enabled, ratio := true, 1.0
	h.cfg.Experiments = map[string]config.ExperimentDefinition{
		"landed-model-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowCI},
			Config:    map[string]any{"review_model": "experiment-model"},
		},
	}
	h.poll(t)
	h.origin.CommitFile("app.txt", "first\n", "first change")
	h.origin.CommitFile("app.txt", "second\n", "second change")
	h.poll(t)

	runs := h.synthesisJobs(t)
	require.Len(t, runs, 2)
	require.Len(t, runs[0].Experiments, 1)
	require.Len(t, runs[1].Experiments, 1)
	assert.NotEqual(t, runs[0].Experiments[0].SubjectHash, runs[1].Experiments[0].SubjectHash,
		"landed commits share a branch name, so each commit is assigned on its own")
}

func TestCIDefaultBranchStatusDelivery(t *testing.T) {
	githubError := func(status int, message string) error {
		return &googlegithub.ErrorResponse{Response: &http.Response{StatusCode: status}, Message: message}
	}
	tests := []struct {
		name    string
		err     error
		retried bool
	}{
		{name: "a failed request is retried", err: errors.New("connection reset"), retried: true},
		{name: "a server error is retried", err: githubError(http.StatusBadGateway, "Bad Gateway"), retried: true},
		{name: "a token that cannot write statuses", err: githubError(http.StatusForbidden, "Resource not accessible by integration")},
		{name: "a commit GitHub no longer has", err: githubError(http.StatusUnprocessableEntity, "No commit found for SHA")},
		{name: "no GitHub credentials", err: errBranchStatusNoToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			h := newBranchReviewHarness(t)
			h.poll(t)
			failing := true
			var failed, published int
			h.poller.setBranchStatusFn = func(_, _, state, _ string) error {
				switch {
				case state == "pending":
					return nil
				case failing:
					failed++
					return tt.err
				}
				published++
				return nil
			}
			h.origin.CommitFile("app.txt", "changed\n", "change")
			h.poll(t)
			h.runJobs(t)
			h.poll(t)
			h.poll(t)
			failing = false
			h.poll(t)
			h.poll(t)

			if tt.retried {
				assert.Equal(2, failed, "delivery is tried once on each of the two failing polls")
				assert.Equal(1, published, "the result is published once GitHub accepts it")
				return
			}
			assert.Equal(1, failed, "GitHub will not accept this status, so it is sent once")
			assert.Zero(published)
		})
	}
}

func TestCIDefaultBranchStatusFailureIsTriedOncePerResultEachPoll(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	attempts := map[string]int{}
	h.poller.setBranchStatusFn = func(_, sha, state, _ string) error {
		if state == "pending" {
			return nil
		}
		attempts[sha]++
		return errors.New("connection reset")
	}
	first := h.origin.CommitFile("app.txt", "first\n", "first change")
	second := h.origin.CommitFile("app.txt", "second\n", "second change")
	third := h.origin.CommitFile("app.txt", "third\n", "third change")
	h.poll(t)
	h.runJobs(t)

	h.poll(t)
	assert.Equal(t, map[string]int{first: 1, second: 1, third: 1}, attempts,
		"recovering several results must not send each failing status again for every result")
}

func TestCIDefaultBranchReviewsFetchOriginWhenCheckoutTracksAnotherRemote(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	// The registered checkout's current branch follows a contributor's fork,
	// so a fetch that names no remote would update the fork, not origin.
	fork := testutil.NewGitRepo(t)
	fork.CommitFile("fork.txt", "fork\n", "fork commit")
	for _, args := range [][]string{
		{"remote", "add", "fork", fork.Path()},
		{"config", "branch.main.remote", "fork"},
		{"config", "branch.main.merge", "refs/heads/main"},
	} {
		out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", h.clone}, args...)...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	start := h.origin.HeadSHA()
	landed := h.origin.CommitFile("app.txt", "changed\n", "change")

	h.poll(t)
	runs := h.synthesisJobs(t)
	require.Len(t, runs, 1, "the commit that landed on origin is reviewed")
	assert.Equal(t, start+".."+landed, runs[0].GitRef)
}

func TestCIDefaultBranchCompletionPublishesOnlyItsOwnStatus(t *testing.T) {
	h := newBranchReviewHarness(t)
	h.poll(t)
	attempts := map[string]int{}
	h.poller.setBranchStatusFn = func(_, sha, state, _ string) error {
		if state == "pending" {
			return nil
		}
		attempts[sha]++
		return errors.New("connection reset")
	}
	h.origin.CommitFile("app.txt", "first\n", "first change")
	h.poll(t)
	h.runJobs(t)
	h.poll(t) // the first result is recorded, and its status fails to send

	second := h.origin.CommitFile("app.txt", "second\n", "second change")
	h.poll(t)
	h.runJobs(t)
	clear(attempts)
	for _, run := range h.synthesisJobs(t) {
		if strings.HasSuffix(run.GitRef, second) {
			h.poller.handleReviewCompleted(Event{Type: "review.completed", JobID: run.ID})
		}
	}
	assert.Equal(t, map[string]int{second: 1}, attempts,
		"a completion does not retry the statuses that failed earlier")
}
