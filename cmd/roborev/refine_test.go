package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type mockDaemonClient struct {
	reviews   map[string]*storage.Review
	jobs      map[int64]*storage.ReviewJob
	responses map[int64][]storage.Response

	closedJobIDs    []int64
	addedComments   []addedComment
	enqueuedReviews []enqueuedReview

	nextReviewID int64

	markClosedErr     error
	getReviewBySHAErr error
}

type addedComment struct {
	JobID     int64
	Commenter string
	Comment   string
}

type enqueuedReview struct {
	RepoPath  string
	GitRef    string
	AgentName string
}

func newMockDaemonClient() *mockDaemonClient {
	return &mockDaemonClient{
		reviews:   make(map[string]*storage.Review),
		jobs:      make(map[int64]*storage.ReviewJob),
		responses: make(map[int64][]storage.Response),
	}
}

func (m *mockDaemonClient) GetReviewBySHA(sha string) (*storage.Review, error) {
	if m.getReviewBySHAErr != nil {
		return nil, m.getReviewBySHAErr
	}
	review, ok := m.reviews[sha]
	if !ok {
		return nil, nil
	}
	return review, nil
}

func (m *mockDaemonClient) GetReviewByJobID(jobID int64) (*storage.Review, error) {
	job, ok := m.jobs[jobID]
	if !ok {
		return nil, nil
	}
	return m.reviews[job.GitRef], nil
}

func (m *mockDaemonClient) MarkReviewClosed(jobID int64) error {
	if m.markClosedErr != nil {
		return m.markClosedErr
	}
	m.closedJobIDs = append(m.closedJobIDs, jobID)
	return nil
}

func (m *mockDaemonClient) AddComment(jobID int64, commenter, comment string) error {
	m.addedComments = append(m.addedComments, addedComment{jobID, commenter, comment})
	return nil
}

func (m *mockDaemonClient) EnqueueReview(repoPath, gitRef, agentName string) (int64, error) {
	m.enqueuedReviews = append(m.enqueuedReviews, enqueuedReview{repoPath, gitRef, agentName})
	return int64(len(m.enqueuedReviews)), nil
}

func (m *mockDaemonClient) WaitForReview(jobID int64) (*storage.Review, error) {
	job, ok := m.jobs[jobID]
	if !ok {
		return nil, nil
	}
	return m.reviews[job.GitRef], nil
}

func (m *mockDaemonClient) FindJobForCommit(ctx context.Context, repoPath, sha string) (*storage.ReviewJob, error) {
	for _, job := range m.jobs {
		if job.GitRef == sha {
			return job, nil
		}
	}
	return nil, nil
}

func (m *mockDaemonClient) FindPendingJobForRef(ctx context.Context, repoPath, gitRef string) (*storage.ReviewJob, error) {
	for _, job := range m.jobs {
		if job.GitRef == gitRef {
			if job.Status == storage.JobStatusQueued || job.Status == storage.JobStatusRunning {
				return job, nil
			}
		}
	}
	return nil, nil
}

func (m *mockDaemonClient) GetCommentsForJob(jobID int64) ([]storage.Response, error) {
	return m.responses[jobID], nil
}

func (m *mockDaemonClient) Remap(req daemon.RemapRequest) (*daemon.RemapResult, error) {
	return &daemon.RemapResult{}, nil
}

func (m *mockDaemonClient) WithReview(sha string, jobID int64, output string, closed bool) *mockDaemonClient {
	m.nextReviewID++
	m.reviews[sha] = &storage.Review{
		ID:     m.nextReviewID,
		JobID:  jobID,
		Output: output,
		Closed: closed,
	}
	return m
}

func (m *mockDaemonClient) WithJob(id int64, gitRef string, status storage.JobStatus) *mockDaemonClient {
	m.jobs[id] = &storage.ReviewJob{
		ID:     id,
		GitRef: gitRef,
		Status: status,
	}
	return m
}

var _ daemon.Client = (*mockDaemonClient)(nil)

func TestSelectRefineAgentUnavailableWithoutBackupFails(t *testing.T) {
	t.Setenv("PATH", "")

	_, err := selectRefineAgent("", nil, "codex", agent.ReasoningFast, "")
	require.Error(t, err, "expected error when no agents are available")
	if !strings.Contains(err.Error(), "no configured agent available") {
		require.NoError(t, err)
	}
}

