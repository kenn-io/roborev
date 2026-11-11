package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestKataPublicationExpiredNeverCreatesIssue(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "expired-review", "worker")
	cfg := kataConnectionConfig(fake)
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
	seed := storage.KataPublicationSeed{Endpoint: fake.URL(), Project: "reviews", Policy: "comments"}
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail, KataPublication: &seed}))
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	publisher := newKataPublisher(tc.DB, NewStaticConfig(cfg))
	publisher.now = func() time.Time { return review.CreatedAt.Add(7*24*time.Hour - kata.DefaultRequestTimeout) }
	status, err := publisher.Publish(t.Context(), job.ID)
	require.ErrorContains(t, err, "expired")
	require.NotNil(t, status)
	for _, req := range fake.AllRequests() {
		assert.NotEqual(t, http.MethodPost, req.Method)
	}
	record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Empty(t, record.Marker.ProjectUID)
	publisher.now = func() time.Time { return review.CreatedAt }
	cfg.Hooks[0].Findings = "summary"
	_, err = publisher.Publish(t.Context(), job.ID)
	require.ErrorContains(t, err, "authorization changed")
	for _, req := range fake.AllRequests() {
		assert.NotEqual(t, http.MethodPost, req.Method)
	}
}

func TestKataPublicationLockIsCancelable(t *testing.T) {
	publisher := newKataPublisher(nil, nil)
	id := testUUID("kata-publication-lock")
	unlock, err := publisher.lock(t.Context(), id)
	require.NoError(t, err)
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = publisher.lock(ctx, id)
	require.ErrorIs(t, err, context.Canceled)
}

func TestKataPublicationRecoversLostResponses(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "recovery-review", "worker")
	cfg := kataConnectionConfig(fake)
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
	seed := storage.KataPublicationSeed{Endpoint: fake.URL(), Project: "reviews", Policy: "comments"}
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail, KataPublication: &seed}))
	var issue *katafake.Issue
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}
		if issue == nil {
			var req gen.CreateIssueRequestBody
			if err := json.UnmarshalRead(r.Body, &req); err != nil {
				fake.Fail(w, 400, "bad_request", "bad request")
				return true
			}
			issue = fake.AddIssue(7, "synthetic", req.Title, *req.Body, req.Labels...)
			issue.Out.Metadata = req.Metadata
			fake.Fail(w, 503, "unavailable", "lost issue response")
			return true
		}
		var req gen.CommentRequestBody
		if err := json.UnmarshalRead(r.Body, &req); err != nil {
			fake.Fail(w, 400, "bad_request", "bad request")
			return true
		}
		issue.Comments = append(issue.Comments, gen.CommentOut{UID: "synthetic-comment", Handle: new("c:synthetic"), Body: req.Body, Author: "service"})
		fake.Fail(w, 503, "unavailable", "lost comment response")
		return true
	}
	publisher := newKataPublisher(tc.DB, NewStaticConfig(cfg))
	_, err = publisher.Publish(t.Context(), job.ID)
	require.Error(t, err)
	_, err = publisher.Publish(t.Context(), job.ID)
	require.Error(t, err)
	status, err := publisher.Publish(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "complete", status.State)
	require.Len(t, status.Roots, 1)
	assert.Equal(t, "service", status.Roots[0].Comment.Author)
	var posts int
	for _, req := range fake.AllRequests() {
		if req.Method == http.MethodPost {
			posts++
		}
	}
	assert.Equal(t, 2, posts)
	record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "complete", record.Marker.State)
}

func TestKataRecoveryStopsInFlightAttempt(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "shutdown-review", "worker")
	cfg := kataConnectionConfig(fake)
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
	seed := storage.KataPublicationSeed{Endpoint: fake.URL(), Project: "reviews", Policy: "comments"}
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail, KataPublication: &seed}))
	started, canceled := make(chan struct{}), make(chan struct{})
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/health" {
			return false
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
		return true
	}
	runner := NewHookRunner(NewStaticConfig(cfg), tc.Broadcaster, log.New(io.Discard, "", 0), tc.DB)
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(runner.Stop) }
	t.Cleanup(stop)
	runner.startKataRecovery()
	// The fake server uses an external socket, so the test waits on its barriers.
	wait := func(ch <-chan struct{}) error {
		select {
		case <-ch:
			return nil
		case <-time.After(10 * time.Second):
			return errors.New("socket barrier timed out")
		}
	}
	require.NoError(t, wait(started))
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	require.NoError(t, wait(done))
	require.NoError(t, wait(canceled))
}
