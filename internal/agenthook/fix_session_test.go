package agenthook

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

var (
	firstFixSessionID  = uuid.MustParse("00000000-0000-4000-8000-000000000001")
	secondFixSessionID = uuid.MustParse("00000000-0000-4000-8000-000000000002")
)

func TestCompleteFixSessionRemovesOwnership(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := &StateStore{
		path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{},
		fixSessions: map[string]FixSession{"worktree": {
			ID: firstFixSessionID, ExpiresAt: now.Add(FixSessionLifetime),
		}},
		now: func() time.Time { return now },
	}

	err := store.CompleteFixSession(firstFixSessionID)
	require.NoError(t, err)
	assert.Empty(t, store.fixSessions)

	err = store.CompleteFixSession(secondFixSessionID)
	require.NoError(t, err)
}

func TestPrepareFixSessionGrantPrunesExpiredOwnership(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := &StateStore{fixSessions: map[string]FixSession{
		"expired": {ID: firstFixSessionID, ExpiresAt: now},
		"active":  {ID: secondFixSessionID, ExpiresAt: now.Add(time.Hour)},
	}}

	fixSessions, fixSession, granted := store.prepareFixSessionGrantLocked(
		Request{Agent: "claude", Event: Input{SessionID: "session-1"}},
		"new", now,
	)

	assert.True(t, granted)
	require.NotNil(t, fixSession)
	assert.NotContains(t, fixSessions, "expired")
	assert.Contains(t, fixSessions, "active")
	assert.Contains(t, fixSessions, "new")
}

func TestRecordStopFixSessionAllowsOneConcurrentOwner(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	reviews, release := synchronizedFailedReviewSource(repo.Path(), 2)
	store := &StateStore{
		reviews: reviews, path: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{}, fixSessions: map[string]FixSession{},
		now: func() time.Time { return now },
	}
	requests := []Request{
		{
			Agent: "claude", Event: Input{SessionID: "session-a", CWD: repo.Path(), HookEventName: "Stop"},
			Threshold: 1, Instruction: "Resolve reviews.",
		},
		{
			Agent: "codex", Event: Input{SessionID: "session-b", CWD: repo.Path(), HookEventName: "Stop"},
			Threshold: 1, Instruction: "Resolve reviews.",
		},
	}

	responses, errs := concurrentRecords(store, requests, release)
	for _, err := range errs {
		require.NoError(t, err)
	}
	triggered := triggeredResponses(responses)
	require.Len(t, triggered, 1)
	assert.NotNil(t, triggered[0].FixSessionID)
	assert.Len(t, store.fixSessions, 1)

	owner := store.fixSessions[worktreeSequenceKey(repo.Path(), repo.Path())]
	assert.Equal(t, now.Add(FixSessionLifetime), owner.ExpiresAt)
	ownerRequest := requests[0]
	if owner.SessionID == requests[1].Event.SessionID {
		ownerRequest = requests[1]
	}
	now = now.Add(11 * time.Hour)
	closeout, err := store.Record(ownerRequest)
	require.NoError(t, err)
	require.NotNil(t, closeout.FixSessionID)
	assert.Equal(t, "fix_session", closeout.TriggeredBy)
	assert.Equal(t, owner.ID, *closeout.FixSessionID)
	assert.Equal(t, owner.ExpiresAt, store.fixSessions[worktreeSequenceKey(repo.Path(), repo.Path())].ExpiresAt)

	recursive := ownerRequest
	recursive.Event.StopHookActive = true
	skipped, err := store.Record(recursive)
	require.NoError(t, err)
	assert.True(t, skipped.Skipped)
	assert.False(t, skipped.Triggered)

	err = store.CompleteFixSession(owner.ID)
	require.NoError(t, err)
	blocked := requests[0]
	if triggered[0].SessionID == blocked.Event.SessionID {
		blocked = requests[1]
	}
	retry, err := store.Record(blocked)
	require.NoError(t, err)
	assert.True(t, retry.Triggered, "the blocked session must keep its reminder progress")
	assert.NotNil(t, retry.FixSessionID)
}

