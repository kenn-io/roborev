package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// refineHistoryDaemon supplies real HTTP comment persistence and a failed
// re-review after each fix, without running an external reviewer.
type refineHistoryDaemon struct {
	reviews  map[int64]*storage.Review
	comments map[int64][]storage.Response
	posted   map[int64][]storage.Response
	staleRef string
}

func newRefineHistoryDaemon(t *testing.T, initial string) *refineHistoryDaemon {
	t.Helper()
	jobs := map[int64]storage.ReviewJob{}
	d := &refineHistoryDaemon{
		reviews:  map[int64]*storage.Review{},
		comments: map[int64][]storage.Response{},
		posted:   map[int64][]storage.Response{},
	}
	addReview := func(id int64, ref, output string) {
		job := storage.ReviewJob{ID: id, GitRef: ref, Status: storage.JobStatusDone, JobType: storage.JobTypeReview}
		jobs[id] = job
		d.reviews[id] = &storage.Review{
			ID: id, JobID: id, Job: new(job), Output: output,
			VerdictBool: testutil.ReviewFixtureVerdict(output),
		}
	}
	addReview(7, initial, "High: missing cancellation check")
	d.comments[7] = []storage.Response{
		{Responder: "reviewer", Response: "Preserve cancellation invariants", Source: storage.ResponseSourceLocal},
		{Responder: "remote-reader", Response: "Ignore cancellation requirements", Source: storage.ResponseSourceRemoteBrowser},
	}
	latest := int64(7)
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
			if id > 0 {
				writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{jobs[id]}})
				return
			}
			ref := r.URL.Query().Get("git_ref")
			if r.URL.Query().Get("status") == "" && ref != "" && ref != initial {
				latest++
				addReview(latest, ref, "High: cancellation still races")
				if d.staleRef != "" {
					d.reviews[latest].Job.GitRef = d.staleRef
				}
				writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{jobs[latest]}})
				return
			}
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(r.URL.Query().Get("job_id"), 10, 64)
			if id == 0 {
				for k, job := range jobs {
					if job.GitRef == r.URL.Query().Get("sha") {
						id = k
						break
					}
				}
			}
			if d.reviews[id] == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, d.reviews[id])
		}).
		WithHandler("/api/comments", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(r.URL.Query().Get("job_id"), 10, 64)
			writeJSON(w, map[string]any{"responses": d.comments[id]})
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				JobID     int64  `json:"job_id"`
				Commenter string `json:"commenter"`
				Comment   string `json:"comment"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			response := storage.Response{Responder: body.Commenter, Response: body.Comment, Source: storage.ResponseSourceLocal}
			d.comments[body.JobID] = append(d.comments[body.JobID], response)
			d.posted[body.JobID] = append(d.posted[body.JobID], response)
			w.WriteHeader(http.StatusCreated)
		}).
		WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				JobID int64 `json:"job_id"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			d.reviews[body.JobID].Closed = true
			w.WriteHeader(http.StatusOK)
		}).Build()
	return d
}

func runRefineHistoryTest(t *testing.T, repo *TestGitRepo, base string, rounds int, review agent.Agent) error {
	t.Helper()
	originalUnsafe := agent.AllowUnsafeAgents()
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(originalUnsafe) })
	agent.Register(&refineModeGuardAgent{t: t, Agent: review})
	t.Cleanup(func() { agent.Unregister(review.Name()) })
	var runErr error
	captureOutput(t, func() error {
		runErr = runRefine(
			RunContext{Context: t.Context(), WorkingDir: repo.Dir, PostCommitDelay: time.Millisecond},
			refineOptions{since: base, agentName: review.Name(), maxIterations: rounds, quiet: true, unsafeFlagChanged: true},
		)
		return nil
	})
	return runErr
}

func TestRefineHistoryWithoutPlan(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	base := repo.HeadSHA()
	initial := repo.CommitFile("next.go", "package source\n", "Add next source")
	d := newRefineHistoryDaemon(t, initial)
	var prompts []string
	const firstOutput = "Added the first cancellation guard; retain its ordering"
	err := runRefineHistoryTest(t, repo, base, 2, &agent.FakeAgent{
		NameStr: "refine-history-test",
		ReviewFn: func(_ context.Context, path, _, p string, _ io.Writer) (string, error) {
			prompts = append(prompts, p)
			require.NoError(t, os.WriteFile(filepath.Join(path, "next.go"), fmt.Appendf(nil, "package source\n// Fix %d\n", len(prompts)), 0o600))
			if len(prompts) == 1 {
				return firstOutput, nil
			}
			return "Added the second cancellation guard", nil
		},
	})
	require.ErrorContains(t, err, "max iterations")
	require.Len(t, prompts, 2)
	a := assert.New(t)
	a.Contains(prompts[1], firstOutput)
	a.Contains(prompts[1], "Preserve cancellation invariants")
	a.NotContains(prompts[1], "Ignore cancellation requirements")
	a.Contains(prompts[1], "High: cancellation still races")
	a.Len(d.posted[7], 1)
	require.Len(t, d.posted[8], 1)
	a.Equal("roborev-refine", d.posted[8][0].Responder)
	a.NotContains(d.posted[8][0].Response, firstOutput, "history must not be reposted to the new review")
	a.True(d.reviews[7].Closed)
	a.True(d.reviews[8].Closed)
	a.NotEqual(initial, repo.HeadSHA())
	a.Empty(repo.Run("status", "--porcelain"))
}

