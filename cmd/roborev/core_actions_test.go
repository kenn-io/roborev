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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestCoreActionRefineCommit(t *testing.T) {
	for _, mode := range []string{"terminal", "skill", "no change", "commit failure", "retry failures"} {
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
			md, rec := newCLIUseDaemon(t, MockRefineHooks{OnReview: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
				writeJSON(w, review)
				return true
			}}, nil)
			md.State.jobs[7], md.State.reviews[head] = &job, review
			if mode == "commit failure" {
				require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
			}
			name := "core-action-refine-test"
			changes := 0
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, dir, _, _ string, _ io.Writer) (string, error) {
				if mode == "retry failures" {
					changes++
					return "", errors.New("agent unavailable")
				}
				if mode == "no change" {
					return "No changes needed", nil
				}
				changes++
				require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n// fixed "+strconv.Itoa(changes)+"\n"), 0o644))
				return "Fixed cancellation", nil
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			args := []string{"refine", "--since", base, "--agent", name, "--max-iterations", "3", "--quiet", "--server", md.Server.URL}
			if mode == "skill" {
				args = append(args, "--from-skill")
			}
			result := runCLI(t, args...)
			if mode == "commit failure" {
				require.ErrorContains(t, result.err, "failed to commit changes")
			} else {
				require.ErrorContains(t, result.err, "max iterations")
			}
			if mode == "no change" || mode == "commit failure" || mode == "retry failures" {
				assert.Equal(t, head, repo.HeadSHA())
			} else {
				assert.Equal(t, "3", repo.Run("rev-list", "--count", head+"..HEAD"))
				assert.Contains(t, result.stdout, "Created commit")
			}
			var fixes, failures []string
			for _, post := range rec.telemetryPosts() {
				if strings.Contains(post.body, `"event":"finding_fixed"`) {
					fixes = append(fixes, post.body)
				}
				if strings.Contains(post.body, `"event":"error_shown"`) {
					failures = append(failures, post.body)
				}
			}
			if mode == "terminal" {
				assert.Len(t, fixes, 3)
			} else {
				assert.Empty(t, fixes)
			}
			if mode == "skill" {
				assert.Empty(t, failures)
			} else {
				require.Len(t, failures, 1)
				assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, failures[0])
			}
			if mode == "retry failures" {
				assert.Equal(t, 3, changes)
			}

		})
	}
}