func TestResolveAllowUnsafeAgents(t *testing.T) {
	boolTrue := true
	boolFalse := false

	tests := []struct {
		name        string
		flag        bool
		flagChanged bool
		cfg         *config.Config
		expected    bool
	}{
		{
			name:        "config enabled, flag not changed - uses config",
			flag:        false,
			flagChanged: false,
			cfg:         &config.Config{AllowUnsafeAgents: &boolTrue},
			expected:    true,
		},
		{
			name:        "config disabled, flag not changed - honors config",
			flag:        false,
			flagChanged: false,
			cfg:         &config.Config{AllowUnsafeAgents: &boolFalse},
			expected:    false,
		},
		{
			name:        "flag explicitly enabled - uses flag over config",
			flag:        true,
			flagChanged: true,
			cfg:         &config.Config{AllowUnsafeAgents: &boolFalse},
			expected:    true,
		},
		{
			name:        "flag explicitly disabled - uses flag over config",
			flag:        false,
			flagChanged: true,
			cfg:         &config.Config{AllowUnsafeAgents: &boolTrue},
			expected:    false,
		},
		{
			name:        "nil config, flag not changed - defaults to true",
			flag:        false,
			flagChanged: false,
			cfg:         nil,
			expected:    true,
		},
		{
			name:        "nil config, flag explicitly enabled - uses flag",
			flag:        true,
			flagChanged: true,
			cfg:         nil,
			expected:    true,
		},
		{
			name:        "config not set (nil pointer), flag not changed - defaults to true",
			flag:        false,
			flagChanged: false,
			cfg:         &config.Config{AllowUnsafeAgents: nil},
			expected:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := resolveAllowUnsafeAgents(tc.flag, tc.flagChanged, tc.cfg)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestSelectRefineAgentCodexUsesRequestedReasoning(t *testing.T) {
	t.Cleanup(testutil.MockExecutable(t, "codex", 0))

	selected, err := selectRefineAgent("", nil, "codex", agent.ReasoningFast, "")
	require.NoError(t, err, "selectRefineAgent failed: %v")

	codexAgent, ok := selected.(*agent.CodexAgent)
	assert.True(t, ok)
	assert.Equal(t, agent.ReasoningFast, codexAgent.Reasoning)
}

func TestSelectRefineAgentCodexACPConfigAliasUsesACPResolution(t *testing.T) {
	t.Cleanup(testutil.MockExecutable(t, "codex", 0))
	t.Cleanup(testutil.MockExecutable(t, "acp-agent", 0))

	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "codex",
			Command: "acp-agent",
		},
	}

	selected, err := selectRefineAgent("", cfg, "codex", agent.ReasoningFast, "")
	require.NoError(t, err, "selectRefineAgent failed: %v")

	acpAgent, ok := selected.(*agent.ACPAgent)
	assert.True(t, ok)
	assert.Equal(t, "acp-agent", acpAgent.CommandName())
}

func TestSelectRefineAgentBackupUsesRequestedReasoning(t *testing.T) {
	t.Cleanup(testutil.MockExecutableIsolated(t, "codex", 0))

	selected, err := selectRefineAgent("", nil, "gemini", agent.ReasoningThorough, "codex")
	require.NoError(t, err, "selectRefineAgent failed: %v")

	codexAgent, ok := selected.(*agent.CodexAgent)
	assert.True(t, ok)
	assert.Equal(t, agent.ReasoningThorough, codexAgent.Reasoning)
}

func TestFindFailedReviewForBranch(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(*mockDaemonClient)
		commits       []string
		skip          map[int64]bool
		wantJobID     int64
		wantErrs      []string
		wantClosedIDs []int64
	}{
		{
			name: "oldest first",
			setup: func(c *mockDaemonClient) {
				c.WithReview("oldest123", 100, "No issues found.", false).
					WithReview("middle456", 200, "Found a bug in the code.", false).
					WithReview("newest789", 300, "Security vulnerability detected.", false)
			},
			commits:       []string{"oldest123", "middle456", "newest789"},
			wantJobID:     200,
			wantClosedIDs: []int64{100},
		},
		{
			name: "skips closed",
			setup: func(c *mockDaemonClient) {
				c.WithReview("commit1", 100, "Bug found.", false).
					WithReview("commit2", 200, "Another bug.", true).
					WithReview("commit3", 300, "More issues.", false)
			},
			commits:   []string{"commit1", "commit2", "commit3"},
			wantJobID: 100,
		},
		{
			name: "skips given up reviews",
			setup: func(c *mockDaemonClient) {
				c.WithReview("commit1", 100, "Bug found.", false).
					WithReview("commit2", 200, "Another bug.", false).
					WithReview("commit3", 300, "No issues found.", false)
			},
			commits:   []string{"commit1", "commit2", "commit3"},
			skip:      map[int64]bool{1: true},
			wantJobID: 200,
		},
		{
			name: "all skipped returns nil",
			setup: func(c *mockDaemonClient) {
				c.WithReview("commit1", 100, "Bug found.", false).
					WithReview("commit2", 200, "Another.", false)
			},
			commits:   []string{"commit1", "commit2"},
			skip:      map[int64]bool{1: true, 2: true},
			wantJobID: 0,
		},
		{
			name: "all pass",
			setup: func(c *mockDaemonClient) {
				c.WithReview("commit1", 100, "No issues found.", false).
					WithReview("commit2", 200, "No findings.", false)
			},
			commits:       []string{"commit1", "commit2"},
			wantJobID:     0,
			wantClosedIDs: []int64{100, 200},
		},
		{
			name:      "no reviews",
			setup:     func(c *mockDaemonClient) {},
			commits:   []string{"unreviewed1", "unreviewed2"},
			wantJobID: 0,
		},
		{
			name: "marks passing as closed",
			setup: func(c *mockDaemonClient) {
				c.WithReview("commit1", 100, "No issues found.", false).
					WithReview("commit2", 200, "No findings.", false)
			},
			commits:       []string{"commit1", "commit2"},
			wantJobID:     0,
			wantClosedIDs: []int64{100, 200},
		},
		{
			name: "marks passing before failure",
			setup: func(c *mockDaemonClient) {
				c.WithReview("commit1", 100, "No issues found.", false).
					WithReview("commit2", 200, "Bug found.", false)
			},
			commits:       []string{"commit1", "commit2"},
			wantJobID:     200,
			wantClosedIDs: []int64{100},
		},
		{
			name: "does not mark already closed",
			setup: func(c *mockDaemonClient) {
				c.WithReview("commit1", 100, "No issues found.", true).
					WithReview("commit2", 200, "Bug found.", false)
			},
			commits:   []string{"commit1", "commit2"},
			wantJobID: 200,
		},
		{
			name: "mixed scenario",
			setup: func(c *mockDaemonClient) {
				c.WithReview("commit1", 100, "No issues found.", false).
					WithReview("commit2", 200, "No issues.", true).
					WithReview("commit3", 300, "Bug found.", true).
					WithReview("commit4", 400, "No findings detected.", false).
					WithReview("commit5", 500, "Critical error.", false)
			},
			commits:       []string{"commit1", "commit2", "commit3", "commit4", "commit5"},
			wantJobID:     500,
			wantClosedIDs: []int64{100, 400},
		},
		{
			name: "stops at first failure",
			setup: func(c *mockDaemonClient) {
				c.WithReview("commit1", 100, "Bug found.", false).
					WithReview("commit2", 200, "No issues found.", false).
					WithReview("commit3", 300, "Another bug.", false)
			},
			commits:   []string{"commit1", "commit2", "commit3"},
			wantJobID: 100,
		},
		{
			name: "mark closed error",
			setup: func(c *mockDaemonClient) {
				c.WithReview("commit1", 100, "No issues found.", false)
				c.markClosedErr = fmt.Errorf("daemon connection failed")
			},
			commits:  []string{"commit1"},
			wantErrs: []string{"closing review (job 100)"},
		},
		{
			name: "get review by sha error",
			setup: func(c *mockDaemonClient) {
				c.getReviewBySHAErr = fmt.Errorf("daemon connection failed")
			},
			commits:  []string{"commit1", "commit2"},
			wantErrs: []string{"fetching review", "commit1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newMockDaemonClient()
			tt.setup(client)

			found, err := findFailedReviewForBranch(client, tt.commits, tt.skip)

			if len(tt.wantErrs) > 0 {
				require.Error(t, err)
				for _, wantErr := range tt.wantErrs {
					require.Contains(t, err.Error(), wantErr, "expected error containing %q, got: %v", wantErr, err)
				}
				require.Nil(t, found)
				return
			}

			require.NoError(t, err, "findFailedReviewForBranch failed: %v")

			if tt.wantJobID == 0 {
				assert.Nil(t, found)
			} else {
				assert.NotNil(t, found)
				assert.Equal(t, tt.wantJobID, found.JobID)
			}

			if len(tt.wantClosedIDs) > 0 {
				assert.Len(t, tt.wantClosedIDs, len(client.closedJobIDs))
				closed := make(map[int64]bool)
				for _, id := range client.closedJobIDs {
					closed[id] = true
				}
				for _, id := range tt.wantClosedIDs {
					assert.True(t, closed[id])
				}
			} else {
				assert.Empty(t, client.closedJobIDs)
			}
		})
	}
}

