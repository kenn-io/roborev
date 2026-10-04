package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

func TestGoalReviewIsNotCodeFixCandidate(t *testing.T) {
	failed := "F"
	assert.False(t, isFixCandidateJob(storage.ReviewJob{JobType: storage.JobTypeGoalReview, Verdict: &failed}))
}

func TestFixSingleGoalReviewRejected(t *testing.T) {
	var reviews atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 99, Status: storage.JobStatusDone, Agent: "test", JobType: storage.JobTypeGoalReview}}})
		case "/api/review":
			reviews.Add(1)
			http.Error(w, "unexpected review fetch", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	patchServerAddr(t, ts.URL)
	cmd, _ := newTestCmd(t)
	tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
	err := fixSingleJob(cmd, t.TempDir(), 99, fixOptions{agentName: "test"}, tracker)
	require.ErrorContains(t, err, "goal reviews")
	assert.Zero(t, reviews.Load(), "reject intent reviews before entering the code-fix flow")
}

func TestFixBatchRejectsGoalReviewsBeforeProcessing(t *testing.T) {
	for _, verdict := range []string{"P", "F"} {
		for _, tc := range []struct {
			name string
			ids  []int64
		}{
			{"goal only", []int64{99}},
			{"code then goal", []int64{98, 99}},
			{"goal then code", []int64{99, 98}},
		} {
			t.Run(verdict+"/"+tc.name, func(t *testing.T) {
				assert := assert.New(t)
				repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
				var reviews, mutations atomic.Int64
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/jobs":
						id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
						assert.NoError(err)
						job := storage.ReviewJob{ID: id, Status: storage.JobStatusDone, Agent: "test", JobType: storage.JobTypeReview, Verdict: new("P")}
						if id == 99 {
							job.JobType = storage.JobTypeGoalReview
							job.Verdict = &verdict
						}
						writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{job}})
					case "/api/review":
						reviews.Add(1)
						review := storage.Review{Output: "No issues found.", VerdictBool: new(1)}
						if r.URL.Query().Get("job_id") == "99" && verdict == "F" {
							review.Output = "- medium: Missing acceptance criteria"
							review.VerdictBool = new(0)
						}
						writeJSON(w, review)
					case "/api/comments":
						writeJSON(w, map[string]any{"responses": []storage.Response{}})
					case "/api/comment", "/api/review/close":
						mutations.Add(1)
						writeJSON(w, map[string]any{})
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(ts.Close)
				patchServerAddr(t, ts.URL)
				cmd, _ := newTestCmd(t)
				tester := agent.NewTestAgent()
				roots := currentRepoRoots{worktreeRoot: repo.Dir, mainRepoRoot: repo.Dir}
				err := processFixBatch(context.Background(), cmd, roots, tc.ids, 0,
					fixOptions{agentName: "test", quiet: true},
					&fixSessionTracker{base: tester, out: io.Discard})
				require.ErrorContains(t, err, "goal reviews cannot be fixed as code")
				assert.Zero(reviews.Load())
				assert.Zero(mutations.Load())
				assert.Empty(tester.Calls())
			})
		}
	}
}
