package agenthook

import (
	"context"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitagenthook "go.kenn.io/kit/agenthook"
	"hegel.dev/go/hegel"

	"go.kenn.io/roborev/internal/agentguide"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

const testRepoFlow = `digraph roborev_sample {
 node [shape=box];
 start [shape=oval label="Repository findings"];
 back [shape=oval label="Back to roborev_triage"];
 start -> back;
}`

type reviewFlowFunc func(context.Context, string) config.AgentHookReviewFlow

func (f reviewFlowFunc) ReviewFlow(ctx context.Context, repo string) config.AgentHookReviewFlow {
	return f(ctx, repo)
}

func TestWithReviewFlow(t *testing.T) {
	flow := config.AgentHookReviewFlow{Text: testRepoFlow, Ref: "origin/main"}
	assert.Equal(t, "R", withReviewFlow("R", config.AgentHookReviewFlow{}))
	got := withReviewFlow("R", flow)
	assert.Equal(t, "R\n\nRepository review flow, from .roborev.toml on \"origin/main\". It refines the Which disposition step above for this repository:\n\n"+agentguide.FenceDOT(testRepoFlow), got)
	flow.Ref = ""
	assert.Contains(t, withReviewFlow("R", flow), "from .roborev.toml in the working tree.")
	flow.Ref = "ma\"in\nX"
	assert.Contains(t, withReviewFlow("R", flow), "on \"mainX\". It refines")
}

func TestReviewReminderAppendsRepoReviewFlow(t *testing.T) {
	for _, event := range []string{"Stop", "UserPromptSubmit", "PostToolUse", "commit"} {
		t.Run(event, func(t *testing.T) {
			repo := testutil.NewGitRepo(t)
			repo.CommitFile("main.go", "package main\n", "initial")
			count := 1
			store := &StateStore{reviews: newDeferredReminderSource(repo.Path(), &count), path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
			calls := 0
			store.flows = reviewFlowFunc(func(_ context.Context, path string) config.AgentHookReviewFlow {
				calls++
				require.Equal(t, repo.Path(), path)
				locked := store.mu.TryLock()
				require.True(t, locked, "flow loading must run outside the state lock")
				store.mu.Unlock()
				return config.AgentHookReviewFlow{Text: testRepoFlow, Ref: "origin/main"}
			})
			req := Request{Agent: kitagenthook.AgentClaude, Event: Input{SessionID: "session-1", CWD: repo.Path(), HookEventName: event, ToolName: "Bash", ToolInput: map[string]jsontext.Value{"command": jsontext.Value(`"git status"`)}}, Threshold: 1, FailedReviewThreshold: 1}
			if event == "commit" {
				req.Event.HookEventName = "PostToolUse"
				req.FailedReviewThreshold = 0
				req.CommitThreshold = 1
				_, err := store.Record(req)
				require.NoError(t, err)
				repo.CommitFile("feature.go", "package main\n", "feature")
				req.Event.ToolInput = map[string]jsontext.Value{"command": jsontext.Value(`"git commit -m feature"`)}
			}
			resp, err := store.Record(req)
			require.NoError(t, err)
			require.True(t, resp.Triggered)
			require.NotNil(t, resp.FixSessionID)
			assert.Equal(t, 1, calls)
			assert.True(t, strings.HasSuffix(resp.Reason, agentguide.FenceDOT(testRepoFlow)))
			assert.Less(t, strings.Index(resp.Reason, "job"), strings.Index(resp.Reason, "Repository review flow"))
			continuationReq := req
			continuationReq.Event.HookEventName = "Stop"
			continuationReq.Event.ToolName = ""
			continuationReq.Event.ToolInput = nil
			continuation, err := store.Record(continuationReq)
			require.NoError(t, err)
			require.True(t, continuation.Triggered)
			assert.Equal(t, "fix_session", continuation.TriggeredBy)
			assert.NotContains(t, continuation.Reason, "Repository review flow")
			assert.Equal(t, 1, calls, "continuations do not load flows")
		})
	}
}

func TestDeferredReminderUsesPendingRepoReviewFlow(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")
	other := testutil.NewGitRepo(t)
	other.CommitFile("main.go", "package main\n", "initial")
	reviews := fakeReviewSource{
		resolve: func(_ context.Context, path, _ string) (TrackedRepoResolution, bool) {
			return TrackedRepoResolution{Tracked: true, RootPath: path, Name: filepath.Base(path)}, true
		},
		list: func(_ context.Context, repoRoot, _ string) ([]storage.ReviewJob, bool) {
			if repoRoot != repo.Path() {
				return nil, true
			}
			return []storage.ReviewJob{failedReviewJob(1)}, true
		},
	}
	store := &StateStore{reviews: reviews, path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
	calls := 0
	store.flows = reviewFlowFunc(func(_ context.Context, path string) config.AgentHookReviewFlow {
		calls++
		assert.Equal(t, repo.Path(), path)
		return config.AgentHookReviewFlow{Text: testRepoFlow}
	})
	_, err := store.Record(Request{Event: Input{SessionID: "session-1", CWD: repo.Path(), HookEventName: "PostToolUse", ToolName: "Bash", ToolInput: map[string]jsontext.Value{"command": jsontext.Value(`"git status"`)}}, FailedReviewThreshold: 1, DeferPostToolReminder: true})
	require.NoError(t, err)
	assert.Zero(t, calls)
	resp, err := store.Record(Request{Event: Input{SessionID: "session-1", CWD: other.Path(), HookEventName: "Stop"}})
	require.NoError(t, err)
	require.True(t, resp.Triggered)
	assert.True(t, strings.HasSuffix(resp.Reason, agentguide.FenceDOT(testRepoFlow)))
	assert.Less(t, strings.Index(resp.Reason, "The triggering worktree is"), strings.Index(resp.Reason, "Repository review flow"))
	assert.Equal(t, 1, calls)
}

func TestCustomInstructionDoesNotLoadRepoReviewFlow(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")
	count := 1
	store := &StateStore{reviews: newDeferredReminderSource(repo.Path(), &count), path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
	store.flows = reviewFlowFunc(func(_ context.Context, path string) config.AgentHookReviewFlow {
		assert.Empty(t, path, "custom instruction loaded repository flow")
		return config.AgentHookReviewFlow{}
	})
	resp, err := store.Record(Request{Event: Input{SessionID: "session-1", CWD: repo.Path(), HookEventName: "Stop"}, Threshold: 1, Instruction: "Resolve open roborev findings now."})
	require.NoError(t, err)
	require.True(t, resp.Triggered)
	assert.NotContains(t, resp.Reason, "Repository review flow")
}

func TestEmptyReviewFlowPreservesReasonProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		reason := hegel.Draw(ht, hegel.Text())
		assert.Equal(ht, reason, withReviewFlow(reason, config.AgentHookReviewFlow{}))
	})
}

func TestInvalidRepoReviewFlowPreservesTriggeredReminder(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")
	repo.WriteFile(".roborev.toml", "[agent_hook]\nreview_flow = 'not dot'\n")
	count := 1
	store := &StateStore{reviews: newDeferredReminderSource(repo.Path(), &count), path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
	resp, err := store.Record(Request{Agent: kitagenthook.AgentClaude, Event: Input{SessionID: "session-1", CWD: repo.Path(), HookEventName: "Stop"}, Threshold: 1})
	require.NoError(t, err)
	require.True(t, resp.Triggered)
	require.NotNil(t, resp.FixSessionID)
	assert.Equal(t, buildStopReason(Request{}, 1, reviewIDSet{1: {}}), resp.Reason)
	assert.Equal(t, 1, store.sessions["session-1"].ReminderPromptCount)
	data, err := os.ReadFile(store.path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "acknowledged_review")
}