func TestFindPendingJobForBranch(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(*mockDaemonClient)
		commits   []string
		wantJobID int64
	}{
		{
			name: "finds running job",
			setup: func(c *mockDaemonClient) {
				c.WithJob(100, "commit1", storage.JobStatusDone).
					WithJob(200, "commit2", storage.JobStatusRunning)
			},
			commits:   []string{"commit1", "commit2"},
			wantJobID: 200,
		},
		{
			name: "finds queued job",
			setup: func(c *mockDaemonClient) {
				c.WithJob(100, "commit1", storage.JobStatusQueued)
			},
			commits:   []string{"commit1"},
			wantJobID: 100,
		},
		{
			name: "no pending jobs",
			setup: func(c *mockDaemonClient) {
				c.WithJob(100, "commit1", storage.JobStatusDone).
					WithJob(200, "commit2", storage.JobStatusDone)
			},
			commits:   []string{"commit1", "commit2"},
			wantJobID: 0,
		},
		{
			name:      "no jobs for commits",
			setup:     func(c *mockDaemonClient) {},
			commits:   []string{"unreviewed1", "unreviewed2"},
			wantJobID: 0,
		},
		{
			name: "oldest first",
			setup: func(c *mockDaemonClient) {
				c.WithJob(100, "commit1", storage.JobStatusRunning).
					WithJob(200, "commit2", storage.JobStatusRunning)
			},
			commits:   []string{"commit1", "commit2"},
			wantJobID: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newMockDaemonClient()
			tt.setup(client)

			pending, err := findPendingJobForBranch(t.Context(), client, "/repo", tt.commits)
			require.NoError(t, err, "findPendingJobForBranch failed: %v")

			if tt.wantJobID == 0 {
				assert.Nil(t, pending)
			} else {
				assert.NotNil(t, pending)
				assert.Equal(t, tt.wantJobID, pending.ID)
			}
		})
	}
}

func chdirForTest(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	require.NoError(t, err)
	if err := os.Chdir(dir); err != nil {
		require.NoError(t, err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
}

func TestValidateRefineContext_RefusesMainBranchWithoutSince(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)

	chdirForTest(t, repo.Root)

	_, _, _, _, err := validateRefineContext(t.Context(), "", "", "")
	require.Error(t, err, "expected error when validating on main without --since")
	assert.Contains(t, err.Error(), "refusing to refine on main")
	assert.Contains(t, err.Error(), "--since")
}

func TestValidateRefineContext_AllowsMainBranchWithSince(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)
	baseSHA := repo.RevParse("HEAD")

	repo.CommitFile("second.txt", "second", "second commit")

	chdirForTest(t, repo.Root)

	repoPath, currentBranch, _, mergeBase, err := validateRefineContext(t.Context(), "", baseSHA, "")
	require.NoError(t, err, "validation should pass with --since on main, got: %v")

	assert.NotEmpty(t, repoPath)
	assert.Equal(t, "main", currentBranch)
	assert.Equal(t, mergeBase, baseSHA)
}

func TestValidateRefineContext_SinceIgnoresUpstreamMissing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// --since provides the merge base explicitly, so an unfetched or
	// missing @{upstream} must not block a valid --since invocation.
	repo := testutil.InitTestRepo(t)
	baseSHA := repo.RevParse("HEAD")
	repo.CommitFile("second.txt", "second", "second commit")

	// Configure tracking against an upstream that never resolves locally.
	repo.SetBranchConfig("main", "remote", "upstream")
	repo.SetBranchConfig("main", "merge", "refs/heads/main")

	chdirForTest(t, repo.Root)

	repoPath, currentBranch, _, mergeBase, err := validateRefineContext(t.Context(), "", baseSHA, "")
	require.NoError(t, err, "--since should bypass upstream resolution: %v", err)

	assert.NotEmpty(t, repoPath)
	assert.Equal(t, "main", currentBranch)
	assert.Equal(t, baseSHA, mergeBase)
}

func TestValidateRefineContext_UpstreamMissingErrorsWithoutSince(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Without --since, a configured-but-unfetched upstream must surface
	// as UpstreamMissingError rather than silently falling back to the
	// repository default branch (which could yield the wrong range).
	repo := testutil.InitTestRepo(t)
	repo.CheckoutNewBranch("feature")
	repo.CommitFile("feature.txt", "feature", "feature commit")
	repo.SetBranchConfig("feature", "remote", "upstream")
	repo.SetBranchConfig("feature", "merge", "refs/heads/main")

	chdirForTest(t, repo.Root)

	_, _, _, _, err := validateRefineContext(t.Context(), "", "", "")
	require.Error(t, err, "expected missing-upstream error without --since")

	var missing *git.UpstreamMissingError
	assert.ErrorAs(t, err, &missing, "expected UpstreamMissingError, got: %v", err)
}

