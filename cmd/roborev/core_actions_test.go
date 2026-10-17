package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/prompt/analyze"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestCoreActionRefineCommit(t *testing.T) {
	for _, mode := range []string{"terminal", "skill", "no change", "commit failure", "planning failure", "planning setup failure", "follow-up warnings", "pending before fix", "pending after fix"} {
		t.Run(mode, func(t *testing.T) {
			resetCLIUse(t)
			cliOutputIsTerminal = func(io.Writer) bool { return true }
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			originalUnsafe := agent.AllowUnsafeAgents()
			originalDelay := postCommitWaitDelay
			postCommitWaitDelay = time.Millisecond
			t.Cleanup(func() {
				agent.SetAllowUnsafeAgents(originalUnsafe)
				postCommitWaitDelay = originalDelay
			})
			repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
			base := repo.HeadSHA()
			head := repo.CommitFile("next.go", "package main\n", "Add source")
			chdir(t, repo.Dir)
			job := storage.ReviewJob{ID: 7, GitRef: head, Status: storage.JobStatusDone}
			review := &storage.Review{ID: 7, JobID: 7, Job: &job, Output: "High: missing cancellation", VerdictBool: testutil.ReviewFixtureVerdict("High: missing cancellation")}
			var changes, failures atomic.Int32
			closeCalls := 0
			hooks := MockRefineHooks{OnReview: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
				if mode == "follow-up warnings" && changes.Load() == 1 && r.URL.Query().Get("job_id") == "9" {
					failures.Add(1)
					http.Error(w, "review unavailable", http.StatusInternalServerError)
					return true
				}
				if mode == "follow-up warnings" && changes.Load() >= 2 {
					writeJSON(w, &storage.Review{ID: 9, JobID: 9, Output: "No issues found.", VerdictBool: testutil.ReviewFixtureVerdict("No issues found.")})
				} else {
					writeJSON(w, review)
				}
				return true
			}}
			if mode == "follow-up warnings" {
				hooks.OnGetJobs = func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
					ref := r.URL.Query().Get("git_ref")
					if ref == "" || ref == head || strings.Contains(ref, "..") {
						return false
					}
					job := &storage.ReviewJob{ID: 9, GitRef: ref, Status: storage.JobStatusDone}
					state.mu.Lock()
					state.jobs[9] = job
					state.mu.Unlock()
					writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{*job}})
					return true
				}
				hooks.OnComment = func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
					failures.Add(1)
					http.Error(w, "comment unavailable", http.StatusInternalServerError)
					return true
				}
				hooks.OnReviewClose = func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
					closeCalls++
					if closeCalls > 3 {
						return false
					}
					failures.Add(1)
					http.Error(w, "close unavailable", http.StatusInternalServerError)
					return true
				}
				hooks.OnEnqueue = func(w http.ResponseWriter, _ *http.Request, state *mockRefineState) bool {
					job := &storage.ReviewJob{ID: 9, Status: storage.JobStatusDone}
					state.mu.Lock()
					state.jobs[9] = job
					state.mu.Unlock()
					writeJSON(w, job)
					return true
				}
			}

			if strings.HasPrefix(mode, "pending ") {
				var phase atomic.Int32
				hooks.OnReview = func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					switch r.URL.Query().Get("job_id") {
					case "8":
						phase.Store(1)
						http.Error(w, "pending review unavailable", http.StatusInternalServerError)
					case "9", "10":
						id, err := strconv.ParseInt(r.URL.Query().Get("job_id"), 10, 64)
						require.NoError(t, err)
						writeJSON(w, &storage.Review{JobID: id, Output: "No issues found.", VerdictBool: testutil.ReviewFixtureVerdict("No issues found.")})
					default:
						if mode == "pending after fix" && changes.Load() == 0 {
							writeJSON(w, review)
						} else {
							w.WriteHeader(http.StatusNotFound)
						}
					}
					return true
				}
				hooks.OnGetJobs = func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					var jobs []storage.ReviewJob
					if id := r.URL.Query().Get("id"); id != "" {
						parsed, err := strconv.ParseInt(id, 10, 64)
						require.NoError(t, err)
						jobs = []storage.ReviewJob{{ID: parsed, GitRef: head, Status: storage.JobStatusDone}}
					} else if r.URL.Query().Get("git_ref") == head && phase.Load() < 2 && (mode == "pending before fix" || changes.Load() > 0) {
						jobs = []storage.ReviewJob{{ID: 8 + int64(phase.Load()), GitRef: head, Status: storage.JobStatusQueued}}
					}
					writeJSON(w, map[string]any{"jobs": jobs})
					return true
				}
				hooks.OnReviewClose = func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					var body struct {
						JobID int64 `json:"job_id"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					if body.JobID != 9 {
						return false
					}
					phase.Store(2)
					http.Error(w, "pending close unavailable", http.StatusInternalServerError)
					return true
				}
				hooks.OnEnqueue = func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
					respondJSON(w, http.StatusCreated, &storage.ReviewJob{ID: 10, Status: storage.JobStatusDone})
					return true
				}
			}

			md, rec := newCLIUseDaemon(t, hooks, nil)
			md.State.jobs[7], md.State.reviews[head] = &job, review
			if mode == "commit failure" {
				require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
			}
			name := "core-action-refine-test"
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, dir, _, _ string, _ io.Writer) (string, error) {
				if mode == "planning failure" {
					return "", errors.New("agent unavailable")
				}
				if mode == "no change" {
					return "No changes needed", nil
				}
				change := changes.Add(1)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n// fixed "+strconv.Itoa(int(change))+"\n"), 0o644))
				return "Fixed cancellation", nil
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			iterations := "2"
			if mode == "follow-up warnings" {
				iterations = "3"
			}
			args := []string{"refine", "--since", base, "--agent", name, "--max-iterations", iterations, "--quiet", "--server", md.Server.URL}
			if mode == "planning failure" || mode == "planning setup failure" {
				args = append(args, "--plan")
			}
			if mode == "skill" {
				args = append(args, "--from-skill")
			}
			if mode == "planning setup failure" {
				tempFile := filepath.Join(t.TempDir(), "blocked-temp-root")
				require.NoError(t, os.WriteFile(tempFile, []byte("blocked"), 0o600))
				for _, key := range []string{"TMPDIR", "TEMP", "TMP"} {
					t.Setenv(key, tempFile)
				}
			}
			result := runCLI(t, args...)
			switch mode {
			case "pending before fix", "pending after fix":
				require.NoError(t, result.err)
				assert.Contains(t, result.stdout, "Waiting for in-progress review (job 8)")
				assert.Contains(t, result.stdout, "Waiting for in-progress review (job 9)")
				assert.Contains(t, result.stdout, "Warning: review failed:")
				assert.Contains(t, result.stdout, "Warning: failed to close review (job 9)")
				assert.Contains(t, result.stdout, "All reviews passed!")
				shown := 0
				for _, post := range rec.telemetryPosts() {
					if strings.Contains(post.body, `"event":"error_shown"`) {
						shown++
						assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, post.body)
					}
				}
				if mode == "pending after fix" {
					assert.Equal(t, 2, shown)
				} else {
					assert.Zero(t, shown)
				}
			case "follow-up warnings":
				require.NoError(t, result.err)
				assert.Contains(t, result.stdout, "All reviews passed!")
				assert.Contains(t, result.stdout, "Warning: failed to add comment")
				assert.Contains(t, result.stdout, "Warning: failed to close review")
				assert.Contains(t, result.stdout, "Warning: review failed")
				assert.Equal(t, int(failures.Load()), strings.Count(result.stdout, "Warning:"))
				var shown int
				for _, post := range rec.telemetryPosts() {
					if strings.Contains(post.body, `"event":"error_shown"`) {
						shown++
						assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, post.body)
					}
				}
				assert.Equal(t, int(failures.Load()), shown)
			case "commit failure":
				require.ErrorContains(t, result.err, "failed to commit changes")
			default:
				require.ErrorContains(t, result.err, "max iterations")
			}
			if mode == "planning setup failure" {
				assert.Equal(t, int32(0), changes.Load())
				assert.Equal(t, 2, strings.Count(result.stdout, "Planning error: create planning worktree:"))
				for _, post := range rec.telemetryPosts() {
					assert.NotContains(t, post.body, `"event":"error_shown"`)
				}
			}
			switch mode {
			case "pending after fix":
				assert.Equal(t, "1", repo.Run("rev-list", "--count", head+"..HEAD"))
			case "no change", "commit failure", "planning failure", "planning setup failure", "pending before fix":
				assert.Equal(t, head, repo.HeadSHA())
			default:
				assert.Equal(t, "2", repo.Run("rev-list", "--count", head+"..HEAD"))
				assert.Contains(t, result.stdout, "Created commit")
			}
			if mode == "planning failure" {
				var errorsShown int
				for _, post := range rec.telemetryPosts() {
					if strings.Contains(post.body, `"event":"error_shown"`) {
						errorsShown++
					}
				}
				assert.Equal(t, 3, errorsShown) // Two printed retries and the returned iteration limit.
			}
			var fixes []string
			for _, post := range rec.telemetryPosts() {
				if strings.Contains(post.body, `"event":"finding_fixed"`) {
					fixes = append(fixes, post.body)
				}
			}
			if mode == "terminal" || mode == "follow-up warnings" || mode == "pending after fix" {
				count := 2
				if mode == "pending after fix" {
					count = 1
				}
				require.Len(t, fixes, count)
				for _, body := range fixes {
					assert.JSONEq(t, `{"event":"finding_fixed","properties":{"surface":"cli"}}`, body)
				}
			} else {
				assert.Empty(t, fixes)
			}
		})
	}
}

func TestCoreActionFixCommit(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, outcome := range []string{"commit", "no change", "failure", "visible retry failure", "quiet retry failure", "visible recording failure", "quiet recording failure", "planning failure", "visible enqueue failure", "quiet enqueue failure", "agent setup failure", "config setup failure"} {
			t.Run(map[bool]string{false: "single", true: "batch"}[batch]+"/"+outcome, func(t *testing.T) {
				resetCLIUse(t)
				cliOutputIsTerminal = func(io.Writer) bool { return true }
				t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
				repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
				before := repo.HeadSHA()
				hooks := MockRefineHooks{}
				if strings.Contains(outcome, "enqueue failure") {
					hooks.OnGetJobs = func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
						if r.URL.Query().Get("git_ref") == "" {
							return false
						}
						http.Error(w, "enqueue unavailable", http.StatusUnauthorized)
						return true
					}
				}
				if strings.Contains(outcome, "recording failure") {
					fail := func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
						http.Error(w, "unavailable", http.StatusInternalServerError)
						return true
					}
					hooks.OnComment, hooks.OnReviewClose = fail, fail
				}
				md, rec := newCLIUseDaemon(t, hooks, nil)
				md.State.mu.Lock()
				md.State.jobs[100] = &storage.ReviewJob{ID: 100, GitRef: "def456", Agent: "test", Status: storage.JobStatusDone}
				md.State.reviews["def456"] = &storage.Review{ID: 2, JobID: 100, Agent: "test", Output: "Finding"}
				md.State.jobs[99] = &storage.ReviewJob{ID: 99, GitRef: "abc123", Agent: "test", Status: storage.JobStatusDone}
				md.State.reviews["abc123"] = &storage.Review{ID: 1, JobID: 99, Agent: "test", Output: "Finding"}
				md.State.mu.Unlock()
				patchServerAddr(t, md.Server.URL)
				calls := 0
				fixAgent := &agent.FakeAgent{NameStr: "test", ReviewFn: func(_ context.Context, dir, _, _ string, _ io.Writer) (string, error) {
					if strings.Contains(outcome, "retry failure") {
						calls++
						if calls%2 == 0 {
							return "", errors.New("commit agent unavailable")
						}
						return "pending changes", os.WriteFile(filepath.Join(dir, "pending.txt"), []byte(strconv.Itoa(calls)), 0o644)
					}
					if outcome == "failure" || outcome == "planning failure" {
						return "", errors.New("agent failed")
					}
					if outcome == "no change" || strings.Contains(outcome, "recording failure") {
						return "No change needed", nil
					}
					file, err := os.OpenFile(filepath.Join(dir, "fixed.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
					require.NoError(t, err)
					_, err = file.WriteString("fixed\n")
					require.NoError(t, err)
					require.NoError(t, file.Close())
					for _, args := range [][]string{{"add", "fixed.txt"}, {"commit", "-m", "fix: apply change"}} {
						cmd := exec.Command("git", args...)
						cmd.Dir = dir
						output, err := cmd.CombinedOutput()
						require.NoError(t, err, "%s", output)
					}
					return "Fixed", nil
				}}
				cmd, out := newTestCmd(t)
				cmd.SetContext(t.Context())
				tracker := &fixSessionTracker{base: fixAgent, out: io.Discard}
				opts := fixOptions{agentName: "test", quiet: !strings.HasPrefix(outcome, "visible"), planOnly: outcome == "planning failure", classify: agent.ClassifyLimit}
				if outcome == "agent setup failure" {
					tracker.base = nil
					opts.agentName = "agent-does-not-exist"
				}
				if outcome == "config setup failure" {
					require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".roborev.toml"), []byte("[invalid"), 0o600))
				}
				var err error
				if batch {
					roots := currentRepoRoots{worktreeRoot: repo.Dir, mainRepoRoot: repo.Dir}
					err = processFixBatch(context.Background(), cmd, roots, []int64{99, 100}, 1, opts, tracker)
				} else {
					err = fixSingleJob(cmd, repo.Dir, 99, opts, tracker)
				}
				if (outcome == "failure" && !batch) || outcome == "planning failure" || strings.Contains(outcome, "setup failure") {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				if outcome == "commit" {
					assert.NotEqual(t, before, repo.HeadSHA())
					assert.NotEmpty(t, rec.businessPaths())
				}
				posts := rec.telemetryPosts()
				if outcome == "commit" {
					count := 1
					if batch {
						count = 2
					}
					require.Len(t, posts, count)
					for _, post := range posts {
						assert.JSONEq(t, `{"event":"finding_fixed","properties":{"surface":"cli"}}`, post.body)
					}
				} else if strings.Contains(outcome, "enqueue failure") {
					successes, shown := 0, 0
					for _, post := range posts {
						if strings.Contains(post.body, `"event":"error_shown"`) {
							shown++
						} else {
							successes++
						}
					}
					count := 1
					if batch {
						count = 2
					}
					assert.Equal(t, count, successes)
					if strings.HasPrefix(outcome, "visible") {
						assert.Contains(t, out.String(), "Warning: could not enqueue review for fix commit")
						assert.Equal(t, count, shown)
					} else {
						assert.NotContains(t, out.String(), "Warning: could not enqueue")
						assert.Zero(t, shown)
					}
				} else if outcome == "failure" || strings.HasPrefix(outcome, "visible") {
					count := 1
					if batch {
						count = 2
					}
					switch outcome {
					case "visible retry failure":
						assert.Contains(t, out.String(), "Warning: commit agent failed")
					case "visible recording failure":
						count *= 2
						assert.Contains(t, out.String(), "could not add response")
						assert.Contains(t, out.String(), "could not close")
					}
					require.Len(t, posts, count)
					for _, post := range posts {
						assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, post.body)
					}
				} else {
					assert.Empty(t, posts)
				}
			})
		}
	}
}

func TestCoreActionCLIOutputEligibility(t *testing.T) {
	resetCLIUse(t)
	md, rec := newCLIUseDaemon(t, MockRefineHooks{OnReviewClose: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		var body struct {
			JobID int64 `json:"job_id"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if body.JobID == 999 {
			w.WriteHeader(http.StatusNotFound)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		return true
	}}, nil)
	md.State.jobs[1] = &storage.ReviewJob{ID: 1, GitRef: "abc123", Status: storage.JobStatusDone}
	md.State.reviews["abc123"] = &storage.Review{ID: 1, JobID: 1, Output: "Review output"}
	for _, format := range []string{"text", "json", "prompt", "close"} {
		t.Run(format, func(t *testing.T) {
			var output string
			for _, terminal := range []bool{false, true} {
				cliOutputIsTerminal = func(io.Writer) bool { return terminal }
				rec.reset()
				args := []string{"show", "--job", "1"}
				event := "review_viewed"
				if format == "json" {
					args = append(args, "--json")
				}
				if format == "prompt" {
					args = append(args, "--prompt")
					event = ""
				}
				if format == "close" {
					args = []string{"close", "1"}
					event = "review_closed"
				}
				result := runCLIFresh(t, true, append(args, "--server", md.Server.URL)...)
				require.NoError(t, result.err)
				assert.NotEmpty(t, rec.businessPaths())
				assert.NotEmpty(t, result.stdout)
				if terminal {
					assert.Equal(t, output, result.stdout)
					posts := rec.telemetryPosts()
					wantPosts := 2
					if event == "" {
						wantPosts = 1
					}
					require.Len(t, posts, wantPosts)
					if event == "" {
						assert.JSONEq(t, cliUseBody, posts[0].body)
					} else {
						bodies := []string{posts[0].body, posts[1].body}
						assert.Contains(t, bodies, `{"event":"`+event+`","properties":{"surface":"cli"}}`)
					}
				} else {
					output = result.stdout
					posts := rec.telemetryPosts()
					require.Len(t, posts, 1)
					assert.JSONEq(t, cliUseBody, posts[0].body)
				}
			}
		})
		if format == "close" {
			cliOutputIsTerminal = func(io.Writer) bool { return true }
			for _, tc := range []struct {
				name    string
				args    []string
				wantErr bool
			}{
				{name: "reopen", args: []string{"close", "1", "--reopen"}},
				{name: "failed close", args: []string{"close", "999"}, wantErr: true},
			} {
				t.Run("close/"+tc.name, func(t *testing.T) {
					rec.reset()
					result := runCLIFresh(t, true, append(tc.args, "--server", md.Server.URL)...)
					if tc.wantErr {
						require.Error(t, result.err)
					} else {
						require.NoError(t, result.err)
					}
					for _, post := range rec.telemetryPosts() {
						assert.NotContains(t, post.body, `"event":"review_closed"`)
					}
				})
			}

			t.Run("close/automatic", func(t *testing.T) {
				rec.reset()
				clearCLIUseState()
				require.NoError(t, markJobClosed(t.Context(), md.Server.URL, 1))
				assert.Contains(t, rec.businessPaths(), "POST /api/review/close")
				assert.Empty(t, rec.telemetryPosts())
			})
		}
	}
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	t.Run("telemetry disabled", func(t *testing.T) {
		cliTelemetryEnabled = func() bool { return false }
		rec.reset()
		clearCLIUseState()
		result := runCLI(t, "show", "--job", "1", "--json", "--server", md.Server.URL)
		require.NoError(t, result.err)
		assert.Contains(t, result.stdout, "Review output")
		assert.Empty(t, rec.telemetryPosts())
	})
}