func TestRecordPostToolUseFixSessionAllowsOneConcurrentOwner(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("main.go", "package main\n", "initial")
	reviews, release := synchronizedFailedReviewSource(repo.Path(), 2)
	lineage := repoHeadKey(repo.Path(), "main")
	worktreeKey := worktreeSequenceKey(repo.Path(), repo.Path())
	seed := func() SessionState {
		return SessionState{
			CommitCountsSincePrompt: map[string]int{lineage: 1},
			RepoHeads:               map[string]string{lineage: head, worktreeKey: head},
			WorktreeLineageKeys:     map[string]string{worktreeKey: lineage},
		}
	}
	store := &StateStore{
		reviews: reviews, path: filepath.Join(t.TempDir(), "state.json"),
		sessions:    map[string]SessionState{"session-a": seed(), "session-b": seed()},
		fixSessions: map[string]FixSession{},
	}
	requests := []Request{
		{
			Agent: "claude", Event: Input{
				SessionID: "session-a", CWD: repo.Path(), HookEventName: "PostToolUse", ToolName: "Bash",
				ToolInput: map[string]json.RawMessage{"command": json.RawMessage(`"go test ./..."`)},
			},
			CommitThreshold: 1, Instruction: "Resolve reviews.",
		},
		{
			Agent: "codex", Event: Input{
				SessionID: "session-b", CWD: repo.Path(), HookEventName: "PostToolUse", ToolName: "Bash",
				ToolInput: map[string]json.RawMessage{"command": json.RawMessage(`"go test ./..."`)},
			},
			CommitThreshold: 1, Instruction: "Resolve reviews.",
		},
	}

	responses, errs := concurrentRecords(store, requests, release)
	for _, err := range errs {
		require.NoError(t, err)
	}
	triggered := triggeredResponses(responses)
	require.Len(t, triggered, 1)
	assert.Equal(t, "commit", triggered[0].TriggeredBy)
	assert.NotNil(t, triggered[0].FixSessionID)
	assert.Len(t, store.fixSessions, 1)
}

func TestRecordFixSessionsDoNotCrossWorktrees(t *testing.T) {
	repoA := testutil.NewGitRepo(t)
	repoA.CommitFile("a.go", "package main\n", "initial")
	repoB := testutil.NewGitRepo(t)
	repoB.CommitFile("b.go", "package main\n", "initial")
	closed := false
	verdict := "F"
	reviews := fakeReviewSource{
		resolve: func(_ context.Context, path, _ string) (TrackedRepoResolution, bool) {
			return TrackedRepoResolution{Tracked: true, RootPath: path}, true
		},
		list: func(context.Context, string, string) ([]storage.ReviewJob, bool) {
			return []storage.ReviewJob{{
				ID: 1, Status: storage.JobStatusDone, Closed: &closed, Verdict: &verdict, Branch: "main",
			}}, true
		},
	}
	store := &StateStore{
		reviews: reviews, path: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{}, fixSessions: map[string]FixSession{},
	}

	for i, repo := range []*testutil.TestRepo{repoA, repoB} {
		resp, err := store.Record(Request{
			Agent: "claude", Event: Input{
				SessionID: fmt.Sprintf("session-%d", i), CWD: repo.Path(), HookEventName: "Stop",
			},
			Threshold: 1, Instruction: "Resolve reviews.",
		})
		require.NoError(t, err)
		assert.True(t, resp.Triggered)
		assert.NotNil(t, resp.FixSessionID)
	}
	assert.Len(t, store.fixSessions, 2)
}

func TestRecordWorktreeOwnerSurvivesReloadAndBranchTransitionUntilExpiry(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewGitRepo(t)
	firstHead := repo.CommitFile("main.go", "package main\n", "initial")
	repo.CheckoutDetached(firstHead)
	failedReview := failedReviewJob(1)
	failedReview.Branch = "feature"
	failedReview.GitRef = firstHead
	reviews := trackedReviewSource(repo.Path(), failedReview)
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := &StateStore{
		reviews: reviews, path: StatePath(), sessions: map[string]SessionState{},
		fixSessions: map[string]FixSession{},
		now:         func() time.Time { return now },
	}

	first, err := store.Record(Request{
		Agent: "claude", Event: Input{
			SessionID: "session-a", CWD: repo.Path(), HookEventName: "Stop",
		},
		Threshold: 1, Instruction: "Resolve reviews.",
	})
	require.NoError(t, err)
	require.True(t, first.Triggered)
	require.NotNil(t, first.FixSessionID)
	reloaded, err := LoadState(reviews)
	require.NoError(t, err)
	reloaded.now = func() time.Time { return now }
	repo.CheckoutNewBranch("feature", firstHead)

	secondRequest := Request{
		Agent: "codex", Event: Input{
			SessionID: "session-b", CWD: repo.Path(), HookEventName: "Stop",
		},
		Threshold: 1, Instruction: "Resolve reviews.",
	}
	second, err := reloaded.Record(secondRequest)

	require.NoError(t, err)
	assert.False(t, second.Triggered)
	assert.Len(t, reloaded.fixSessions, 1)

	now = now.Add(FixSessionLifetime)
	replacement, err := reloaded.Record(secondRequest)
	require.NoError(t, err)
	require.NotNil(t, replacement.FixSessionID)
	assert.True(t, replacement.Triggered)
	assert.NotEqual(t, *first.FixSessionID, *replacement.FixSessionID)
}