func TestValidateRefineContext_SinceWorksOnFeatureBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)
	baseSHA := repo.RevParse("HEAD")

	repo.CheckoutNewBranch("feature")
	repo.CommitFile("feature.txt", "feature", "feature commit")

	chdirForTest(t, repo.Root)

	repoPath, currentBranch, _, mergeBase, err := validateRefineContext(t.Context(), "", baseSHA, "")
	require.NoError(t, err, "--since should work on feature branch, got: %v")

	assert.NotEmpty(t, repoPath)
	assert.Equal(t, "feature", currentBranch)
	assert.Equal(t, mergeBase, baseSHA)
}

func TestValidateRefineContext_InvalidSinceRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)

	chdirForTest(t, repo.Root)

	_, _, _, _, err := validateRefineContext(t.Context(), "", "nonexistent-ref-abc123", "")
	require.Error(t, err, "expected error for invalid --since ref")
	assert.Contains(t, err.Error(), "cannot resolve --since")
}

func TestValidateRefineContext_SinceNotAncestorOfHEAD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)

	repo.CheckoutNewBranch("other-branch")
	repo.CommitFile("other.txt", "other", "commit on other branch")
	otherBranchSHA := repo.RevParse("HEAD")

	repo.CheckoutBranch("main")
	repo.CommitFile("main2.txt", "main2", "second commit on main")

	chdirForTest(t, repo.Root)

	_, _, _, _, err := validateRefineContext(t.Context(), "", otherBranchSHA, "")
	require.Error(t, err, "expected error when --since is not an ancestor of HEAD")
	assert.Contains(t, err.Error(), "not an ancestor of HEAD")
}

func TestValidateRefineContext_PrefersNonOriginUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Fork workflow: origin (stale fork) lags behind upstream (real repo).
	// Feature branch tracks upstream/main. validateRefineContext must
	// pick upstream/main as the base so commits already merged upstream
	// are not refined.
	repo := testutil.InitTestRepo(t)
	staleOriginSHA := repo.RevParse("HEAD")
	upstreamSHA := repo.CommitFile("upstream_c2.go", "package main", "upstream c2")

	repo.AddRemote("upstream", "/dev/null")
	repo.SetRef("refs/remotes/upstream/main", upstreamSHA)
	repo.SetBranchConfig("main", "remote", "upstream")
	repo.SetBranchConfig("main", "merge", "refs/heads/main")

	repo.AddRemote("origin", "/dev/null")
	repo.SetRef("refs/remotes/origin/main", staleOriginSHA)
	repo.SetRemoteHead("origin", "main")

	repo.CheckoutNewBranch("feature", upstreamSHA)
	repo.SetBranchConfig("feature", "remote", "upstream")
	repo.SetBranchConfig("feature", "merge", "refs/heads/main")
	repo.CommitFile("feature.go", "package main", "feature commit")

	chdirForTest(t, repo.Root)

	repoPath, currentBranch, base, mergeBase, err := validateRefineContext(t.Context(), "", "", "")
	require.NoError(t, err, "validation should pass on feature tracking upstream/main")

	assert.NotEmpty(t, repoPath)
	assert.Equal(t, "feature", currentBranch)
	assert.Equal(t, "upstream/main", base,
		"base must resolve to the branch's upstream, not origin/main")
	assert.Equal(t, upstreamSHA, mergeBase,
		"merge-base must be upstream/main's tip so already-merged commits aren't refined")
}

func TestValidateRefineContext_RefusesLocalMainTrackingNonOriginUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Local main tracking upstream/main must still hit the "refusing to
	// refine on main" guardrail even though base is "upstream/main"
	// (not "origin/main"). Regression for IsOnBaseBranch's non-origin
	// remote handling.
	repo := testutil.InitTestRepo(t)
	mainSHA := repo.HeadSHA()
	repo.AddRemote("upstream", "/dev/null")
	repo.SetRef("refs/remotes/upstream/main", mainSHA)
	repo.SetBranchConfig("main", "remote", "upstream")
	repo.SetBranchConfig("main", "merge", "refs/heads/main")

	chdirForTest(t, repo.Root)

	_, _, _, _, err := validateRefineContext(t.Context(), "", "", "")
	require.Error(t, err, "expected refuse-on-base error for main tracking upstream/main")
	assert.Contains(t, err.Error(), "refusing to refine on main")
}

func TestValidateRefineContext_FeatureBranchWithoutSinceStillWorks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)
	baseSHA := repo.RevParse("HEAD")

	repo.CheckoutNewBranch("feature")
	repo.CommitFile("feature.txt", "feature", "feature commit")

	chdirForTest(t, repo.Root)

	repoPath, currentBranch, _, mergeBase, err := validateRefineContext(t.Context(), "", "", "")
	require.NoError(t, err, "feature branch without --since should work, got: %v")

	assert.NotEmpty(t, repoPath)
	assert.Equal(t, "feature", currentBranch)

	assert.Equal(t, mergeBase, baseSHA)
}

func TestCommitWithHookRetrySucceeds(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)

	repo.WriteNamedHook("pre-commit", `#!/bin/sh
COUNT_FILE=".git/hook-count"
COUNT=0
if [ -f "$COUNT_FILE" ]; then
    COUNT=$(cat "$COUNT_FILE")
fi
COUNT=$((COUNT + 1))
echo "$COUNT" > "$COUNT_FILE"
if [ "$COUNT" -le 2 ]; then
    echo "lint error: trailing whitespace" >&2
    exit 1
fi
exit 0
`)

	if err := os.WriteFile(filepath.Join(repo.Root, "new.txt"), []byte("hello"), 0o644); err != nil {
		require.NoError(t, err)
	}

	testAgent := agent.NewTestAgent()
	sha, err := commitWithHookRetry(t.Context(), repo.Root, "test commit", testAgent, true, git.CommitOptions{})
	require.NoError(t, err, "commitWithHookRetry should succeed: %v")

	require.NotEmpty(t, sha, "expected non-empty SHA")

	commitSHA := repo.RevParse("HEAD")
	assert.Equal(t, commitSHA, sha)
}