func TestCoreActionWaitDisplaysReview(t *testing.T) {
	for _, mode := range []string{"terminal", "quiet"} {
		verdicts := []int{1}
		if mode == "terminal" {
			verdicts = append(verdicts, 0)
		}
		for _, verdict := range verdicts {
			t.Run(strconv.Itoa(verdict)+"/"+mode, func(t *testing.T) {
				resetCLIUse(t)
				cliOutputIsTerminal = func(io.Writer) bool { return true }
				mux := http.NewServeMux()
				md := daemonFromHandler(t, mux)
				rec := &cliUseRecorder{}
				mux.HandleFunc(daemon.TelemetryEventsPath, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					rec.mu.Lock()
					rec.telemetry = append(rec.telemetry, recordedTelemetry{body: string(body)})
					rec.mu.Unlock()
					w.WriteHeader(http.StatusAccepted)
				})
				mux.HandleFunc(daemon.TelemetryAgentCallPath, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
				mockWaitableReview(t, mux, "Review output", verdict)
				args := []string{"wait", "1"}
				if mode == "quiet" {
					args = append(args, "--quiet")
				}
				args = append(args, "--server", md.Server.URL)
				result := runCLI(t, args...)
				if verdict == 0 {
					requireExitCode(t, result.err, 1)
				} else {
					require.NoError(t, result.err)
				}
				if mode == "quiet" {
					assert.NotContains(t, result.stdout, "Review output")
				} else {
					assert.Contains(t, result.stdout, "Review output")
				}
				var views []string
				for _, post := range rec.telemetryPosts() {
					if strings.Contains(post.body, `"review_viewed"`) {
						views = append(views, post.body)
					}
				}
				if mode == "terminal" {
					require.Len(t, views, 1)
					assert.JSONEq(t, `{"event":"review_viewed","properties":{"surface":"cli"}}`, views[0])
				} else {
					assert.Empty(t, views)
				}
			})
		}
	}
}