func TestCoreActionFixCommit(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, outcome := range []string{"commit", "bookkeeping failure", "no change", "failure"} {
			t.Run(map[bool]string{false: "single", true: "batch"}[batch]+"/"+outcome, func(t *testing.T) {
				resetCLIUse(t)
				cliOutputIsTerminal = func(io.Writer) bool { return true }
				t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
				repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
				before := repo.HeadSHA()
				md, rec := newCLIUseDaemon(t, MockRefineHooks{
					OnGetJobs: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
						if outcome != "bookkeeping failure" || r.URL.Query().Get("git_ref") == "" {
							return false
						}
						writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
						return true
					},
					OnEnqueue: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
						if outcome != "bookkeeping failure" {
							return false
						}
						http.Error(w, "enqueue unavailable", http.StatusInternalServerError)
						return true
					},
					OnComment: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
						if outcome != "bookkeeping failure" {
							return false
						}
						http.Error(w, "comment unavailable", http.StatusInternalServerError)
						return true
					},
					OnReviewClose: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
						if outcome != "bookkeeping failure" {
							return false
						}
						http.Error(w, "close unavailable", http.StatusInternalServerError)
						return true
					},
				}, nil)
				md.State.mu.Lock()
				md.State.jobs[100] = &storage.ReviewJob{ID: 100, GitRef: "def456", Agent: "test", Status: storage.JobStatusDone}
				md.State.reviews["def456"] = &storage.Review{ID: 2, JobID: 100, Agent: "test", Output: "Finding"}
				md.State.jobs[99] = &storage.ReviewJob{ID: 99, GitRef: "abc123", Agent: "test", Status: storage.JobStatusDone}
				md.State.reviews["abc123"] = &storage.Review{ID: 1, JobID: 99, Agent: "test", Output: "Finding"}
				md.State.mu.Unlock()
				patchServerAddr(t, md.Server.URL)
				fixAgent := &agent.FakeAgent{NameStr: "test", ReviewFn: func(_ context.Context, dir, _, _ string, _ io.Writer) (string, error) {
					if outcome == "failure" {
						return "", errors.New("agent failed")
					}
					if outcome == "no change" {
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
				cmd, output := newTestCmd(t)
				cmd.SetContext(t.Context())
				tracker := &fixSessionTracker{base: fixAgent, out: io.Discard}
				opts := fixOptions{agentName: "test", quiet: outcome != "bookkeeping failure", classify: agent.ClassifyLimit}
				var err error
				if batch {
					roots := currentRepoRoots{worktreeRoot: repo.Dir, mainRepoRoot: repo.Dir}
					err = processFixBatch(context.Background(), cmd, roots, []int64{99, 100}, 1, opts, tracker)
				} else {
					err = fixSingleJob(cmd, repo.Dir, 99, opts, tracker)
				}
				if outcome == "failure" && !batch {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				if outcome == "commit" || outcome == "bookkeeping failure" {
					assert.NotEqual(t, before, repo.HeadSHA())
					assert.NotEmpty(t, rec.businessPaths())
				}
				if outcome == "bookkeeping failure" {
					assert.Contains(t, output.String(), "could not enqueue review for fix commit")
					assert.Contains(t, output.String(), "could not add response to job")
					assert.Contains(t, output.String(), "could not close job")
				}
				posts := rec.telemetryPosts()
				if outcome == "commit" || outcome == "bookkeeping failure" {
					count := 1
					if batch {
						count = 2
					}
					require.Len(t, posts, count)
					for _, post := range posts {
						assert.JSONEq(t, `{"event":"finding_fixed","properties":{"surface":"cli"}}`, post.body)
					}
				} else if outcome == "failure" {
					require.Len(t, posts, 1)
					assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, posts[0].body)
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
					var actions []string
					for _, post := range rec.telemetryPosts() {
						if post.body != cliUseBody {
							actions = append(actions, post.body)
						}
					}
					if tc.wantErr {
						require.Len(t, actions, 1)
						assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"review_closed"}}`, actions[0])
					} else {
						assert.Empty(t, actions)
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
					if strings.Contains(post.body, `"event":"review_viewed"`) {
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
	for range 3 {
		reportCLIError(cmd, "finding_fixed", context.DeadlineExceeded)
	}
	require.Len(t, rec.telemetryPosts(), 1)
	assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, rec.telemetryPosts()[0].body)
}

func TestOutcomeFixBeforeAgentStart(t *testing.T) {
	resetCLIUse(t)
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
	t.Chdir(repo.Dir)
	md, rec := newCLIUseDaemon(t, MockRefineHooks{OnGetJobs: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
		writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
		return true
	}}, nil)
	result := runCLIFresh(t, true, "fix", "998", "--server", md.Server.URL)
	require.ErrorContains(t, result.err, "not found")
	var failures []string
	for _, post := range rec.telemetryPosts() {
		if strings.Contains(post.body, `"event":"error_shown"`) {
			failures = append(failures, post.body)
		}
	}
	require.Len(t, failures, 1)
	assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, failures[0])
}

func TestOutcomeBatchFixMissingJobs(t *testing.T) {
	for _, missing := range []string{"jobs", "reviews"} {
		t.Run(missing, func(t *testing.T) {
			resetCLIUse(t)
			cliOutputIsTerminal = func(io.Writer) bool { return true }
			repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
			t.Chdir(repo.Dir)
			md, rec := newCLIUseDaemon(t, MockRefineHooks{
				OnGetJobs: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
					if missing == "reviews" {
						return false
					}
					writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
					return true
				},
				OnReview: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
					http.Error(w, "review missing", http.StatusNotFound)
					return true
				},
			}, nil)
			for _, id := range []int64{998, 999} {
				md.State.jobs[id] = &storage.ReviewJob{ID: id, GitRef: "abc123", Agent: "test", Status: storage.JobStatusDone}
			}
			result := runCLIFresh(t, true, "fix", "998", "999", "--batch", "--server", md.Server.URL)
			require.NoError(t, result.err)
			assert.Contains(t, result.stdout, "Warning: skipping job 998")
			assert.Contains(t, result.stdout, "Warning: skipping job 999")
			var failures []string
			for _, post := range rec.telemetryPosts() {
				if strings.Contains(post.body, `"event":"error_shown"`) {
					failures = append(failures, post.body)
				}
			}
			require.Len(t, failures, 1)
			assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, failures[0])
		})
	}
}