func TestCommitWithHookRetryUsesCommitOptions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)

	if err := os.WriteFile(filepath.Join(repo.Root, "new.txt"), []byte("hello"), 0o644); err != nil {
		require.NoError(t, err)
	}

	testAgent := agent.NewTestAgent()
	_, err := commitWithHookRetry(
		t.Context(),
		repo.Root,
		"test commit",
		testAgent,
		true,
		git.CommitOptions{
			Author:    "Fix Author <fix@example.com>",
			CoAuthors: []string{"Pair Reviewer <pair@example.com>"},
		},
	)
	require.NoError(t, err)

	show := repo.Run("show", "-s", "--format=%an <%ae>%n%B", "HEAD")
	assert.Contains(t, show, "Fix Author <fix@example.com>")
	assert.Contains(t, show, "Co-authored-by: Pair Reviewer <pair@example.com>")
}

func TestChangedRefineSubmodulesDetectsDirtySubmoduleIgnoredByParentStatus(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	submoduleSource := NewGitTestRepo(t)
	submoduleSource.CommitFile("sub.txt", "base\n", "submodule base")

	parent := NewGitTestRepo(t)
	parent.CommitFile("parent.txt", "base\n", "parent base")
	parent.Run("-c", "protocol.file.allow=always", "submodule", "add", submoduleSource.Dir, "vendor/sub")
	parent.Run("commit", "-m", "add submodule")

	before, err := snapshotRefineSubmodules(t.Context(), parent.Dir)
	require.NoError(t, err)

	parent.Run("config", "submodule.vendor/sub.ignore", "dirty")
	require.NoError(t, os.WriteFile(
		filepath.Join(parent.Dir, "vendor", "sub", "sub.txt"),
		[]byte("dirty\n"),
		0o644,
	))
	require.True(t, git.IsWorkingTreeClean(parent.Dir))

	changed, err := changedRefineSubmodules(t.Context(), parent.Dir, before)

	require.NoError(t, err)
	assert.Equal(t, []string{"vendor/sub"}, changed)
}

func TestChangedRefineSubmodulesDetectsGitlinkOnlyChange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	submoduleSource := NewGitTestRepo(t)
	submoduleSource.CommitFile("sub.txt", "base\n", "submodule base")

	parent := NewGitTestRepo(t)
	parent.CommitFile("parent.txt", "base\n", "parent base")
	parent.Run("-c", "protocol.file.allow=always", "submodule", "add", submoduleSource.Dir, "vendor/sub")
	parent.Run("commit", "-m", "add submodule")

	before, err := snapshotRefineSubmodules(t.Context(), parent.Dir)
	require.NoError(t, err)

	nextSHA := submoduleSource.CommitFile("sub.txt", "next\n", "submodule next")
	parent.Run("update-index", "--cacheinfo", "160000", nextSHA, "vendor/sub")

	changed, err := changedRefineSubmodules(t.Context(), parent.Dir, before)

	require.NoError(t, err)
	assert.Equal(t, []string{"vendor/sub"}, changed)
}

func TestChangedRefineSubmodulesDetectsGitmodulesOnlyChange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	submoduleSource := NewGitTestRepo(t)
	submoduleSource.CommitFile("sub.txt", "base\n", "submodule base")

	parent := NewGitTestRepo(t)
	parent.CommitFile("parent.txt", "base\n", "parent base")
	parent.Run("-c", "protocol.file.allow=always", "submodule", "add", submoduleSource.Dir, "vendor/sub")
	parent.Run("commit", "-m", "add submodule")

	before, err := snapshotRefineSubmodules(t.Context(), parent.Dir)
	require.NoError(t, err)

	parent.Run("config", "-f", ".gitmodules", "submodule.vendor/sub.branch", "main")

	changed, err := changedRefineSubmodules(t.Context(), parent.Dir, before)

	require.NoError(t, err)
	assert.Equal(t, []string{"vendor/sub"}, changed)
}

func TestCommitWithHookRetryDoesNotRunHookFixAgentWithSubmodules(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	submoduleSource := NewGitTestRepo(t)
	submoduleSource.CommitFile("sub.txt", "base\n", "submodule base")

	parent := NewGitTestRepo(t)
	parent.CommitFile("parent.txt", "base\n", "parent base")
	parent.Run("-c", "protocol.file.allow=always", "submodule", "add", submoduleSource.Dir, "vendor/sub")
	parent.Run("commit", "-m", "add submodule")
	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, ".git", "hooks", "pre-commit"), []byte(`#!/bin/sh
echo "hook failure" >&2
exit 1
`), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, "new.txt"), []byte("hello\n"), 0o644))
	agentCalled := false
	testAgent := &functionalMockAgent{nameVal: "test", reviewFunc: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
		agentCalled = true
		return "Changes:\n- no-op", nil
	}}

	_, err := commitWithHookRetry(t.Context(), parent.Dir, "test commit", testAgent, true, git.CommitOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot automatically retry hooks in repositories with git submodules")
	assert.False(t, agentCalled)
}

func TestCommitWithHookRetryRestoresSubmoduleGitlinkFromFailedHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	submoduleSource := NewGitTestRepo(t)
	submoduleSource.CommitFile("sub.txt", "base\n", "submodule base")

	parent := NewGitTestRepo(t)
	parent.CommitFile("parent.txt", "base\n", "parent base")
	parent.Run("-c", "protocol.file.allow=always", "submodule", "add", submoduleSource.Dir, "vendor/sub")
	parent.Run("commit", "-m", "add submodule")
	gitlinkBefore := parent.Run("ls-files", "--stage", "--", "vendor/sub")
	nextSHA := submoduleSource.CommitFile("sub.txt", "next\n", "submodule next")
	hookScript := fmt.Sprintf(`#!/bin/sh
set -e
git update-index --cacheinfo 160000 %s vendor/sub
echo "hook failure" >&2
exit 1
`, nextSHA)
	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, ".git", "hooks", "pre-commit"), []byte(hookScript), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, "new.txt"), []byte("hello\n"), 0o644))
	agentCalled := false
	testAgent := &functionalMockAgent{nameVal: "test", reviewFunc: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
		agentCalled = true
		return "Changes:\n- no-op", nil
	}}

	_, err := commitWithHookRetry(t.Context(), parent.Dir, "test commit", testAgent, true, git.CommitOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "roborev refine cannot modify git submodules")
	assert.Contains(t, err.Error(), "vendor/sub")
	assert.Equal(t, gitlinkBefore, parent.Run("ls-files", "--stage", "--", "vendor/sub"))
	assert.False(t, agentCalled)
}

