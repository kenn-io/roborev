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
		name    string
		enabled bool
	}{
		{name: "enabled skips docs fix re-review", enabled: true},
		{name: "default waits for re-review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalUnsafe := agent.AllowUnsafeAgents()
			t.Cleanup(func() { agent.SetAllowUnsafeAgents(originalUnsafe) })
			repo := newPlanTestRepo(t, map[string]string{"README.md": "Base docs\n", "main.go": "package main\n"})
			base := repo.HeadSHA()
			initial := repo.CommitFile("README.md", "Docs with error\n", "Update docs")
			original := storage.Review{
				ID: 7, JobID: 7, Output: "Medium: correct docs",
				VerdictBool: testutil.ReviewFixtureVerdict("Medium: correct docs"),
				Job:         &storage.ReviewJob{ID: 7, GitRef: initial, JobType: storage.JobTypeReview, Status: storage.JobStatusDone},
			}
			passed := storage.Review{ID: 9, JobID: 9, Output: "No issues found.", VerdictBool: testutil.ReviewFixtureVerdict("No issues found.")}
			var fixLookups, branchEnqueues int
			var comments []string
			var closed []int64
			fixSHA := func() string {
				if sha := repo.HeadSHA(); sha != initial {
					return sha
				}
				return ""
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
					case id == 8 || (sha != "" && sha == fixSHA()):
						fixLookups++
						writeJSON(w, storage.Review{
							ID: 8, JobID: 8, Output: "No issues found.",
							VerdictBool: testutil.ReviewFixtureVerdict("No issues found."),
							Job:         &storage.ReviewJob{ID: 8, GitRef: fixSHA(), JobType: storage.JobTypeReview, Status: storage.JobStatusDone},
						})
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
					case id == 8 || (ref != "" && ref == fixSHA()):
						fixLookups++
						jobs = []storage.ReviewJob{{ID: 8, GitRef: fixSHA(), Status: storage.JobStatusDone}}
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
						Comment string `json:"comment"`
					}
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
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
					w.WriteHeader(http.StatusOK)
				}).
				WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						GitRef string `json:"git_ref"`
					}
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					assert.Equal(t, base+".."+fixSHA(), body.GitRef)
					branchEnqueues++
					w.WriteHeader(http.StatusCreated)
					writeJSON(w, storage.ReviewJob{ID: 9})
				}).Build()
			name := "refine-trivial-test"
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, _, _ string, _ io.Writer) (string, error) {
				require.NoError(t, os.WriteFile(filepath.Join(path, "README.md"), []byte("Corrected docs\n"), 0o600))
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
			chdir(t, repo.Dir)
			var runErr error
			stdout := captureOutput(t, func() error {
				runErr = cmd.RunE(cmd, nil)
				return nil
			})
			require.NoError(t, runErr)
			a := assert.New(t)
			a.NotEmpty(fixSHA())
			a.Equal(1, branchEnqueues, "branch review still runs")
			a.Contains(closed, int64(7))
			a.Contains(stdout, "All reviews passed!")
			if tc.enabled {
				a.Zero(fixLookups, "refine must not wait on the fix commit's review")
				a.Contains(strings.Join(comments, "\n"), refineSkipNote)
				a.Contains(stdout, refineSkipNote)
			} else {
				a.Positive(fixLookups)
				a.Contains(closed, int64(8))
				a.NotContains(stdout, refineSkipNote)
			}
		})
	}
}
