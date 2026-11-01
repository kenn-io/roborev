package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestRefineSkipTrivialRereview(t *testing.T) {
	originalDelay := postCommitWaitDelay
	postCommitWaitDelay = time.Millisecond
	t.Cleanup(func() { postCommitWaitDelay = originalDelay })

	for _, tc := range []struct {
		name                                                      string
		enabled, wantSkip, commentFails, plan, hookAddsDocsCommit bool
		autoStatus                                                storage.JobStatus
		autoOutput                                                string
	}{
		{name: "queued hook review", enabled: true, wantSkip: true, autoStatus: storage.JobStatusQueued},
		{name: "queued hook review after docs commit", enabled: true, wantSkip: true, hookAddsDocsCommit: true, autoStatus: storage.JobStatusQueued},
		{name: "failed hook review after docs commit", enabled: true, wantSkip: true, hookAddsDocsCommit: true, autoStatus: storage.JobStatusDone, autoOutput: "Medium: another issue"},
		{name: "default off", autoStatus: storage.JobStatusDone, autoOutput: "No issues found."},
		{name: "planning recording failure", enabled: true, commentFails: true, plan: true, autoStatus: storage.JobStatusDone, autoOutput: "Medium: another issue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalUnsafe := agent.AllowUnsafeAgents()
			t.Cleanup(func() { agent.SetAllowUnsafeAgents(originalUnsafe) })
			repo := newPlanTestRepo(t, map[string]string{"README.md": "Base docs\n", "main.go": "package main\n"})
			base := repo.HeadSHA()
			initial := repo.CommitFile("README.md", "Docs with error\n", "Update docs")
			hooks := filepath.Join(repo.Dir, ".git", "hooks")
			require.NoError(t, os.MkdirAll(hooks, 0o755))
			hook := "#!/bin/sh\n"
			if tc.hookAddsDocsCommit {
				hook += "if [ ! -f \"$(git rev-parse --git-path refine-skip-hook-ran)\" ]; then\n" +
					"  touch \"$(git rev-parse --git-path refine-skip-hook-ran)\"\n" +
					"  git rev-parse HEAD > \"$(git rev-parse --git-path refine-skip-first-hook-commit)\"\n" +
					"  printf 'Additional documentation\\n' > HOOK.md\n" +
					"  git add HOOK.md\n" +
					"  git -c user.name='Test User' -c user.email=test@example.com commit -m 'Add hook documentation'\n" +
					"fi\n"
			}
			hook += "git rev-parse HEAD > \"$(git rev-parse --git-path refine-skip-hook)\"\n"
			require.NoError(t, os.WriteFile(filepath.Join(hooks, "post-commit"), []byte(hook), 0o755))
			repo.Run("config", "core.hooksPath", hooks)
			if tc.autoOutput == "" {
				tc.autoOutput = "No issues found."
			}
			original := trivialReview("medium")
			original.ID, original.JobID = 7, 7
			original.Job.ID, original.Job.GitRef, original.Job.Status = 7, initial, storage.JobStatusDone
			original.VerdictBool = testutil.ReviewFixtureVerdict("Medium: correct docs")
			original.Output = "Medium: correct docs"
			auto := storage.Review{ID: 8, JobID: 8, Output: tc.autoOutput, VerdictBool: testutil.ReviewFixtureVerdict(tc.autoOutput)}
			passed := storage.Review{ID: 9, JobID: 9, Output: "No issues found.", VerdictBool: testutil.ReviewFixtureVerdict("No issues found.")}
			var autoLookups, branchEnqueues, agentCalls int
			var autoDone bool
			var comments []string
			var closed []int64
			fixedSHA := func() string {
				data, err := os.ReadFile(filepath.Join(repo.Dir, ".git", "refine-skip-hook"))
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(data))
			}
			firstHookSHA := func() string {
				data, err := os.ReadFile(filepath.Join(repo.Dir, ".git", "refine-skip-first-hook-commit"))
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(data))
			}
			isHookCommit := func(sha string) bool {
				return sha == fixedSHA() || (tc.hookAddsDocsCommit && sha == firstHookSHA())
			}
			isFailedHookReview := func(sha string) bool {
				return tc.hookAddsDocsCommit && sha == firstHookSHA() &&
					tc.autoStatus == storage.JobStatusDone &&
					strings.Contains(tc.autoOutput, "Medium:")
			}
			_ = newMockDaemonBuilder(t).
				WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
					id, _ := strconv.ParseInt(r.URL.Query().Get("job_id"), 10, 64)
					sha := r.URL.Query().Get("sha")
					switch {
					case id == 7 || sha == initial:
						writeJSON(w, original)
					case id == 9:
						writeJSON(w, passed)
					case id == 8 || (sha != "" && (sha == fixedSHA() || isFailedHookReview(sha))):
						reviewSHA := sha
						if reviewSHA == "" {
							reviewSHA = fixedSHA()
						}
						auto.Job = &storage.ReviewJob{ID: 8, GitRef: reviewSHA, JobType: storage.JobTypeReview, Status: storage.JobStatusDone}
						autoLookups++
						writeJSON(w, auto)
					default:
						w.WriteHeader(http.StatusNotFound)
					}
				}).
				WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
					id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
					ref := r.URL.Query().Get("git_ref")
					var jobs []storage.ReviewJob
					switch {
					case id == 9:
						jobs = []storage.ReviewJob{{ID: 9, Status: storage.JobStatusDone}}
					case id == 8 || (ref != "" && isHookCommit(ref)):
						status := tc.autoStatus
						if autoDone {
							status = storage.JobStatusDone
						}
						if id == 8 {
							autoDone = true
							status = storage.JobStatusDone
						}
						autoLookups++
						jobs = []storage.ReviewJob{{ID: 8, GitRef: ref, Status: status}}
					case id == 7:
						jobs = []storage.ReviewJob{*original.Job}
					}
					writeJSON(w, map[string]any{"jobs": jobs})
				}).
				WithHandler("/api/comments", func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, map[string]any{"responses": []storage.Response{}})
				}).
				WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						JobID   int64  `json:"job_id"`
						Comment string `json:"comment"`
					}
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					if tc.plan {
						assert.Contains(t, []int64{7, 8}, body.JobID)
					} else {
						assert.EqualValues(t, 7, body.JobID)
					}
					if tc.commentFails && strings.HasPrefix(body.Comment, "Created commit ") {
						http.Error(w, "recording unavailable", http.StatusInternalServerError)
						return
					}
					comments = append(comments, body.Comment)
					w.WriteHeader(http.StatusCreated)
				}).
				WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						JobID int64 `json:"job_id"`
					}
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					closed = append(closed, body.JobID)
					if body.JobID == 7 {
						original.Closed = true
					}
					if body.JobID == 8 {
						auto.Closed = true
					}
					w.WriteHeader(http.StatusOK)
				}).
				WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						GitRef string `json:"git_ref"`
					}
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					assert.Equal(t, base+".."+fixedSHA(), body.GitRef)
					branchEnqueues++
					w.WriteHeader(http.StatusCreated)
					writeJSON(w, storage.ReviewJob{ID: 9})
				}).Build()
			name := "refine-trivial-test"
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, _, prompt string, _ io.Writer) (string, error) {
				agentCalls++
				if tc.plan {
					if agentCalls > 2 {
						assert.NotContains(
							t, prompt, "Skipped individual re-review of",
							"a failed comment write must not fabricate skip history",
						)
					}
					if agentCalls%2 == 1 {
						return "Correct the documentation", nil
					}
					if agentCalls == 4 {
						return "No further changes", nil
					}
				}
				file, content := "README.md", "Corrected docs\n"
				require.NoError(t, os.WriteFile(filepath.Join(path, file), []byte(content), 0o600))
				return "Corrected the finding", nil
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			cmd := refineCmd()
			cmd.SetContext(t.Context())
			require.NoError(t, cmd.Flags().Set("agent", name))
			require.NoError(t, cmd.Flags().Set("since", base))
			require.NoError(t, cmd.Flags().Set("max-iterations", "3"))
			require.NoError(t, cmd.Flags().Set("quiet", "true"))
			require.NoError(t, cmd.Flags().Set("allow-unsafe-agents", "true"))
			if tc.enabled {
				require.NoError(t, cmd.Flags().Set("skip-trivial-rereview", "true"))
			}
			if tc.plan {
				require.NoError(t, cmd.Flags().Set("plan", "true"))
			}
			chdir(t, repo.Dir)
			var runErr error
			stdout := captureOutput(t, func() error {
				runErr = cmd.RunE(cmd, nil)
				return nil
			})
			require.NoError(t, runErr)
			a := assert.New(t)
			if tc.plan {
				a.Equal(4, agentCalls)
			} else {
				a.Equal(1, agentCalls)
			}
			a.NotEmpty(fixedSHA(), "post-commit hook still ran")
			a.Equal(1, branchEnqueues)
			a.Contains(closed, int64(7))
			reason := "Skipped individual re-review of " + fixedSHA() + ": documentation/test-only fix; all findings Medium or lower in a non-security review."
			if tc.wantSkip {
				skipSummary := "1 fix commit"
				if tc.hookAddsDocsCommit {
					skipSummary = "2 fix commits"
				}
				a.Zero(autoLookups, "skipped automatic reviews must not be fetched or closed")
				a.NotContains(closed, int64(8))
				a.Contains(strings.Join(comments, "\n"), reason)
				a.Contains(stdout, reason)
				a.Contains(stdout, "Branch review passed; individual re-reviews were skipped for "+skipSummary+".")
				a.NotContains(stdout, "All reviews passed!")
			} else {
				a.Positive(autoLookups)
				if tc.plan {
					a.NotContains(closed, int64(8))
				} else {
					a.Contains(closed, int64(8))
				}
				a.NotContains(stdout, reason)
				a.Contains(stdout, "All reviews passed!")
			}
		})
	}
}