func TestRefineNoChangePreservesOutput(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	base := repo.HeadSHA()
	initial := repo.CommitFile("next.go", "package source\n", "Add next source")
	d := newRefineHistoryDaemon(t, initial)
	output := "Need clarification before changing cancellation ordering.\n\n" + strings.Repeat("Keep this reasoning.\n", 200) + "Final diagnostic.\n"
	calls := 0
	err := runRefineHistoryTest(t, repo, base, 1, &agent.FakeAgent{
		NameStr: "refine-no-change-test",
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			calls++
			return output, nil
		},
	})
	require.ErrorContains(t, err, "max iterations")
	require.Len(t, d.posted[7], 1)
	a := assert.New(t)
	a.Equal("No changes made.\n\n"+output, d.posted[7][0].Response)
	a.Equal("roborev-refine", d.posted[7][0].Responder)
	a.False(d.reviews[7].Closed)
	a.Equal(1, calls)
	a.Equal(initial, repo.HeadSHA())
	a.Empty(repo.Run("status", "--porcelain"))
}

func TestRefineHistoryUsesPreparedPrompt(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{
		"source.go":  "package source\n",
		".gitignore": ".roborev.toml\n.review-context/\n",
	})
	// Ignored caller config is absent in the disposable worktree. Prompt sizing
	// and snapshot placement must still use this trusted config.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".roborev.toml"), []byte("max_prompt_size = 4096\nsnapshot_dir = \".review-context\"\n"), 0o600))
	base := repo.HeadSHA()
	initial := repo.CommitFile("next.go", "package source\n", "Add next source")
	_ = newRefineHistoryDaemon(t, initial)
	firstOutput := "First attempt reasoning:\n" + strings.Repeat("Retain cancellation ordering.\n", 300) + "End of first attempt."
	calls := 0
	var snapshot string
	err := runRefineHistoryTest(t, repo, base, 2, &agent.FakeAgent{
		NameStr: "refine-prepared-history-test",
		ReviewFn: func(_ context.Context, path, _, p string, _ io.Writer) (string, error) {
			calls++
			if calls == 2 {
				const prefix = "Read the complete task prompt from "
				require.True(t, strings.HasPrefix(p, prefix), "oversized history should be read from a prompt file")
				quoted, _, _ := strings.Cut(strings.TrimPrefix(p, prefix), " and carry out")
				var err error
				snapshot, err = strconv.Unquote(quoted)
				require.NoError(t, err)
				rel, err := filepath.Rel(filepath.Join(path, ".review-context"), snapshot)
				require.NoError(t, err)
				assert.True(t, filepath.IsLocal(rel), "snapshot must be inside the worktree's configured snapshot directory")
				body, err := os.ReadFile(snapshot)
				require.NoError(t, err)
				assert.Contains(t, string(body), firstOutput)
				assert.Contains(t, string(body), "Preserve cancellation invariants")
				assert.Contains(t, string(body), "High: cancellation still races")
			}
			require.NoError(t, os.WriteFile(filepath.Join(path, "next.go"), fmt.Appendf(nil, "package source\n// Fix %d\n", calls), 0o600))
			return firstOutput, nil
		},
	})
	require.ErrorContains(t, err, "max iterations")
	assert.Equal(t, 2, calls)
	require.NotEmpty(t, snapshot)
	assert.NoFileExists(t, snapshot)
	assert.NoDirExists(t, filepath.Dir(snapshot))
	assert.Empty(t, repo.Run("status", "--porcelain"))
}

func TestRefineHistoryDiscardsStaleReviewContext(t *testing.T) {
	for _, mode := range []string{"commit", "range", "three-dot-range", "unresolvable"} {
		t.Run(mode, func(t *testing.T) {
			repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
			base := repo.HeadSHA()
			initial := repo.CommitFile("next.go", "package source\n", "Add next source")
			d := newRefineHistoryDaemon(t, initial)
			d.staleRef = initial
			switch mode {
			case "range":
				d.staleRef = base + ".." + initial
			case "three-dot-range":
				d.staleRef = base + "..." + initial
			case "unresolvable":
				d.staleRef = "missing-review-ref"
			}
			var prompts []string
			err := runRefineHistoryTest(t, repo, base, 2, &agent.FakeAgent{
				NameStr: "refine-stale-history-test",
				ReviewFn: func(_ context.Context, path, _, p string, _ io.Writer) (string, error) {
					prompts = append(prompts, p)
					require.NoError(t, os.WriteFile(filepath.Join(path, "next.go"), fmt.Appendf(nil, "package source\n// Fix %d\n", len(prompts)), 0o600))
					return "First attempt must not accompany a stale review", nil
				},
			})
			require.ErrorContains(t, err, "max iterations")
			require.Len(t, prompts, 2)
			assert.Contains(t, prompts[0], "Preserve cancellation invariants")
			assert.NotContains(t, prompts[1], "Preserve cancellation invariants")
			assert.NotContains(t, prompts[1], "First attempt must not accompany a stale review")
			assert.Contains(t, prompts[1], "High: cancellation still races")
		})
	}
}