func TestOutcomeCLIErrorEligibility(t *testing.T) {
	resetCLIUse(t)
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	repo := newTestGitRepo(t)
	repo.CommitFile("source.txt", "content", "initial commit")
	md, rec := newCLIUseDaemon(t, MockRefineHooks{OnReviewClose: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
		w.WriteHeader(http.StatusNotFound)
		return true
	}, OnEnqueue: func(w http.ResponseWriter, _ *http.Request, state *mockRefineState) bool {
		respondJSON(w, http.StatusCreated, state.jobs[999])
		return true
	}}, nil)
	for _, id := range []int64{999, 1000} {
		md.State.jobs[id] = &storage.ReviewJob{ID: id, Status: storage.JobStatusFailed, Error: "review unavailable"}
	}
	for _, tc := range []struct {
		args  []string
		kind  string
		count int
	}{
		{[]string{"show", "--job", "999"}, "review_viewed", 1},
		{[]string{"close", "999"}, "review_closed", 1},
		{[]string{"wait", "--job", "999"}, "review_viewed", 1},
		{[]string{"wait", "--job", "999", "1000"}, "review_viewed", 2},
		{[]string{"wait", "--job", "999", "1000", "--quiet"}, "review_viewed", 1},
		{[]string{"review", "--repo", repo.Dir, "--wait"}, "review_viewed", 1},
	} {
		rec.reset()
		result := runCLIFresh(t, true, append(tc.args, "--server", md.Server.URL)...)
		require.Error(t, result.err)
		var shown []string
		for _, post := range rec.telemetryPosts() {
			if strings.Contains(post.body, `"event":"error_shown"`) {
				shown = append(shown, post.body)
			}
		}
		require.Len(t, shown, tc.count, "%v", tc.args)
		for _, body := range shown {
			assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"`+tc.kind+`"}}`, body)
		}
	}
}

func TestOutcomeCLISetupExclusions(t *testing.T) {
	resetCLIUse(t)
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	unavailable := false
	md, rec := newCLIUseDaemon(t, MockRefineHooks{OnGetJobs: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		if r.URL.Query().Get("id") != "998" {
			return false
		}
		writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
		return true
	}, OnPing: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
		if !unavailable {
			return false
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return true
	}}, nil)
	md.State.jobs[999] = &storage.ReviewJob{ID: 999, Status: storage.JobStatusFailed}
	repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
	outside := t.TempDir()
	for _, tc := range []struct {
		args        []string
		dir, want   string
		unavailable bool
	}{
		{[]string{"fix", "invalid"}, outside, "invalid job ID", false},
		{[]string{"fix", "invalid", "--batch"}, outside, "invalid job ID", false},
		{[]string{"fix", "--branch", "main", "99"}, repo.Dir, "--branch cannot be used", false},
		{[]string{"refine"}, outside, "git", false},
		{[]string{"fix", "998"}, repo.Dir, "not found", false},
		{[]string{"fix", "998", "--batch"}, repo.Dir, "", false},
		{[]string{"fix", "999", "--batch"}, repo.Dir, "", false},
		{[]string{"fix", "999", "--plan-only"}, repo.Dir, "not complete", false},
		{[]string{"close", "invalid"}, repo.Dir, "invalid job_id", false},
		{[]string{"close", "invalid"}, repo.Dir, "daemon not running", true},
		{[]string{"close", "999"}, repo.Dir, "daemon not running", true},
		{[]string{"show", "--job", "999"}, repo.Dir, "daemon not running", true},
		{[]string{"wait", "--job", "999", "1000"}, repo.Dir, "daemon not running", true},
	} {
		t.Run(strings.Join(tc.args, " ")+tc.dir, func(t *testing.T) {
			t.Chdir(tc.dir)
			unavailable = tc.unavailable
			rec.reset()
			result := runCLIFresh(t, true, append(tc.args, "--server", md.Server.URL)...)
			if tc.want == "" {
				require.NoError(t, result.err)
			} else {
				require.ErrorContains(t, result.err, tc.want)
			}
			for _, post := range rec.telemetryPosts() {
				assert.NotContains(t, post.body, `"event":"error_shown"`)
			}
		})
	}
}

func TestOutcomeAnalyzeAgentStart(t *testing.T) {
	resetCLIUse(t)
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
	md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)
	md.State.reviews["analysis"] = &storage.Review{JobID: 99, Output: "High: missing cancellation"}
	name := "outcome-analyze-failure"
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return "", errors.New("agent execution failed")
	}})
	t.Cleanup(func() { agent.Unregister(name) })
	for _, tc := range []struct {
		name, want string
		count      int
		jobID      int64
	}{{"analysis failed", "analysis failed", 0, 999}, {"unavailable-fix-agent", "get agent", 0, 99}, {name, "agent execution failed", 1, 99}} {
		t.Run(tc.name, func(t *testing.T) {
			rec.reset()
			job := &storage.ReviewJob{ID: tc.jobID, GitRef: "analysis", Status: storage.JobStatusDone}
			if tc.jobID == 999 {
				job.Status = storage.JobStatusFailed
				job.Error = "analysis unavailable"
			}
			md.State.jobs = map[int64]*storage.ReviewJob{tc.jobID: job}
			cmd, _ := newTestCmd(t)
			cmd.SetContext(t.Context())
			err := runAnalyzeAndFix(cmd, getDaemonEndpoint(), repo.Dir, tc.jobID, &analyze.AnalysisType{Name: "duplication"}, analyzeOptions{fixAgent: tc.name, quiet: true})
			require.ErrorContains(t, err, tc.want)
			posts := rec.telemetryPosts()
			require.Len(t, posts, tc.count)
			if tc.count > 0 {
				assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, posts[0].body)
			}
		})
	}
}

func TestOutcomeCLIReportingGates(t *testing.T) {
	resetCLIUse(t)
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	_, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)
	cmd, _ := newTestCmd(t)
	cmd.SetContext(t.Context())
	for _, tc := range []struct{ terminal, skill bool }{{false, false}, {true, true}} {
		cliOutputIsTerminal = func(io.Writer) bool { return tc.terminal }
		fromSkill = tc.skill
		reportCLIError(cmd, "finding_fixed", errors.New("fix failed"))
		assert.Empty(t, rec.telemetryPosts())
	}
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	fromSkill = false
	reportCLIError(cmd, "finding_fixed", errors.Join(errors.New("agent stopped"), context.Canceled))
	assert.Empty(t, rec.telemetryPosts())
	reportCLIError(cmd, "finding_fixed", context.DeadlineExceeded)
	require.Len(t, rec.telemetryPosts(), 1)
	assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, rec.telemetryPosts()[0].body)
}
