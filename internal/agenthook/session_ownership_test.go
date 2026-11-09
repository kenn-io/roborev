package agenthook

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"

	"go.kenn.io/roborev/internal/storage"
)

func TestSessionBacklogAndDelayedReviewAfterReload(t *testing.T) {
	for _, toolID := range []string{"created", ""} {
		t.Run("tool ID "+toolID, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			repo := gittest.NewRepo(t, gittest.Options{InitArgs: []string{"init", "-b", "main"}, ConfigureUser: true, ResolvePath: true})
			repo.CommitFile("base.txt", "base", "base")
			repo.Run("commit", "--allow-empty", "-m", "old")
			old := strings.TrimSpace(repo.Run("rev-parse", "HEAD"))
			jobs := []storage.ReviewJob{}
			for id := int64(1); id <= 4; id++ {
				job := failedReviewJob(id, old)
				jobs = append(jobs, job)
			}
			reviews := fakeReviewSource{list: func(context.Context, string, string) ([]storage.ReviewJob, bool) { return jobs, true }}
			store, err := LoadState(reviews)
			require.NoError(t, err)
			req := Request{Event: Input{SessionID: "own", CWD: repo.Root, ToolName: "Bash"}, Threshold: 5}
			for _, event := range []string{"PostToolUse", "Stop", "UserPromptSubmit"} {
				quiet := req
				quiet.Event.HookEventName = event
				quiet.Agent = "zcode"
				quiet.FailedReviewThreshold = 1
				resp, err := store.RecordContext(t.Context(), quiet)
				require.NoError(t, err)
				assert.False(t, resp.Triggered)
			}
			req.Event.ToolUseID = toolID
			req.Event.ToolInput = map[string]jsontext.Value{"command": jsontext.Value(`"git commit --allow-empty -m own"`)}
			req.Event.HookEventName = "PreToolUse"
			_, err = store.RecordContext(t.Context(), req)
			require.NoError(t, err)
			repo.Run("commit", "--allow-empty", "-m", "own")
			owned := strings.TrimSpace(repo.Run("rev-parse", "HEAD"))
			job := failedReviewJob(99, owned)
			jobs = append(jobs, job)
			req.Event.HookEventName = "PostToolUse"
			resp, err := store.RecordContext(t.Context(), req)
			require.NoError(t, err)
			require.False(t, resp.Triggered)
			store, err = LoadState(reviews)
			require.NoError(t, err)
			req.Event.HookEventName = "Stop"
			req.Threshold = 1
			resp, err = store.RecordContext(t.Context(), req)
			require.NoError(t, err)
			require.True(t, resp.Triggered)
			assert.Contains(t, resp.Reason, "Review job IDs: 99.")

			// A delayed failure still belongs to the session after the reminder reset.
			job.ID = 100
			jobs = append(jobs, job)
			req.FailedReviewThreshold = 1
			req.Event.HookEventName = "UserPromptSubmit"
			resp, err = store.RecordContext(t.Context(), req)
			require.NoError(t, err)
			require.True(t, resp.Triggered)
			assert.Contains(t, resp.Reason, "job 100 (")
		})
	}
}

func runSessionCommand(t *testing.T, store *StateStore, req Request, run func()) Response {
	t.Helper()
	req.Event.HookEventName = "PreToolUse"
	_, err := store.RecordContext(t.Context(), req)
	require.NoError(t, err)
	run()
	req.Event.HookEventName = "PostToolUse"
	got, err := store.RecordContext(t.Context(), req)
	require.NoError(t, err)
	return got
}