func TestCommitWithHookRetryRestoresSubmoduleWorktreeFromFailedHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	submoduleSource := NewGitTestRepo(t)
	submoduleSource.CommitFile("sub.txt", "base\n", "submodule base")

	parent := NewGitTestRepo(t)
	parent.CommitFile("parent.txt", "base\n", "parent base")
	parent.Run("-c", "protocol.file.allow=always", "submodule", "add", submoduleSource.Dir, "vendor/sub")
	parent.Run("commit", "-m", "add submodule")
	submoduleFile := filepath.Join(parent.Dir, "vendor", "sub", "sub.txt")
	generatedFile := filepath.Join(parent.Dir, "vendor", "sub", "generated.tmp")
	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, ".git", "hooks", "pre-commit"), []byte(`#!/bin/sh
printf 'changed by hook\n' > vendor/sub/sub.txt
printf 'generated by hook\n' > vendor/sub/generated.tmp
echo "hook failure" >&2
exit 1
`), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, "new.txt"), []byte("hello\n"), 0o644))
	agentCalled := false
	testAgent := &functionalMockAgent{nameVal: "test", reviewFunc: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
		agentCalled = true
		return "Changes:\n- no-op", nil
	}}

	_, err := commitWithHookRetry(t.Context(), parent.Dir, "test commit", testAgent, true, git.CommitOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "roborev refine cannot modify git submodules")
	assert.Contains(t, err.Error(), "vendor/sub")
	content, readErr := os.ReadFile(submoduleFile)
	require.NoError(t, readErr)
	assert.Equal(t, "base\n", string(content))
	assert.NoFileExists(t, generatedFile)
	assert.False(t, agentCalled)
}

func TestCommitWithHookRetryRestoresNewSubmoduleFromFailedHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	submoduleSource := NewGitTestRepo(t)
	submoduleSource.CommitFile("sub.txt", "base\n", "submodule base")

	parent := NewGitTestRepo(t)
	parent.CommitFile("parent.txt", "base\n", "parent base")
	submoduleSourcePath := filepath.ToSlash(submoduleSource.Dir)
	hookScript := fmt.Sprintf(`#!/bin/sh
set -e
git -c protocol.file.allow=always submodule add %s vendor/new
echo "hook failure" >&2
exit 1
`, submoduleSourcePath)
	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, ".git", "hooks", "pre-commit"), []byte(hookScript), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, "new.txt"), []byte("hello\n"), 0o644))
	agentCalled := false
	testAgent := &functionalMockAgent{nameVal: "test", reviewFunc: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
		agentCalled = true
		return "Changes:\n- no-op", nil
	}}

	_, err := commitWithHookRetry(t.Context(), parent.Dir, "test commit", testAgent, true, git.CommitOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "roborev refine cannot modify git submodules")
	assert.Contains(t, err.Error(), "vendor/new")
	assert.Empty(t, parent.Run("ls-files", "--stage", "--", "vendor/new"))
	assert.NoFileExists(t, filepath.Join(parent.Dir, ".gitmodules"))
	assert.DirExists(t, filepath.Join(parent.Dir, "vendor", "new"))
	assert.False(t, agentCalled)
}

func TestCommitWithHookRetryPreservesExistingPathFromFailedHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	submoduleSource := NewGitTestRepo(t)
	newSubmoduleSHA := submoduleSource.CommitFile("sub.txt", "base\n", "submodule base")

	parent := NewGitTestRepo(t)
	parent.CommitFile(".gitignore", "/vendor/existing/\n", "ignore existing vendor path")
	existingFile := filepath.Join(parent.Dir, "vendor", "existing", "keep.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(existingFile), 0o755))
	require.NoError(t, os.WriteFile(existingFile, []byte("keep\n"), 0o644))
	hookScript := fmt.Sprintf(`#!/bin/sh
set -e
git update-index --add --cacheinfo 160000 %s vendor/existing
echo "hook failure" >&2
exit 1
`, newSubmoduleSHA)
	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, ".git", "hooks", "pre-commit"), []byte(hookScript), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, "new.txt"), []byte("hello\n"), 0o644))
	agentCalled := false
	testAgent := &functionalMockAgent{nameVal: "test", reviewFunc: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
		agentCalled = true
		return "Changes:\n- no-op", nil
	}}

	_, err := commitWithHookRetry(t.Context(), parent.Dir, "test commit", testAgent, true, git.CommitOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "roborev refine cannot modify git submodules")
	assert.Contains(t, err.Error(), "vendor/existing")
	assert.Empty(t, parent.Run("ls-files", "--stage", "--", "vendor/existing"))
	content, readErr := os.ReadFile(existingFile)
	require.NoError(t, readErr)
	assert.Equal(t, "keep\n", string(content))
	assert.False(t, agentCalled)
}

func TestCommitWithHookRetryPreservesGitmodulesWithoutPriorGitlinks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	submoduleSource := NewGitTestRepo(t)
	submoduleSource.CommitFile("sub.txt", "base\n", "submodule base")

	parent := NewGitTestRepo(t)
	originalGitmodules := "[submodule \"historical\"]\n\tpath = historical\n\turl = https://example.invalid/historical.git\n"
	parent.CommitFile(".gitmodules", originalGitmodules, "historical gitmodules")
	submoduleSourcePath := filepath.ToSlash(submoduleSource.Dir)
	hookScript := fmt.Sprintf(`#!/bin/sh
set -e
git -c protocol.file.allow=always submodule add %s vendor/new
echo "hook failure" >&2
exit 1
`, submoduleSourcePath)
	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, ".git", "hooks", "pre-commit"), []byte(hookScript), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, "new.txt"), []byte("hello\n"), 0o644))
	agentCalled := false
	testAgent := &functionalMockAgent{nameVal: "test", reviewFunc: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
		agentCalled = true
		return "Changes:\n- no-op", nil
	}}

	_, err := commitWithHookRetry(t.Context(), parent.Dir, "test commit", testAgent, true, git.CommitOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "roborev refine cannot modify git submodules")
	assert.Contains(t, err.Error(), "vendor/new")
	content, readErr := os.ReadFile(filepath.Join(parent.Dir, ".gitmodules"))
	require.NoError(t, readErr)
	assert.Equal(t, originalGitmodules, string(content))
	assert.Empty(t, parent.Run("status", "--porcelain", "--", ".gitmodules"))
	assert.False(t, agentCalled)
}

