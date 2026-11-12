package daemon

import (
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestKataCreateRequestFailed(t *testing.T) {
	hook := config.HookConfig{Type: "kata"}
	event := Event{Type: "review.failed", JobID: 7, RepoName: "myrepo", SHA: "abc123def456", Agent: "codex", Error: "boom", TS: time.Unix(1700000000, 0).UTC()}

	req, ok := kataCreateRequest(hook, event, kataCommit{})
	require.True(t, ok)
	assert.Contains(t, req.Title, "Review failed for myrepo")
	assert.Contains(t, req.Title, "roborev show 7")
	require.NotNil(t, req.Priority)
	assert.Equal(t, 1, *req.Priority)
	assert.Contains(t, req.Labels, "roborev")
	assert.Contains(t, req.Labels, "review-failed")
	assert.Equal(t, "roborev:7:review.failed:abc123def456", req.IdempotencyKey)
	assert.Contains(t, req.Body, "boom")
	assert.Contains(t, req.Body, "roborev show 7")
}

func TestKataCreateRequestPrefersUUID(t *testing.T) {
	hook := config.HookConfig{Type: "kata"}
	jobUUID := testUUID("uuid-abc")
	event := Event{Type: "review.failed", JobID: 7, JobUUID: &jobUUID, SHA: "deadbeef"}
	req, ok := kataCreateRequest(hook, event, kataCommit{})
	require.True(t, ok)
	assert.Equal(t, "roborev:job:"+jobUUID.String()+":review.failed", req.IdempotencyKey) //nolint:forbidigo // Kata idempotency key text boundary.
}

func TestKataCreateRequestFindings(t *testing.T) {
	p := 4
	hook := config.HookConfig{Type: "kata", Project: "proj", Labels: []string{"team"}, Priority: &p}
	event := Event{Type: "review.completed", Verdict: "F", JobID: 9, RepoName: "r", SHA: "deadbeef", Findings: "## Bug\nbad"}

	req, ok := kataCreateRequest(hook, event, kataCommit{})
	require.True(t, ok)
	assert.Equal(t, "proj", req.Project)
	assert.Equal(t, 4, *req.Priority) // configured priority wins
	assert.Contains(t, req.Labels, "team")
	assert.Contains(t, req.Labels, "review-finding")
	assert.Contains(t, req.Body, "roborev fix 9")
	assert.Contains(t, req.Body, "bad")
	assert.Contains(t, req.Body, "\n\nFix:")
}

func TestKataCreateRequestPassingSkipped(t *testing.T) {
	_, ok := kataCreateRequest(config.HookConfig{Type: "kata"}, Event{Type: "review.completed", Verdict: "P", JobID: 1}, kataCommit{})
	assert.False(t, ok)
}

func TestKataHookBodyTruncated(t *testing.T) {
	event := Event{Type: "review.completed", Verdict: "F", JobID: 2, SHA: "s", Findings: strings.Repeat("x", kataHookMaxBodyBytes*2)}
	req, ok := kataCreateRequest(config.HookConfig{Type: "kata"}, event, kataCommit{})
	require.True(t, ok)
	assert.LessOrEqual(t, len(req.Body), kataHookMaxBodyBytes+64)
	assert.Contains(t, req.Body, "truncated")
}

func TestRunKataHookCreatesAndLogs(t *testing.T) {
	var buf strings.Builder
	fake := &katatest.FakeClient{CreateResult: kata.CreateResult{ShortID: "new1"}}
	hr := &HookRunner{
		logger:        log.New(&buf, "", 0),
		newKataClient: func(string) kata.Client { return fake },
	}
	hr.wg.Add(1)
	hr.runKataHook(config.HookConfig{Type: "kata", Project: "p"}, Event{Type: "review.failed", JobID: 5, SHA: "x"}, t.TempDir())

	require.Len(t, fake.CreateReqs, 1)
	assert.Contains(t, buf.String(), "created issue new1")
}

func TestRunKataHookLogsNoBinding(t *testing.T) {
	var buf strings.Builder
	fake := &katatest.FakeClient{BindingErr: kata.ErrNoBinding}
	hr := &HookRunner{
		logger:        log.New(&buf, "", 0),
		newKataClient: func(string) kata.Client { return fake },
	}
	hr.wg.Add(1)
	hr.runKataHook(config.HookConfig{Type: "kata"}, Event{Type: "review.failed", JobID: 5, SHA: "x"}, t.TempDir())

	assert.Empty(t, fake.CreateReqs, "must not create without a binding")
	assert.Contains(t, buf.String(), "no .kata.toml binding")
}

// runKataHookFor files the event through runKataHook in repoDir and returns
// the requests the fake Kata client received.
func runKataHookFor(t *testing.T, event Event, repoDir string) []kata.CreateReq {
	t.Helper()
	var buf strings.Builder
	fake := &katatest.FakeClient{CreateResult: kata.CreateResult{ShortID: "new1"}}
	hr := &HookRunner{
		logger:        log.New(&buf, "", 0),
		newKataClient: func(string) kata.Client { return fake },
	}
	hr.wg.Add(1)
	hr.runKataHook(config.HookConfig{Type: "kata", Project: "p"}, event, repoDir)
	return fake.CreateReqs
}

func TestRunKataHookLinksLandedCommitAndPullRequest(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	parent := repo.CommitFile("app.txt", "one\n", "initial")
	landed := repo.CommitFile("app.txt", "two\n", "fix(api): validate input (#42)")
	repo.Run("remote", "add", "origin", "https://x-access-token:hidden-secret@github.com/acme/api.git")

	reqs := runKataHookFor(t, Event{
		Type: "review.completed", Verdict: "F", JobID: 9, RepoName: "api",
		SHA: parent + ".." + landed, PanelRole: storage.PanelRoleSynthesis,
		Source: storage.JobSourceCIBranch, Findings: "- [P1] Missing validation",
	}, repo.Path())

	require.Len(t, reqs, 1)
	req := reqs[0]
	assert.Equal(t, "Review findings for api ("+landed[:7]+"): fix(api): validate input (#42)", req.Title,
		"the title names the landed commit, not the start of the reviewed range")
	assert.Contains(t, req.Body, "- commit link: https://github.com/acme/api/commit/"+landed+"\n")
	assert.Contains(t, req.Body, "- pull request: https://github.com/acme/api/pull/42\n")
	assert.Contains(t, req.Body, "Missing validation")
	assert.NotContains(t, req.Body, "hidden-secret", "remote credentials stay out of issue text")
}

func TestRunKataHookLinksMergeCommitPullRequest(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("app.txt", "one\n", "initial")
	landed := repo.CommitFile("app.txt", "two\n", "Merge pull request #7 from acme/feature")
	repo.Run("remote", "add", "origin", "git@github.com:acme/api.git")

	reqs := runKataHookFor(t, Event{Type: "review.completed", Verdict: "F", JobID: 9, SHA: landed}, repo.Path())
	require.Len(t, reqs, 1)
	assert.Contains(t, reqs[0].Body, "- pull request: https://github.com/acme/api/pull/7\n")
}

func TestRunKataHookOmitsLinksForOtherHosts(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	landed := repo.CommitFile("app.txt", "one\n", "change (#3)")
	repo.Run("remote", "add", "origin", "https://git.example.org/acme/api.git")

	reqs := runKataHookFor(t, Event{Type: "review.completed", Verdict: "F", JobID: 9, SHA: landed}, repo.Path())
	require.Len(t, reqs, 1)
	assert.NotContains(t, reqs[0].Body, "link:")
	assert.NotContains(t, reqs[0].Body, "pull request:")
	assert.Contains(t, reqs[0].Title, "change (#3)")
}

func TestRunKataHookFilesPanelResultOnce(t *testing.T) {
	member := Event{Type: "review.completed", Verdict: "F", JobID: 3, SHA: "x", PanelRole: storage.PanelRoleMember}
	assert.Empty(t, runKataHookFor(t, member, t.TempDir()), "a member's findings arrive through the synthesis")
	failedMember := Event{Type: "review.failed", JobID: 4, SHA: "x", PanelRole: storage.PanelRoleMember, Error: "boom"}
	assert.Empty(t, runKataHookFor(t, failedMember, t.TempDir()))

	synthesis := Event{Type: "review.completed", Verdict: "F", JobID: 5, SHA: "x", PanelRole: storage.PanelRoleSynthesis}
	assert.Len(t, runKataHookFor(t, synthesis, filepath.Join(t.TempDir(), "missing")), 1,
		"an issue is filed even when the commit cannot be read")
}