func TestSessionOwnershipWorkflows(t *testing.T) {
	for _, name := range []string{"cherry-pick", "rebase", "chained amend", "aborted cherry-pick"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := gittest.NewRepoWithCommit(t)
			base := repo.Head()
			store := &StateStore{reviews: trackedReviewSource(repo.Root), path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
			req := Request{Event: Input{SessionID: "own", CWD: repo.Root, ToolUseID: "commit", ToolName: "Bash", ToolInput: map[string]jsontext.Value{}}, FailedReviewThreshold: 1}
			command := ""
			var ownedRef, reviewRef, restored, pick string
			switch name {
			case "cherry-pick":
				repo.Run("checkout", "-b", "source")
				pick := repo.CommitFile("pick.txt", "pick", "pick")
				repo.Run("checkout", "main")
				command = "git cherry-pick " + pick
			case "chained amend":
				req.Event.ToolInput["command"] = jsontext.Value(`"git commit -m own"`)
				runSessionCommand(t, store, req, func() { repo.CommitFile("own.txt", "own", "own") })
				command = "git commit --amend -m amended && git commit -m next"
				req.Threshold, req.FailedReviewThreshold = 1, 0
			case "aborted cherry-pick":
				restored = repo.CommitFile("conflict.txt", "ours", "pre-session")
				repo.Run("checkout", "-b", "source", base)
				pick = repo.CommitFile("conflict.txt", "theirs", "pick")
				repo.Run("checkout", "main")
				repo.CommitFile("tip.txt", "tip", "later pre-session")
				command = "git reset --hard " + restored + " && git cherry-pick " + pick + " || git cherry-pick --abort"
			case "rebase":
				req.Event.ToolInput["command"] = jsontext.Value(`"git commit -m own"`)
				runSessionCommand(t, store, req, func() { ownedRef = repo.CommitFile("own.txt", "own", "own") })
				repo.Run("checkout", "-b", "upstream", base)
				repo.CommitFile("upstream.txt", "upstream", "upstream")
				repo.Run("checkout", "main")
				command = "git rebase upstream"
			}
			raw, err := json.Marshal(command)
			require.NoError(t, err)
			req.Event.ToolInput["command"] = raw
			got := runSessionCommand(t, store, req, func() {
				switch name {
				case "cherry-pick":
					repo.Run("cherry-pick", strings.TrimPrefix(command, "git cherry-pick "))
				case "chained amend":
					repo.Run("commit", "--amend", "--allow-empty", "-m", "amended")
					reviewRef = repo.Head()
					repo.CommitFile("next.txt", "next", "next")
				case "aborted cherry-pick":
					repo.Run("reset", "--hard", restored)
					_, _, err := repo.Runner.Run(t.Context(), repo.Root, nil, "cherry-pick", pick)
					require.Error(t, err, "the pick conflicts with the restored pre-session commit")
					repo.Run("cherry-pick", "--abort")
					require.Equal(t, restored, repo.Head())
				case "rebase":
					repo.Run("rebase", "upstream")
				}
				ref := repo.Head()
				if reviewRef != "" {
					ref = reviewRef
				}
				job := failedReviewJob(42, ref)
				store.reviews = trackedReviewSource(repo.Root, job)
			})
			if name == "chained amend" {
				req.Event.HookEventName = "Stop"
				got, err = store.RecordContext(t.Context(), req)
				require.NoError(t, err)
			}
			switch name {
			case "aborted cherry-pick":
				require.False(t, got.Triggered, "an aborted pick must not claim the restored pre-session commit")
			case "rebase":
				require.False(t, got.Triggered, "the rewritten SHA was not gained by a commit-producing command")
				store.reviews = trackedReviewSource(repo.Root, failedReviewJob(43, ownedRef))
				req.Event.HookEventName = "Stop"
				got, err = store.RecordContext(t.Context(), req)
				require.NoError(t, err)
				require.True(t, got.Triggered, "the original owned SHA still has an actionable review")
				require.Contains(t, got.Reason, "Review job IDs: 43.")
			default:
				require.True(t, got.Triggered)
				require.Contains(t, got.Reason, "Review job IDs: 42.")
			}
		})
	}
}