func TestCommitWithHookRetryRestoresGitmodulesOnlyFailedHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	parent := NewGitTestRepo(t)
	parent.CommitFile("parent.txt", "base\n", "parent base")
	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, ".git", "hooks", "pre-commit"), []byte(`#!/bin/sh
cat > .gitmodules <<'EOF'
[submodule "metadata-only"]
	path = metadata-only
	url = https://example.invalid/metadata-only.git
EOF
echo "hook failure" >&2
exit 1
`), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, "new.txt"), []byte("hello\n"), 0o644))
	agentCalled := false
	testAgent := &functionalMockAgent{nameVal: "test", reviewFunc: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
		agentCalled = true
		return "Changes:\n- no-op", nil
	}}

	_, err := commitWithHookRetry(t.Context(), parent.Dir, "test commit", testAgent, true, git.CommitOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "roborev refine cannot modify git submodules")
	assert.Contains(t, err.Error(), ".gitmodules")
	assert.NoFileExists(t, filepath.Join(parent.Dir, ".gitmodules"))
	assert.False(t, agentCalled)
}

func TestCommitWithHookRetryRollsBackIndexedGitmodulesFromSuccessfulHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	parent := NewGitTestRepo(t)
	parent.CommitFile("parent.txt", "base\n", "parent base")
	headBefore := parent.Run("rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, ".git", "hooks", "pre-commit"), []byte(`#!/bin/sh
blob=$(printf '[submodule "indexed"]\n\tpath = indexed\n\turl = https://example.invalid/indexed.git\n' | git hash-object -w --stdin)
git update-index --add --cacheinfo 100644 "$blob" .gitmodules
exit 0
`), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, "new.txt"), []byte("hello\n"), 0o644))

	_, err := commitWithHookRetry(t.Context(), parent.Dir, "test commit", agent.NewTestAgent(), true, git.CommitOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "roborev refine cannot modify git submodules")
	assert.Contains(t, err.Error(), ".gitmodules")
	assert.Equal(t, headBefore, parent.Run("rev-parse", "HEAD"))
	assert.Empty(t, parent.Run("ls-files", "--stage", "--", ".gitmodules"))
	assert.NoFileExists(t, filepath.Join(parent.Dir, ".gitmodules"))
}

func TestCommitWithHookRetryRollsBackSubmoduleGitlinkFromSuccessfulHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	submoduleSource := NewGitTestRepo(t)
	submoduleSource.CommitFile("sub.txt", "base\n", "submodule base")

	parent := NewGitTestRepo(t)
	parent.CommitFile("parent.txt", "base\n", "parent base")
	parent.Run("-c", "protocol.file.allow=always", "submodule", "add", submoduleSource.Dir, "vendor/sub")
	parent.Run("commit", "-m", "add submodule")
	headBefore := parent.Run("rev-parse", "HEAD")
	nextSHA := submoduleSource.CommitFile("sub.txt", "next\n", "submodule next")
	hookScript := fmt.Sprintf(`#!/bin/sh
set -e
git update-index --cacheinfo 160000 %s vendor/sub
exit 0
`, nextSHA)
	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, ".git", "hooks", "pre-commit"), []byte(hookScript), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(parent.Dir, "new.txt"), []byte("hello\n"), 0o644))

	_, err := commitWithHookRetry(t.Context(), parent.Dir, "test commit", agent.NewTestAgent(), true, git.CommitOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "roborev refine cannot modify git submodules")
	assert.Contains(t, err.Error(), "vendor/sub")
	assert.Equal(t, headBefore, parent.Run("rev-parse", "HEAD"))
}

func TestCommitWithHookRetryExhausted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)

	repo.WriteNamedHook("pre-commit",
		"#!/bin/sh\necho 'always fails' >&2\nexit 1\n")

	if err := os.WriteFile(filepath.Join(repo.Root, "new.txt"), []byte("hello"), 0o644); err != nil {
		require.NoError(t, err)
	}

	testAgent := agent.NewTestAgent()
	_, err := commitWithHookRetry(t.Context(), repo.Root, "test commit", testAgent, true, git.CommitOptions{})
	require.Error(t, err, "expected error after exhausting retries")
	assert.Contains(t, err.Error(), "after 3 attempts")
}

func TestCommitWithHookRetrySkipsNonHookError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)

	testAgent := agent.NewTestAgent()
	_, err := commitWithHookRetry(t.Context(), repo.Root, "empty commit", testAgent, true, git.CommitOptions{})
	require.Error(t, err, "expected error for empty commit without hook")

	assert.NotContains(t, err.Error(), "pre-commit hook failed")
	assert.NotContains(t, err.Error(), "after 3 attempts")
}

func TestCommitWithHookRetrySkipsAddPhaseError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)

	repo.WriteNamedHook("pre-commit", "#!/bin/sh\nexit 0\n")

	if err := os.WriteFile(filepath.Join(repo.Root, "new.txt"), []byte("hello"), 0o644); err != nil {
		require.NoError(t, err)
	}

	lockFile := filepath.Join(repo.Root, ".git", "index.lock")
	if err := os.WriteFile(lockFile, []byte(""), 0o644); err != nil {
		require.NoError(t, err)
	}
	defer os.Remove(lockFile)

	testAgent := agent.NewTestAgent()
	_, err := commitWithHookRetry(t.Context(), repo.Root, "test commit", testAgent, true, git.CommitOptions{})
	require.Error(t, err, "expected error with index.lock present")

	assert.NotContains(t, err.Error(), "pre-commit hook failed")
	assert.NotContains(t, err.Error(), "after 3 attempts")
}

func TestCommitWithHookRetrySkipsCommitPhaseNonHookError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := testutil.InitTestRepo(t)

	repo.WriteNamedHook("pre-commit", "#!/bin/sh\nexit 0\n")

	testAgent := agent.NewTestAgent()
	_, err := commitWithHookRetry(t.Context(), repo.Root, "empty commit", testAgent, true, git.CommitOptions{})
	require.Error(t, err, "expected error for empty commit")

	assert.NotContains(t, err.Error(), "pre-commit hook failed")
	assert.NotContains(t, err.Error(), "after 3 attempts")
}

func TestResolveReasoningWithFast(t *testing.T) {
	tests := []struct {
		name                   string
		reasoning              string
		fast                   bool
		reasoningExplicitlySet bool
		want                   string
	}{
		{
			name:                   "fast flag sets reasoning to fast",
			reasoning:              "",
			fast:                   true,
			reasoningExplicitlySet: false,
			want:                   "fast",
		},
		{
			name:                   "explicit reasoning takes precedence over fast",
			reasoning:              "thorough",
			fast:                   true,
			reasoningExplicitlySet: true,
			want:                   "thorough",
		},
		{
			name:                   "no fast flag preserves reasoning",
			reasoning:              "standard",
			fast:                   false,
			reasoningExplicitlySet: true,
			want:                   "standard",
		},
		{
			name:                   "no flags returns empty",
			reasoning:              "",
			fast:                   false,
			reasoningExplicitlySet: false,
			want:                   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveReasoningWithFast(tt.reasoning, tt.fast, tt.reasoningExplicitlySet)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestApplyModelForAgent_BackupKeepsOwnModel(t *testing.T) {
	t.Cleanup(testutil.MockExecutableIsolated(t, "codex", 0))

	selected, err := selectRefineAgent(
		"", nil, "gemini", agent.ReasoningStandard, "codex",
	)
	require.NoError(t, err, "selectRefineAgent: %v")

	assert.Equal(t, "codex", selected.Name())

	result, model := applyModelForAgent(
		selected,
		"gemini",
		"codex",
		"",
		"",
		nil,
		"refine",
		"standard",
	)

	codexAgent, ok := result.(*agent.CodexAgent)
	assert.True(t, ok)
	if codexAgent.Model != "" {
		assert.Empty(t, codexAgent.Model, "backup agent should keep its default model (empty), got %q", codexAgent.Model)
	}
	assert.Empty(t, model)
}

func TestApplyModelForAgent_EmptyModelPreservesAgentDefault(t *testing.T) {
	t.Cleanup(testutil.MockExecutable(t, "codex", 0))

	a, err := agent.Get("codex")
	require.NoError(t, err, "agent.Get: %v")

	a = a.WithModel("o3")

	result, _ := applyModelForAgent(
		a,
		"codex",
		"",
		"",
		"",
		nil,
		"review",
		"standard",
	)

	codexAgent, ok := result.(*agent.CodexAgent)
	assert.True(t, ok)
	assert.Equal(t, "o3", codexAgent.Model, "agent model should remain %q, got %q", "o3", codexAgent.Model)
}

func TestApplyModelForAgent_SameAgentPrimaryAndBackup(t *testing.T) {
	t.Cleanup(testutil.MockExecutable(t, "codex", 0))

	a, err := agent.Get("codex")
	require.NoError(t, err, "agent.Get: %v")

	cfg := &config.Config{
		ReviewModel:       "primary-model",
		ReviewBackupModel: "backup-model",
	}

	result, model := applyModelForAgent(
		a,
		"codex",
		"codex",
		"",
		"",
		cfg,
		"review",
		"standard",
	)

	codexAgent, ok := result.(*agent.CodexAgent)
	assert.True(t, ok)

	assert.Equal(t, "primary-model", model)
	assert.Equal(t, "primary-model", codexAgent.Model, "expected agent model %q, got %q", "primary-model", codexAgent.Model)
}

func TestApplyModelForAgentFallbackUsesDefaultModelForActualAgent(t *testing.T) {
	a, err := agent.Get("codex")
	require.NoError(t, err, "agent.Get: %v")

	cfg := &config.Config{
		DefaultAgent: "codex",
		DefaultModel: "gpt-5.4",
		ReviewAgent:  "claude",
	}

	result, model := applyModelForAgent(
		a,
		"claude",
		"",
		"",
		"",
		cfg,
		"review",
		"standard",
	)

	codexAgent, ok := result.(*agent.CodexAgent)
	assert.True(t, ok)
	assert.Equal(t, "gpt-5.4", model)
	assert.Equal(t, "gpt-5.4", codexAgent.Model)
}

func TestRefineFlagValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "all-branches and branch mutually exclusive",
			args:    []string{"--all-branches", "--branch", "main"},
			wantErr: "--all-branches and --branch are mutually exclusive",
		},
		{
			name:    "all-branches and since mutually exclusive",
			args:    []string{"--all-branches", "--since", "abc123"},
			wantErr: "--all-branches and --since are mutually exclusive",
		},
		{
			name:    "newest-first requires all-branches or list",
			args:    []string{"--newest-first"},
			wantErr: "--newest-first requires --all-branches or --list",
		},
		{
			name: "newest-first with list is accepted",
			args: []string{"--newest-first", "--list"},

			wantErr: "",
		},
		{
			name:    "newest-first with all-branches is accepted",
			args:    []string{"--newest-first", "--all-branches"},
			wantErr: "",
		},
		{
			name:    "list and since mutually exclusive",
			args:    []string{"--list", "--since", "abc123"},
			wantErr: "--list and --since are mutually exclusive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := refineCmd()
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if tt.wantErr != "" {
				require.Error(t, err, "expected error, got nil")
				if !strings.Contains(err.Error(), tt.wantErr) {
					assert.Contains(t, err.Error(), tt.wantErr)
				}
			} else if err != nil {
				msg := err.Error()
				isValidationErr := strings.Contains(msg, "mutually exclusive") ||
					strings.Contains(msg, "requires --")
				assert.False(t, isValidationErr)
			}
		})
	}
}
