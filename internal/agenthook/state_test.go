package agenthook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestIsCommitProducingCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    bool
	}{
		{name: "commit", command: "git commit -m test", want: true},
		{name: "cherry pick with git options", command: "git -C /tmp/repo cherry-pick abc123", want: true},
		{name: "commit with quoted path option", command: `git -C "/tmp/repo with spaces" commit -m test`, want: true},
		{name: "revert with config option", command: "git -c user.name=test revert abc123", want: true},
		{name: "status", command: "git status", want: false},
		{name: "empty", command: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsCommitProducingCommand(tc.command))
		})
	}
}

func TestThresholdReady(t *testing.T) {
	assert.False(t, thresholdReady(10, 0))
	assert.False(t, thresholdReady(2, 3))
	assert.True(t, thresholdReady(3, 3))
}

func TestRepoHeadKey(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("/repo", repoHeadKey("/repo", ""))
	assert.Equal("/repo\x00main", repoHeadKey("/repo", "main"))
	assert.NotEqual(repoHeadKey("/repo", "main"), repoHeadKey("/repo", "feature"))
}

func TestRecordStopTracksReminderPromptCount(t *testing.T) {
	assert := assert.New(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")

	closed := false
	verdict := "F"
	failed := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jobs := []storage.ReviewJob{}
		if failed {
			jobs = append(jobs, storage.ReviewJob{Status: storage.JobStatusDone, Closed: &closed, Verdict: &verdict})
		}
		assert.NoError(json.NewEncoder(w).Encode(jobsResponse{Jobs: jobs}))
	}))
	t.Cleanup(server.Close)

	store := &StateStore{
		path:     filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{},
	}
	req := Request{
		Event:             Input{SessionID: "session-1", CWD: repo.Path(), HookEventName: "Stop"},
		Threshold:         1,
		Instruction:       "Run roborev fix.",
		RoborevServerAddr: server.URL,
	}

	first, err := store.Record(req)
	require.NoError(t, err)
	assert.True(first.Triggered)
	assert.Equal(1, first.ReminderPromptCount)
	assert.Equal(1, store.sessions["session-1"].ReminderPromptCount)

	second, err := store.Record(req)
	require.NoError(t, err)
	assert.True(second.Triggered)
	assert.Equal(2, second.ReminderPromptCount)

	active := req
	active.Event.StopHookActive = true
	skip, err := store.Record(active)
	require.NoError(t, err)
	assert.True(skip.Skipped)
	assert.Equal(2, skip.ReminderPromptCount)

	failed = false
	quiet, err := store.Record(req)
	require.NoError(t, err)
	assert.False(quiet.Triggered)
	assert.Equal(2, quiet.ReminderPromptCount)
}

func TestRecordStopQueriesMainRepoRootFromWorktree(t *testing.T) {
	assert := assert.New(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")

	worktree := filepath.Join(t.TempDir(), "wt")
	repo.RunGit("worktree", "add", "-b", "feature", worktree)

	var gotRepo string
	closed := false
	verdict := "F"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRepo = r.URL.Query().Get("repo")
		assert.NoError(json.NewEncoder(w).Encode(jobsResponse{
			Jobs: []storage.ReviewJob{
				{Status: storage.JobStatusDone, Closed: &closed, Verdict: &verdict},
			},
		}))
	}))
	t.Cleanup(server.Close)

	store := &StateStore{
		path:     filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{},
	}
	resp, err := store.Record(Request{
		Event: Input{
			SessionID:     "session-1",
			CWD:           worktree,
			HookEventName: "Stop",
		},
		Threshold:             5,
		FailedReviewThreshold: 1,
		Instruction:           "Run roborev fix.",
		RoborevServerAddr:     server.URL,
	})

	require.NoError(t, err)
	assert.True(resp.Triggered)
	assert.Equal("failed_reviews", resp.TriggeredBy)

	// The daemon stores jobs under the main repo root, so a worktree session
	// must query the main root rather than its own checkout path.
	wantMain, err := filepath.EvalSymlinks(repo.Path())
	require.NoError(t, err)
	gotMain, err := filepath.EvalSymlinks(gotRepo)
	require.NoError(t, err)
	assert.Equal(wantMain, gotMain, "worktree session should query the main repo root")
	assert.NotEqual(worktree, gotRepo, "query must not use the worktree checkout path")
}

func TestRecordStopTriggersFailedReviewWithoutRepoConfig(t *testing.T) {
	assert := assert.New(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")

	closed := false
	verdict := "F"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/jobs", r.URL.Path)
		assert.Equal(repo.Path(), r.URL.Query().Get("repo"))
		assert.Equal("main", r.URL.Query().Get("branch"))
		assert.Equal("false", r.URL.Query().Get("closed"))
		assert.Equal("done", r.URL.Query().Get("status"))
		assert.NoError(json.NewEncoder(w).Encode(jobsResponse{
			Jobs: []storage.ReviewJob{
				{Status: storage.JobStatusDone, Closed: &closed, Verdict: &verdict},
			},
		}))
	}))
	t.Cleanup(server.Close)

	store := &StateStore{
		path:     filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{},
	}
	resp, err := store.Record(Request{
		Event: Input{
			SessionID:     "session-1",
			CWD:           repo.Path(),
			HookEventName: "Stop",
		},
		Threshold:             5,
		FailedReviewThreshold: 1,
		Instruction:           "Run roborev fix.",
		RoborevServerAddr:     server.URL,
	})

	require.NoError(t, err)
	assert.False(resp.Skipped)
	assert.True(resp.Triggered)
	assert.Equal("failed_reviews", resp.TriggeredBy)
	assert.Equal(1, resp.FailedReviewCount)
}