func TestRecordCursorTriggerDoesNotAcquireFixSession(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")
	store := &StateStore{
		reviews: trackedReviewSource(repo.Path(), failedReviewJob(1)),
		path:    filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{},
		fixSessions: map[string]FixSession{},
	}

	resp, err := store.Record(Request{
		Agent: "cursor", Event: Input{SessionID: "session-a", CWD: repo.Path(), HookEventName: "Stop"},
		Threshold: 1, Instruction: "Resolve reviews.",
	})

	require.NoError(t, err)
	assert.True(t, resp.Triggered)
	assert.Nil(t, resp.FixSessionID)
	assert.Empty(t, store.fixSessions)
}

func TestDeferredReminderAcquiresFixSessionOnlyAtStop(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("main.go", "package main\n", "initial")
	lineage := repoHeadKey(repo.Path(), "main")
	worktreeKey := worktreeSequenceKey(repo.Path(), repo.Path())
	failedReviewCount := 1
	store := &StateStore{
		reviews: newDeferredReminderSource(repo.Path(), &failedReviewCount),
		path:    filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{"session-a": {
			CommitCountsSincePrompt: map[string]int{lineage: 1},
			RepoHeads:               map[string]string{lineage: head, worktreeKey: head},
			WorktreeLineageKeys:     map[string]string{worktreeKey: lineage},
		}},
		fixSessions: map[string]FixSession{},
	}
	post, err := store.Record(Request{
		Agent: "hermes", Event: Input{
			SessionID: "session-a", CWD: repo.Path(), HookEventName: "PostToolUse", ToolName: "Bash",
			ToolInput: map[string]json.RawMessage{"command": json.RawMessage(`"go test ./..."`)},
		},
		CommitThreshold: 1, Instruction: "Resolve reviews.", DeferPostToolReminder: true,
	})
	require.NoError(t, err)
	assert.False(t, post.Triggered)
	assert.Empty(t, store.fixSessions)

	stop, err := store.Record(Request{
		Agent: "hermes", Event: Input{SessionID: "session-a", CWD: t.TempDir(), HookEventName: "Stop"},
	})
	require.NoError(t, err)
	assert.True(t, stop.Triggered)
	assert.NotNil(t, stop.FixSessionID)
	assert.Len(t, store.fixSessions, 1)
}

func TestRecordTriggerPersistenceFailureDoesNotGrantFixSession(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")
	store := &StateStore{
		reviews: trackedReviewSource(repo.Path(), failedReviewJob(1)),
		path:    t.TempDir(), sessions: map[string]SessionState{}, fixSessions: map[string]FixSession{},
	}

	resp, err := store.Record(Request{
		Agent: "claude", Event: Input{SessionID: "session-a", CWD: repo.Path(), HookEventName: "Stop"},
		Threshold: 1, Instruction: "Resolve reviews.",
	})

	require.Error(t, err)
	assert.False(t, resp.Triggered)
	assert.Empty(t, store.fixSessions)
}

func synchronizedFailedReviewSource(repoPath string, blockedCalls int) (ReviewSource, func()) {
	arrived := make(chan struct{}, blockedCalls)
	release := make(chan struct{})
	var calls atomic.Int32
	closed := false
	verdict := "F"
	source := fakeReviewSource{
		resolve: func(context.Context, string, string) (TrackedRepoResolution, bool) {
			return TrackedRepoResolution{Tracked: true, RootPath: repoPath}, true
		},
		list: func(context.Context, string, string) ([]storage.ReviewJob, bool) {
			if calls.Add(1) <= int32(blockedCalls) {
				arrived <- struct{}{}
				<-release
			}
			return []storage.ReviewJob{{
				ID: 1, Status: storage.JobStatusDone, Closed: &closed, Verdict: &verdict, Branch: "main",
			}}, true
		},
	}
	return source, func() {
		for range blockedCalls {
			<-arrived
		}
		close(release)
	}
}

func concurrentRecords(
	store *StateStore,
	requests []Request,
	release func(),
) ([]Response, []error) {
	type result struct {
		index    int
		response Response
		err      error
	}
	results := make(chan result, len(requests))
	for i, request := range requests {
		go func() {
			response, err := store.Record(request)
			results <- result{index: i, response: response, err: err}
		}()
	}
	release()
	responses := make([]Response, len(requests))
	errs := make([]error, len(requests))
	for range requests {
		result := <-results
		responses[result.index] = result.response
		errs[result.index] = result.err
	}
	return responses, errs
}

func triggeredResponses(responses []Response) []Response {
	triggered := make([]Response, 0, len(responses))
	for _, response := range responses {
		if response.Triggered {
			triggered = append(triggered, response)
		}
	}
	return triggered
}
