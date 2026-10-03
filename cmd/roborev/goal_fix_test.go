package main

import (
	"io"
	"net/http"
	"net/http/httptest"
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