func TestRecordStopTriggersFailedReviewOnDetachedHead(t *testing.T) {
	assert := assert.New(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")
	repo.RunGit("checkout", "--detach")

	closed := false
	verdict := "F"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal("/api/jobs", r.URL.Path)
		assert.Equal(repo.Path(), r.URL.Query().Get("repo"))
		assert.Empty(r.URL.Query().Get("branch"))
		assert.Empty(r.URL.Query().Get("branch_include_empty"))
		assert.Equal("false", r.URL.Query().Get("closed"))
		assert.Equal("done", r.URL.Query().Get("status"))
		assert.NoError(json.NewEncoder(w).Encode(jobsResponse{
			Jobs: []storage.ReviewJob{
				{Status: storage.JobStatusDone, Closed: &closed, Verdict: &verdict},
			},
		}))
	}))
	t.Cleanup(server.Close)

	store := &StateStore{
		path:     filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{},
	}
	resp, err := store.Record(Request{
		Event: Input{
			SessionID:     "session-1",
			CWD:           repo.Path(),
			HookEventName: "Stop",
		},
		Threshold:             5,
		FailedReviewThreshold: 1,
		Instruction:           "Run roborev fix.",
		RoborevServerAddr:     server.URL,
	})

	require.NoError(t, err)
	assert.False(resp.Skipped)
	assert.True(resp.Triggered)
	assert.Equal("failed_reviews", resp.TriggeredBy)
	assert.Equal(1, resp.FailedReviewCount)
	assert.Equal(1, requests)
}

func TestRecordPostToolUseFirstCommitWithoutBaselineDoesNotCount(t *testing.T) {
	assert := assert.New(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")

	closed := false
	verdict := "F"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(json.NewEncoder(w).Encode(jobsResponse{
			Jobs: []storage.ReviewJob{
				{Status: storage.JobStatusDone, Closed: &closed, Verdict: &verdict},
			},
		}))
	}))
	t.Cleanup(server.Close)

	store := &StateStore{
		path:     filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{},
	}
	// The first observed command runs a commit that failed: HEAD is unchanged,
	// but the repo's existing commit makes the latest reflog entry look like a
	// commit. Without a recorded HEAD baseline this must not count, so it must
	// not trip the commit threshold even with an actionable failed review.
	resp, err := store.Record(Request{
		Event: Input{
			SessionID:     "session-1",
			CWD:           repo.Path(),
			HookEventName: "PostToolUse",
			ToolName:      "Bash",
			ToolInput:     map[string]json.RawMessage{"command": json.RawMessage(`"git commit -m test"`)},
		},
		CommitThreshold:       1,
		FailedReviewThreshold: 0,
		Instruction:           "Run roborev fix.",
		RoborevServerAddr:     server.URL,
	})

	require.NoError(t, err)
	assert.False(resp.Triggered, "a failed first commit must not trigger a prompt")
	assert.Equal(0, store.sessions["session-1"].CommitCount)
	assert.Equal(0, store.sessions["session-1"].CommitCountSincePrompt)
}

func TestRecordPreToolUseBaselineLetsFirstCommitCount(t *testing.T) {
	assert := assert.New(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(json.NewEncoder(w).Encode(jobsResponse{Jobs: []storage.ReviewJob{}}))
	}))
	t.Cleanup(server.Close)

	store := &StateStore{
		path:     filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{},
	}
	req := Request{
		Event: Input{
			SessionID:     "session-1",
			CWD:           repo.Path(),
			HookEventName: "PreToolUse",
			ToolName:      "Bash",
			ToolInput:     map[string]json.RawMessage{"command": json.RawMessage(`"git commit -m second"`)},
		},
		CommitThreshold:   5,
		Instruction:       "Run roborev fix.",
		RoborevServerAddr: server.URL,
	}

	pre, err := store.Record(req)
	require.NoError(t, err)
	assert.False(pre.Triggered)
	assert.Equal(0, store.sessions["session-1"].CommitCount)

	repo.CommitFile("feature.go", "package main\n", "second")
	post := req
	post.Event.HookEventName = "PostToolUse"
	_, err = store.Record(post)

	require.NoError(t, err)
	assert.Equal(1, store.sessions["session-1"].CommitCount)
	assert.Equal(1, store.sessions["session-1"].CommitCountSincePrompt)
}

func TestRecordPostToolUseCountsCommitAfterBaseline(t *testing.T) {
	assert := assert.New(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(json.NewEncoder(w).Encode(jobsResponse{Jobs: []storage.ReviewJob{}}))
	}))
	t.Cleanup(server.Close)

	store := &StateStore{
		path:     filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{},
	}
	base := Request{
		Event: Input{
			SessionID:     "session-1",
			CWD:           repo.Path(),
			HookEventName: "PostToolUse",
			ToolName:      "Bash",
			ToolInput:     map[string]json.RawMessage{"command": json.RawMessage(`"git status"`)},
		},
		CommitThreshold:   5,
		Instruction:       "Run roborev fix.",
		RoborevServerAddr: server.URL,
	}

	// First observation establishes the HEAD baseline without counting.
	_, err := store.Record(base)
	require.NoError(t, err)
	assert.Equal(0, store.sessions["session-1"].CommitCount)

	// A real commit moves HEAD; the next commit command counts it.
	repo.CommitFile("feature.go", "package main\n", "second")
	commit := base
	commit.Event.ToolInput = map[string]json.RawMessage{"command": json.RawMessage(`"git commit -m second"`)}
	_, err = store.Record(commit)

	require.NoError(t, err)
	assert.Equal(1, store.sessions["session-1"].CommitCount)
}