func TestSessionCommitFollowedByFailedPushStillReminds(t *testing.T) {
	repo := gittest.NewRepoWithCommit(t)
	store := &StateStore{reviews: trackedReviewSource(repo.Root), path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
	req := Request{Event: Input{SessionID: "own", CWD: repo.Root, HookEventName: "PreToolUse", ToolName: "Bash", ToolInput: map[string]jsontext.Value{"command": jsontext.Value(`"git commit --allow-empty -m own && git push"`)}}, FailedReviewThreshold: 1}
	_, err := store.RecordContext(t.Context(), req)
	require.NoError(t, err)
	repo.Run("commit", "--allow-empty", "-m", "own")
	_, _, err = repo.Runner.Run(t.Context(), repo.Root, nil, "push")
	require.Error(t, err, "the fixture has no push remote")
	store.reviews = trackedReviewSource(repo.Root, failedReviewJob(42, repo.Head()))
	req.Event.HookEventName = "PostToolUseFailure"
	got, err := store.RecordContext(t.Context(), req)
	require.NoError(t, err)
	require.True(t, got.Triggered, "a failed push must retain the preceding commit's review")
	require.Contains(t, got.Reason, "Review job IDs: 42.")
}

func TestSessionOwnsCommitsHeadGains(t *testing.T) {
	repo := gittest.NewRepo(t, gittest.Options{InitArgs: []string{"init", "-b", "main"}, ConfigureUser: true, ResolvePath: true})
	store := &StateStore{reviews: trackedReviewSource(repo.Root), path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
	req := Request{Event: Input{SessionID: "session", CWD: repo.Root, HookEventName: "PreToolUse", ToolName: "Bash", ToolInput: map[string]jsontext.Value{"command": jsontext.Value(`"git commit -m own"`)}}, Threshold: 1}
	_, err := store.RecordContext(t.Context(), req)
	require.NoError(t, err)
	own := repo.CommitFile("own.txt", "own", "own")
	req.Event.HookEventName = "PostToolUse"
	_, err = store.RecordContext(t.Context(), req)
	require.NoError(t, err)
	store.reviews = trackedReviewSource(repo.Root, failedReviewJob(42, own))
	req.Event.HookEventName = "Stop"
	got, err := store.RecordContext(t.Context(), req)
	require.NoError(t, err)
	require.True(t, got.Triggered)
	require.Contains(t, got.Reason, "Review job IDs: 42.")
}

func TestSessionAbandonedCallBaselinesClearAtStop(t *testing.T) {
	for _, completeSecond := range []bool{false, true} {
		t.Run(fmt.Sprint(completeSecond), func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			repo := gittest.NewRepoWithCommit(t)
			reviews := trackedReviewSource(repo.Root)
			store, err := LoadState(reviews)
			require.NoError(t, err)
			req := Request{Event: Input{SessionID: "session", CWD: repo.Root, HookEventName: "PreToolUse", ToolUseID: "u1", ToolName: "Bash", ToolInput: map[string]jsontext.Value{"command": jsontext.Value(`"git commit -m a"`)}}}
			_, err = store.RecordContext(t.Context(), req)
			require.NoError(t, err)
			req.Event.ToolUseID = "u2"
			_, err = store.RecordContext(t.Context(), req)
			require.NoError(t, err)
			if completeSecond {
				req.Event.HookEventName = "PostToolUse"
				_, err = store.RecordContext(t.Context(), req)
				require.NoError(t, err)
				require.Contains(t, store.Sessions()["session"].RepoHeads, "\x00call\x00u1")
			}
			req.Event.HookEventName = "Stop"
			_, err = store.RecordContext(t.Context(), req)
			require.NoError(t, err)
			reloaded, err := LoadState(reviews)
			require.NoError(t, err)
			heads := reloaded.Sessions()["session"].RepoHeads
			for key := range heads {
				require.False(t, strings.HasPrefix(key, "\x00call\x00"), "abandoned call baseline survived Stop")
			}
			require.Equal(t, repo.Head(), heads[repoHeadKey(repo.Root, strings.TrimSpace(repo.Run("branch", "--show-current")))])
		})
	}
}

func TestSessionCommitWhileMutedRemindsAfterUnmuting(t *testing.T) {
	for _, reason := range []string{"snoozed", "missing guidelines"} {
		t.Run(reason, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			repo := gittest.NewRepoWithCommit(t)
			muted := true
			var jobs []storage.ReviewJob
			reviews := fakeReviewSource{
				resolve: func(_ context.Context, root, _ string) (TrackedRepoResolution, bool) {
					resolved := TrackedRepoResolution{Tracked: true, RootPath: root}
					if muted && reason == "snoozed" {
						resolved.SnoozedUntil = time.Now().Add(time.Hour)
					}
					resolved.ReviewGuidelinesMissing = muted && reason == "missing guidelines"
					return resolved, true
				},
				list: func(context.Context, string, string) ([]storage.ReviewJob, bool) { return jobs, true },
			}
			store, err := LoadState(reviews)
			require.NoError(t, err)
			req := Request{Event: Input{SessionID: "session", CWD: repo.Root, ToolUseID: "commit-1", ToolName: "Bash", ToolInput: map[string]jsontext.Value{"command": jsontext.Value(`"git commit -m own"`)}}, FailedReviewThreshold: 1}
			runSessionCommand(t, store, req, func() { repo.CommitFile("own.txt", "own", "own") })
			jobs = []storage.ReviewJob{failedReviewJob(42, repo.Head())}
			muted = false
			reloaded, err := LoadState(reviews)
			require.NoError(t, err)
			req.Event.HookEventName = "Stop"
			got, err := reloaded.RecordContext(t.Context(), req)
			require.NoError(t, err)
			require.True(t, got.Triggered, "the session's commit must remain eligible after reminders resume")
			require.Contains(t, got.Reason, "Review job IDs: 42.")
		})
	}
}

func TestSessionOverlappingCommitRetainsDelayedReview(t *testing.T) {
	repo := gittest.NewRepoWithCommit(t)
	store := &StateStore{reviews: trackedReviewSource(repo.Root), path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
	req := Request{Event: Input{SessionID: "session", CWD: repo.Root, HookEventName: "PreToolUse", ToolUseID: "outer", ToolName: "Bash", ToolInput: map[string]jsontext.Value{"command": jsontext.Value(`"git commit --amend -m amended && git commit -m next"`)}}, Threshold: 1}
	_, err := store.RecordContext(t.Context(), req)
	require.NoError(t, err)
	observer := req
	observer.Event.ToolUseID = "observer"
	observer.Event.ToolInput = map[string]jsontext.Value{"command": jsontext.Value(`"git status"`)}
	_, err = store.RecordContext(t.Context(), observer)
	require.NoError(t, err)
	repo.Run("commit", "--amend", "--allow-empty", "-m", "amended")
	amended := repo.Head()
	repo.CommitFile("next.txt", "next", "next")
	observer.Event.HookEventName = "PostToolUse"
	_, err = store.RecordContext(t.Context(), observer)
	require.NoError(t, err)
	req.Event.HookEventName = "PostToolUse"
	_, err = store.RecordContext(t.Context(), req)
	require.NoError(t, err)
	store.reviews = trackedReviewSource(repo.Root, failedReviewJob(42, amended))
	req.Event.HookEventName = "Stop"
	got, err := store.RecordContext(t.Context(), req)
	require.NoError(t, err)
	require.True(t, got.Triggered)
	require.Contains(t, got.Reason, "Review job IDs: 42.")
}
