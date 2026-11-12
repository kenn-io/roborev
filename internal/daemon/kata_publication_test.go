package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

type kataLatencyTransport struct {
	handler http.Handler
	started chan struct{}
}

func TestKataPublicationInspectSyncedReviewUsesDefaultProject(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "synced-review", "worker")
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail}))
	// Sync-only repositories store their remote identity as the root path.
	identity := "https://example.invalid/team/synthetic.git"
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE repos SET root_path = ?, identity = ? WHERE id = ?`, identity, identity, tc.Repo.ID)
	require.NoError(t, err)
	before, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	require.Nil(t, before.Marker)

	publisher := newKataPublisher(tc.DB, NewStaticConfig(kataConnectionConfig(fake)))
	status, err := publisher.Inspect(t.Context(), job.ID)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, "missing", status.State)
	assert.Equal(t, fake.Projects[0].UID, status.Project.UID)
	for _, req := range fake.AllRequests() {
		assert.Equal(t, http.MethodGet, req.Method)
	}
	after, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func (tr kataLatencyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case tr.started <- struct{}{}:
	default:
	}
	select {
	case <-time.After(4 * time.Second):
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	response := httptest.NewRecorder()
	if r.Body == nil {
		r.Body = http.NoBody
	}
	tr.handler.ServeHTTP(response, r)
	return response.Result(), nil
}

// This test is nonparallel: parallel tests cannot use the process-wide default
// transport until it has been restored. The fake socket is outside the bubble;
// the transport serves its handler in-process to make each delay virtual.
func TestKataPublicationEntryPathsAllowLongSequences(t *testing.T) {
	for _, entry := range []string{"direct", "completion hook", "manual", "startup"} {
		t.Run(entry, func(t *testing.T) {
			fake := katafake.New(t)
			tc := newWorkerTestContext(t, 1)
			job := tc.createAndClaimJob(t, "latency-review", "worker")
			cfg := kataConnectionConfig(fake)
			cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
			seed := storage.KataPublicationSeed{Endpoint: fake.URL(), Project: "reviews", Policy: "comments"}
			doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "first", Fix: "fix"}, {Severity: "high", Problem: "second", Fix: "fix"}}}
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
						fake.Fail(w, 400, "bad_request", "bad issue")
						return true
					}
					issue = fake.AddIssue(7, "latency", req.Title, *req.Body, req.Labels...)
					issue.Out.Metadata = req.Metadata
					fake.Reply(w, map[string]any{"issue": map[string]any{"uid": issue.Out.UID, "project_id": 7, "metadata": req.Metadata}})
					return true
				}
				var req gen.CommentRequestBody
				if err := json.UnmarshalRead(r.Body, &req); err != nil {
					fake.Fail(w, 400, "bad_request", "bad comment")
					return true
				}
				uid := fmt.Sprintf("root-%d", len(issue.Comments)+1)
				issue.Comments = append(issue.Comments, gen.CommentOut{UID: uid, Handle: &uid, Body: req.Body, Author: "service"})
				fake.Reply(w, map[string]any{"issue": map[string]any{"uid": issue.Out.UID}, "comment": map[string]any{"uid": uid, "body": req.Body}})
				return true
			}
			previous := http.DefaultTransport
			started := make(chan struct{}, 1)
			http.DefaultTransport = kataLatencyTransport{handler: fake.Handler(), started: started}
			t.Cleanup(func() { http.DefaultTransport = previous })
			synctest.Test(t, func(t *testing.T) {
				publisher := newKataPublisher(tc.DB, NewStaticConfig(cfg))
				runner := NewHookRunner(NewStaticConfig(cfg), tc.Broadcaster, log.New(io.Discard, "", 0), tc.DB)
				defer runner.Stop()
				start := time.Now()
				switch entry {
				case "direct":
					_, err = publisher.Publish(t.Context(), job.ID)
				case "completion hook":
					tc.Broadcaster.Broadcast(Event{Type: "review.completed", Verdict: "F", JobID: job.ID})
					runner.WaitUntilIdle()
				case "manual":
					_, err = runner.publishKataManual(t.Context(), job.ID, "")
				case "startup":
					runner.startKataRecovery()
					<-started // The startup goroutine has entered the transport.
					runner.WaitUntilIdle()
				}
				require.NoError(t, err)
				assert.Greater(t, time.Since(start), 30*time.Second)
				record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
				require.NoError(t, err)
				require.Equal(t, "complete", record.Marker.State)
				status, err := publisher.Inspect(t.Context(), job.ID)
				require.NoError(t, err)
				require.Equal(t, "complete", status.State)
				require.Len(t, status.Roots, 2)
				for _, root := range status.Roots {
					assert.Equal(t, "complete", root.State)
				}
				require.Len(t, issue.Comments, 2)
			})
		})
	}
}

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
	started := make(chan struct{})
	var startOnce sync.Once
	var watchStartup atomic.Bool
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			if watchStartup.Load() && r.URL.Path == "/api/v1/health" {
				startOnce.Do(func() { close(started) })
			}
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
	cfg.Hooks[0].Labels = []string{"changed"}
	_, err = publisher.Publish(t.Context(), job.ID)
	require.ErrorContains(t, err, "destination or request changed")
	cfg.Hooks[0].Labels = nil
	oldUID := fake.Projects[0].UID
	fake.Projects[0].UID = "different-project-uid"
	_, err = publisher.Publish(t.Context(), job.ID)
	require.ErrorContains(t, err, "destination or request changed")
	fake.Projects[0].UID = oldUID
	fake.Token = "rotated-token"
	cfg.Integrations.Kata.APIKey = "rotated-token"
	_, err = publisher.Publish(t.Context(), job.ID)
	require.Error(t, err)
	// A fresh runner's startup pass recovers the accepted root whose response
	// was lost. The fake uses a socket, so wait for its request barrier.
	runner := NewHookRunner(NewStaticConfig(cfg), tc.Broadcaster, log.New(io.Discard, "", 0), tc.DB)
	t.Cleanup(runner.Stop)
	watchStartup.Store(true)
	runner.startKataRecovery()
	testutil.ReceiveWithTimeout(t, started, 10*time.Second)
	runner.WaitUntilIdle()
	recovered, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, "complete", recovered.Marker.State)
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
	// Unsupported local and remote versions must stop an attempted write.
	marker := *record.Marker
	marker.Version = 2
	unsupported, err := json.Marshal(marker)
	require.NoError(t, err)
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET kata_publication = ? WHERE job_id = ?`, string(unsupported), job.ID)
	require.NoError(t, err)
	beforeVersion := len(fake.AllRequests())
	_, err = publisher.Publish(t.Context(), job.ID)
	require.Error(t, err)
	for _, req := range fake.AllRequests()[beforeVersion:] {
		assert.NotEqual(t, http.MethodPost, req.Method)
	}
	originalMarker, err := json.Marshal(record.Marker)
	require.NoError(t, err)
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET kata_publication = ? WHERE job_id = ?`, string(originalMarker), job.ID)
	require.NoError(t, err)
	issue.Out.Metadata["roborev.version"] = 2
	beforeVersion = len(fake.AllRequests())
	_, err = publisher.Publish(t.Context(), job.ID)
	require.Error(t, err)
	for _, req := range fake.AllRequests()[beforeVersion:] {
		assert.NotEqual(t, http.MethodPost, req.Method)
	}
	issue.Out.Metadata["roborev.version"] = 1
	// Reading uses the saved selection even after write authorization changes.
	cfg.Hooks[0].Findings = "summary"
	cfg.Hooks[0].Labels = []string{"changed"}
	cfg.Integrations.Kata.Teammate = "different"
	observed, err := publisher.Inspect(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "complete", observed.State)
	cfg.Hooks = nil
	cfg.Integrations.Kata.Teammate = "different"
	observed, err = publisher.Inspect(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "complete", observed.State)
	assert.Equal(t, "service", observed.Roots[0].Comment.Author)
	original := issue.Comments[0]
	saved, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	replacement := "different finding"
	digest := sha256.Sum256([]byte(replacement))
	issue.Comments[0].UID = "replacement-uid"
	issue.Comments[0].Body = fmt.Sprintf("%s\n\n<!-- roborev:v1 review=%s position=0 digest=%x -->", replacement, saved.ReviewUUID, digest)
	observed, err = publisher.Inspect(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "partial", observed.State)
	assert.Equal(t, "edited", observed.Roots[0].State)
	assert.Equal(t, "replacement-uid", observed.Roots[0].Comment.UID)
	issue.Comments[0] = original
	// Expiry does not suppress read-back of the pinned project.
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	completed := review.CreatedAt.Add(-8 * 24 * time.Hour)
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET created_at = ? WHERE job_id = ?`, completed.Format(time.RFC3339Nano), job.ID)
	require.NoError(t, err)
	issue.Out.Metadata["roborev.completed_at"] = completed.UTC().Format(time.RFC3339Nano)
	observed, err = publisher.Inspect(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "complete", observed.State)
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET created_at = ? WHERE job_id = ?`, review.CreatedAt.UTC().Format(time.RFC3339Nano), job.ID)
	require.NoError(t, err)
	issue.Out.Metadata["roborev.completed_at"] = review.CreatedAt.UTC().Format(time.RFC3339Nano)
	_, err = publisher.Publish(t.Context(), job.ID)
	require.Error(t, err)
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
	cfg.Integrations.Kata.Teammate = "roborev"
	// An edited root with a removed marker must never be mistaken for a
	// never-delivered root and posted again.
	issue.Comments[0].Body = "edited content without a marker"
	now := time.Now()
	issue.Comments[0].EditedAt = &now
	before := len(fake.AllRequests())
	observed, err = publisher.Publish(t.Context(), job.ID)
	require.ErrorContains(t, err, "ambiguous")
	require.Equal(t, "ambiguous", observed.Roots[0].State)
	for _, req := range fake.AllRequests()[before:] {
		assert.NotEqual(t, http.MethodPost, req.Method)
	}
}

func TestKataStartupRecoversDroppedWriteResponses(t *testing.T) {
	fake := katafake.New(t)
	// Serialize both the override and the fake's normal list/show handlers.
	var serverMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverMu.Lock()
		defer serverMu.Unlock()
		fake.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "dropped-response-review", "worker")
	cfg := kataConnectionConfig(fake)
	cfg.Integrations.Kata.Endpoint = server.URL
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
	seed := storage.KataPublicationSeed{Endpoint: server.URL, Project: "reviews", Policy: "comments"}
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail, KataPublication: &seed}))
	var issue *katafake.Issue
	started := make(chan struct{}, 2)
	var watchStartup atomic.Bool
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			w.Header().Set("Connection", "close") // Avoid transport retries after an accepted, dropped response.
			if r.URL.Path == "/api/v1/health" && watchStartup.Swap(false) {
				select {
				case started <- struct{}{}:
				default:
				}
			}
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
		} else {
			var req gen.CommentRequestBody
			if err := json.UnmarshalRead(r.Body, &req); err != nil {
				fake.Fail(w, 400, "bad_request", "bad request")
				return true
			}
			issue.Comments = append(issue.Comments, gen.CommentOut{UID: "synthetic-comment", Handle: new("c:synthetic"), Body: req.Body, Author: "service"})
		}
		// Accept the write but drop the response on the fake server's socket.
		conn, _, err := w.(http.Hijacker).Hijack()
		if assert.NoError(t, err) {
			_ = conn.Close()
		}
		return true
	}
	publisher := newKataPublisher(tc.DB, NewStaticConfig(cfg))
	_, err = publisher.Publish(t.Context(), job.ID)
	require.Error(t, err)
	func() {
		serverMu.Lock()
		defer serverMu.Unlock()
		require.NotNil(t, issue)
		require.Empty(t, issue.Comments)
	}()
	for attempt := range 2 {
		watchStartup.Store(true)
		runner := NewHookRunner(NewStaticConfig(cfg), tc.Broadcaster, log.New(io.Discard, "", 0), tc.DB)
		var stopOnce sync.Once
		stop := func() { stopOnce.Do(runner.Stop) }
		t.Cleanup(stop)
		runner.startKataRecovery()
		// The fake server uses a socket, so wait for its startup request.
		testutil.ReceiveWithTimeout(t, started, 10*time.Second)
		runner.WaitUntilIdle()
		stop()
		record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
		require.NoError(t, err)
		if attempt == 0 {
			require.Equal(t, "pending", record.Marker.State)
			func() {
				serverMu.Lock()
				defer serverMu.Unlock()
				require.Len(t, issue.Comments, 1)
			}()
		}
	}
	record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, "complete", record.Marker.State)
	func() {
		serverMu.Lock()
		defer serverMu.Unlock()
		require.Len(t, issue.Comments, 1)
	}()
	var posts int
	for _, req := range fake.AllRequests() {
		if req.Method == http.MethodPost {
			posts++
		}
	}
	assert.Equal(t, 2, posts)
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
	testutil.ReceiveWithTimeout(t, started, 10*time.Second)
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	testutil.ReceiveWithTimeout(t, done, 10*time.Second)
	testutil.ReceiveWithTimeout(t, canceled, 10*time.Second)
}

func TestKataCheckoutHonorsCancellation(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := kataCheckout(ctx, &storage.ReviewJob{RepoPath: tc.TmpDir, WorktreePath: tc.TmpDir})
	require.ErrorIs(t, err, context.Canceled)
}

type kataConfigFunc func() *config.Config

func (f kataConfigFunc) Config() *config.Config { return f() }

func TestKataPreparedAttemptChecksInputs(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "prepared-review", "worker")
	cfg := kataConnectionConfig(fake)
	priority := 2
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews", Labels: []string{"one"}, Priority: &priority}}
	seed := storage.KataPublicationSeed{Endpoint: fake.URL(), Project: "reviews", Policy: "comments"}
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail, KataPublication: &seed}))
	p := newKataPublisher(tc.DB, NewStaticConfig(cfg))
	attempt := &kataPreparedAttempt{}
	record, review, _, client, project, prepared, err := p.current(t.Context(), job.ID, attempt)
	require.NoError(t, err)
	client.CloseIdleConnections()
	_, _, _, client, _, again, err := p.current(t.Context(), job.ID, attempt)
	require.NoError(t, err)
	client.CloseIdleConnections()
	assert.Same(t, prepared, again)
	originalTime := review.CreatedAt.UTC().Format(time.RFC3339Nano)
	changedDoc := doc
	changedDoc.Summary = "changed"
	changedRaw, err := json.Marshal(changedDoc)
	require.NoError(t, err)
	for _, change := range []struct {
		name           string
		apply, restore func()
	}{
		{"teammate", func() { cfg.Integrations.Kata.Teammate = "other" }, func() { cfg.Integrations.Kata.Teammate = "roborev" }},
		{"label mutation", func() { cfg.Hooks[0].Labels[0] = "other" }, func() { cfg.Hooks[0].Labels[0] = "one" }},
		{"priority mutation", func() { priority = 3 }, func() { priority = 2 }},
		{"policy", func() { cfg.Hooks[0].Findings = "summary" }, func() { cfg.Hooks[0].Findings = "comments" }},
		{"hook removed", func() { cfg.Hooks[0].Event = "review.failed" }, func() { cfg.Hooks[0].Event = "review.completed" }},
		{"project", func() { cfg.Hooks[0].Project = "other" }, func() { cfg.Hooks[0].Project = "reviews" }},
		{"endpoint", func() { cfg.Integrations.Kata.Endpoint = "https://other.example.test" }, func() { cfg.Integrations.Kata.Endpoint = fake.URL() }},
		{"completion", func() {
			_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET created_at = ? WHERE job_id = ?`, review.CreatedAt.Add(time.Second).UTC().Format(time.RFC3339Nano), job.ID)
			require.NoError(t, err)
		}, func() {
			_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET created_at = ? WHERE job_id = ?`, originalTime, job.ID)
			require.NoError(t, err)
		}},
		{"source", func() {
			_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ? WHERE job_id = ?`, string(changedRaw), job.ID)
			require.NoError(t, err)
		}, func() {
			_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ? WHERE job_id = ?`, string(raw), job.ID)
			require.NoError(t, err)
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			change.apply()
			defer change.restore()
			_, _, _, _, _, _, err := p.current(t.Context(), job.ID, attempt)
			require.Error(t, err)
		})
	}
	// Recheck inputs after the pre-write read, inside the attempt lock.
	for _, change := range []struct {
		name           string
		apply, restore func()
	}{
		{"priority", func() { priority = 4 }, func() { priority = 2 }},
		{"labels", func() { cfg.Hooks[0].Labels[0] = "other" }, func() { cfg.Hooks[0].Labels[0] = "one" }},
		{"teammate", func() { cfg.Integrations.Kata.Teammate = "other" }, func() { cfg.Integrations.Kata.Teammate = "roborev" }},
		{"project", func() { cfg.Hooks[0].Project = "other" }, func() { cfg.Hooks[0].Project = "reviews" }},
		{"endpoint", func() { cfg.Integrations.Kata.Endpoint = "https://other.example.test" }, func() { cfg.Integrations.Kata.Endpoint = fake.URL() }},
		{"policy", func() { cfg.Hooks[0].Findings = "summary" }, func() { cfg.Hooks[0].Findings = "comments" }},
		{"completion", func() {
			_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET created_at = ? WHERE job_id = ?`, review.CreatedAt.Add(time.Second).UTC().Format(time.RFC3339Nano), job.ID)
			require.NoError(t, err)
		}, func() {
			_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET created_at = ? WHERE job_id = ?`, originalTime, job.ID)
			require.NoError(t, err)
		}},
		{"source", func() {
			_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ? WHERE job_id = ?`, string(changedRaw), job.ID)
			require.NoError(t, err)
		}, func() {
			_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ? WHERE job_id = ?`, string(raw), job.ID)
			require.NoError(t, err)
		}},
	} {
		t.Run("locked/"+change.name, func(t *testing.T) {
			calls := 0
			p.cfg = kataConfigFunc(func() *config.Config {
				calls++
				if calls == 2 {
					change.apply()
				}
				return cfg
			})
			defer change.restore()
			sent := false
			err := p.post(t.Context(), job.ID, record, review, project, prepared, attempt, func(context.Context, *kata.APIClient, kata.PublicationProject, *kata.Publication) error {
				sent = true
				return nil
			})
			require.Error(t, err)
			assert.False(t, sent)
		})
	}
	t.Run("mixed selectors resolve outside the write guard", func(t *testing.T) {
		fake := katafake.New(t)
		tc := newWorkerTestContext(t, 1)
		job := tc.createAndClaimJob(t, "mixed-selection", "worker")
		require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".kata.toml"), []byte("version = 1\n[project]\nname = \"reviews\"\n"), 0o600))
		binding, err := kata.WorkspaceBinding(t.Context(), tc.TmpDir)
		require.NoError(t, err)
		require.NotNil(t, binding.Alias)
		cfg := kataConnectionConfig(fake)
		cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}, {Type: "kata", Event: "review.completed", Findings: "comments"}}
		seed := storage.KataPublicationSeed{Endpoint: fake.URL(), Project: binding.Project, AliasKind: binding.Alias.Kind, AliasIdentity: binding.Alias.Identity, Policy: "comments"}
		require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail, KataPublication: &seed}))
		started, release := make(chan struct{}), make(chan struct{})
		var block atomic.Bool
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/api/v1/projects/resolve" {
				return false
			}
			if block.Load() {
				close(started)
				<-release
			}
			fake.Reply(w, gen.ProjectResolveBody{Project: fake.Projects[0]})
			return true
		}
		p := newKataPublisher(tc.DB, NewStaticConfig(cfg))
		attempt := &kataPreparedAttempt{}
		record, review, _, client, project, prepared, err := p.current(t.Context(), job.ID, attempt)
		require.NoError(t, err)
		client.CloseIdleConnections()
		other, err := tc.DB.GetOrCreateRepo(t.TempDir())
		require.NoError(t, err)
		block.Store(true)
		done := make(chan error, 1)
		finished := make(chan struct{})
		var sent atomic.Bool
		go func() {
			defer close(finished)
			done <- p.post(t.Context(), job.ID, record, review, project, prepared, attempt, func(context.Context, *kata.APIClient, kata.PublicationProject, *kata.Publication) error {
				sent.Store(true)
				return nil
			})
		}()
		t.Cleanup(func() { unblock(); <-finished })
		// Wait for the external fake HTTP request.
		testutil.ReceiveWithTimeout(t, started, 10*time.Second)
		deleted := make(chan error, 1)
		go func() { deleted <- tc.DB.DeleteRepoContext(context.WithoutCancel(t.Context()), other.ID, true) }()
		// Wait for the external SQLite attempt lock.
		require.NoError(t, testutil.ReceiveWithTimeout(t, deleted, 10*time.Second))
		cfg.Hooks[0].Project = "other"
		block.Store(false)
		unblock()
		require.ErrorIs(t, <-done, errKataSelectionChanged)
		assert.False(t, sent.Load())
		// The changed selector is resolved again outside the guard and rejected.
		_, _, _, _, _, _, err = p.current(t.Context(), job.ID, attempt)
		require.ErrorContains(t, err, "conflicting")
		cfg.Hooks[0].Project = "reviews"
		calls := 0
		p.cfg = kataConfigFunc(func() *config.Config {
			calls++
			if calls == 3 {
				cfg.Hooks[0].Project = "other"
			}
			return cfg
		})
		_, err = p.run(t.Context(), job.ID)
		require.ErrorContains(t, err, "conflicting")
		for _, request := range fake.Requests() {
			if strings.Contains(request.Path, "/issues") {
				assert.NotEqual(t, http.MethodPost, request.Method, "stale selection must not write")
			}
		}
		cfg.Hooks[0].Project = "reviews"
		p.cfg = NewStaticConfig(cfg)
		guardedRepo, err := tc.DB.GetOrCreateRepo(t.TempDir())
		require.NoError(t, err)
		err = p.post(t.Context(), job.ID, record, review, project, prepared, attempt, func(context.Context, *kata.APIClient, kata.PublicationProject, *kata.Publication) error {
			// Keep HTTP outside the bubble; only the attempt-lock waiter belongs to it.
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				deleted := make(chan error, 1)
				go func() { deleted <- tc.DB.DeleteRepoContext(ctx, guardedRepo.ID, true) }()
				synctest.Wait()
				assert.Empty(t, deleted, "exclusive mutation must wait for the protected send")
				cancel()
				require.ErrorIs(t, <-deleted, context.Canceled)
			})
			return nil
		})
		require.NoError(t, err)
		require.NoError(t, tc.DB.DeleteRepoContext(t.Context(), guardedRepo.ID, true))
	})
}

func TestKataPublicationPostDeadlineBoundary(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "deadline-review", "worker")
	cfg := kataConnectionConfig(fake)
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
	seed := storage.KataPublicationSeed{Endpoint: fake.URL(), Project: "reviews", Policy: "comments"}
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "first", Fix: "fix"}, {Severity: "high", Problem: "second", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail, KataPublication: &seed}))
	p := newKataPublisher(tc.DB, NewStaticConfig(cfg))
	attempt := &kataPreparedAttempt{}
	record, review, _, client, project, publication, err := p.current(t.Context(), job.ID, attempt)
	require.NoError(t, err)
	client.CloseIdleConnections()
	boundary := review.CreatedAt.Add(7*24*time.Hour - kata.DefaultRequestTimeout)
	for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		p.now = func() time.Time { return boundary.Add(delta) }
		sent := false
		err := p.post(t.Context(), job.ID, record, review, project, publication, attempt, func(context.Context, *kata.APIClient, kata.PublicationProject, *kata.Publication) error {
			sent = true
			return nil
		})
		if delta < 0 {
			require.NoError(t, err)
			assert.True(t, sent)
		} else {
			require.ErrorContains(t, err, "expired")
			assert.False(t, sent)
		}
	}
}

func TestKataPublicationExpiresBetweenRoots(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "expiry-between-roots", "worker")
	cfg := kataConnectionConfig(fake)
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
	seed := storage.KataPublicationSeed{Endpoint: fake.URL(), Project: "reviews", Policy: "comments"}
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "first", Fix: "fix"}, {Severity: "high", Problem: "second", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail, KataPublication: &seed}))
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	issue := fake.AddIssue(7, "expiry", "findings", "body")
	issue.Out.Metadata = map[string]any{"roborev.version": 1, "roborev.review_uuid": record.ReviewUUID.String(), "roborev.document_hash": record.DocumentHash, "roborev.policy": "comments", "roborev.completed_at": review.CreatedAt.UTC().Format(time.RFC3339Nano), "roborev.finding_count": 2}
	var firstSent atomic.Bool
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/comments") {
			return false
		}
		var req gen.CommentRequestBody
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
			return true
		}
		issue.Comments = append(issue.Comments, gen.CommentOut{UID: "first-root", Handle: new("c:first"), Body: req.Body, Author: "service"})
		firstSent.Store(true)
		fake.Reply(w, map[string]any{"issue": map[string]any{"uid": issue.Out.UID}, "comment": map[string]any{"uid": "first-root", "body": req.Body}})
		return true
	}
	p := newKataPublisher(tc.DB, NewStaticConfig(cfg))
	p.now = func() time.Time {
		if firstSent.Load() {
			return review.CreatedAt.Add(7*24*time.Hour - kata.DefaultRequestTimeout)
		}
		return review.CreatedAt
	}
	_, err = p.Publish(t.Context(), job.ID)
	require.ErrorContains(t, err, "expired")
	require.Len(t, issue.Comments, 1)
	var posts int
	for _, request := range fake.AllRequests() {
		if request.Method == http.MethodPost {
			posts++
		}
	}
	assert.Equal(t, 1, posts)
}

func TestKataCheckoutValidatesLinkedAndForeignWorktrees(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	linked := filepath.Join(t.TempDir(), "linked")
	out, err := exec.CommandContext(t.Context(), "git", "-C", tc.TmpDir, "worktree", "add", "--detach", linked, "HEAD").CombinedOutput()
	require.NoError(t, err, "%s", out)
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "git", "-C", tc.TmpDir, "worktree", "remove", "--force", linked).Run()
	})
	out, err = exec.CommandContext(t.Context(), "git", "-C", tc.TmpDir, "config", "core.worktree", tc.TmpDir).CombinedOutput()
	require.NoError(t, err, "%s", out)
	got, err := kataCheckout(t.Context(), &storage.ReviewJob{RepoPath: tc.TmpDir, WorktreePath: linked})
	require.NoError(t, err)
	assert.Equal(t, linked, got)
	subdir := filepath.Join(linked, "nested")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	got, err = kataCheckout(t.Context(), &storage.ReviewJob{RepoPath: tc.TmpDir, WorktreePath: subdir})
	require.NoError(t, err)
	assert.Equal(t, tc.TmpDir, got)
	foreign := newWorkerTestContext(t, 1)
	got, err = kataCheckout(t.Context(), &storage.ReviewJob{RepoPath: tc.TmpDir, WorktreePath: foreign.TmpDir})
	require.NoError(t, err)
	assert.Equal(t, tc.TmpDir, got)
}

func TestPassingKataReviewDoesNotSchedulePublication(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "passing-review", "worker")
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: testutil.ReviewFixtureJSON("No issues found.")}))
	cfg := kataConnectionConfig(fake)
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
	var logs bytes.Buffer
	runner := NewHookRunner(NewStaticConfig(cfg), tc.Broadcaster, log.New(&logs, "", 0), tc.DB)
	t.Cleanup(runner.Stop)
	tc.Broadcaster.Broadcast(Event{Type: "review.completed", Verdict: "P", JobID: job.ID})
	runner.WaitUntilIdle()
	assert.Empty(t, fake.AllRequests())
	assert.NotContains(t, logs.String(), "authorization")
	publisher := newKataPublisher(tc.DB, NewStaticConfig(cfg))
	_, err := publisher.Publish(t.Context(), job.ID)
	require.ErrorContains(t, err, "no local publication authorization")
	assert.Empty(t, fake.AllRequests())
}

func TestKataPublicationRechecksRemoteAfterRoot(t *testing.T) {
	for _, change := range []string{"edited", "duplicate", "closed", "truncated"} {
		t.Run(change, func(t *testing.T) {
			fake := katafake.New(t)
			tc := newWorkerTestContext(t, 1)
			job := tc.createAndClaimJob(t, "remote-changed", "worker")
			cfg := kataConnectionConfig(fake)
			cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
			seed := storage.KataPublicationSeed{Endpoint: fake.URL(), Project: "reviews", Policy: "comments"}
			doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "first", Fix: "fix"}, {Severity: "high", Problem: "second", Fix: "fix"}}}
			raw, err := json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail, KataPublication: &seed}))
			review, err := tc.DB.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
			require.NoError(t, err)
			issue := fake.AddIssue(7, "remote", "findings", "body")
			issue.Out.Metadata = map[string]any{"roborev.version": 1, "roborev.review_uuid": record.ReviewUUID.String(), "roborev.document_hash": record.DocumentHash, "roborev.policy": "comments", "roborev.completed_at": review.CreatedAt.UTC().Format(time.RFC3339Nano), "roborev.finding_count": 2}
			posts := 0
			fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if change == "truncated" && posts > 0 && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+issue.Out.UID) {
					fake.Reply(w, map[string]any{"issue": issue.Out, "comments": issue.Comments, "comments_truncated": true})
					return true
				}
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/comments") {
					return false
				}
				posts++
				var req gen.CommentRequestBody
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
					return true
				}
				body := req.Body
				issue.Comments = append(issue.Comments, gen.CommentOut{UID: "root-1", Handle: new("c:first"), Body: body, Author: "service"})
				switch change {
				case "edited":
					issue.Comments[0].Body = "edited"
				case "duplicate":
					fake.AddIssue(7, "duplicate", "findings", "body").Out.Metadata = issue.Out.Metadata
				case "closed":
					issue.Out.Status = "closed"
				}
				fake.Reply(w, map[string]any{"issue": map[string]any{"uid": issue.Out.UID}, "comment": map[string]any{"uid": "root-1", "body": body}})
				return true
			}
			publisher := newKataPublisher(tc.DB, NewStaticConfig(cfg))
			_, err = publisher.Publish(t.Context(), job.ID)
			require.Error(t, err)
			assert.Equal(t, 1, posts)
		})
	}
}

func TestKataPublicationAliasHintDoesNotChangeAuthorization(t *testing.T) {
	for _, savedHint := range []string{"old-name", ""} {
		t.Run(fmt.Sprintf("saved-hint=%q", savedHint), func(t *testing.T) {
			fake := katafake.New(t)
			tc := newWorkerTestContext(t, 1)
			job := tc.createAndClaimJob(t, "alias-hint", "worker")
			tc.GitRepo.RunGit("remote", "add", "origin", "https://example.test/org/original.git")
			writeHint := func(name string) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".kata.toml"), []byte("version = 1\n[project]\nname = \""+name+"\"\n"), 0o600))
			}
			writeHint("old-name")
			binding, err := kata.WorkspaceBinding(t.Context(), tc.TmpDir)
			require.NoError(t, err)
			require.NotNil(t, binding.Alias)
			cfg := kataConnectionConfig(fake)
			cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments"}}
			seed := storage.KataPublicationSeed{Endpoint: fake.URL(), Project: savedHint, AliasKind: binding.Alias.Kind, AliasIdentity: binding.Alias.Identity, Policy: "comments"}
			doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
			raw, err := json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail, KataPublication: &seed}))
			var resolved atomic.Int32
			fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/v1/projects/resolve" {
					return false
				}
				fake.Reply(w, gen.ProjectResolveBody{Project: fake.Projects[resolved.Load()]})
				return true
			}
			p := newKataPublisher(tc.DB, NewStaticConfig(cfg))
			attempt := &kataPreparedAttempt{}
			writeHint("renamed")
			record, review, _, client, project, prepared, err := p.current(t.Context(), job.ID, attempt)
			require.NoError(t, err)
			client.CloseIdleConnections()
			calls := 0
			p.cfg = kataConfigFunc(func() *config.Config {
				calls++
				if calls == 2 {
					writeHint("renamed-again")
				}
				return cfg
			})
			sent := false
			err = p.post(t.Context(), job.ID, record, review, project, prepared, attempt, func(context.Context, *kata.APIClient, kata.PublicationProject, *kata.Publication) error {
				sent = true
				return nil
			})
			require.NoError(t, err)
			assert.True(t, sent)
			p.cfg = NewStaticConfig(cfg)
			resolved.Store(1)
			sent = false
			err = p.post(t.Context(), job.ID, record, review, project, prepared, attempt, func(context.Context, *kata.APIClient, kata.PublicationProject, *kata.Publication) error {
				sent = true
				return nil
			})
			require.ErrorContains(t, err, "destination or request changed")
			assert.False(t, sent)
			resolved.Store(0)
			for _, change := range []struct {
				name           string
				apply, restore func()
			}{
				{"alias identity", func() { tc.GitRepo.RunGit("remote", "set-url", "origin", "https://example.test/org/other.git") }, func() { tc.GitRepo.RunGit("remote", "set-url", "origin", "https://example.test/org/original.git") }},
				{"alias kind", func() { tc.GitRepo.RunGit("remote", "remove", "origin") }, func() { tc.GitRepo.RunGit("remote", "add", "origin", "https://example.test/org/original.git") }},
				{"endpoint", func() { cfg.Integrations.Kata.Endpoint = "https://other.example.test" }, func() { cfg.Integrations.Kata.Endpoint = fake.URL() }},
				{"policy", func() { cfg.Hooks[0].Findings = "summary" }, func() { cfg.Hooks[0].Findings = "comments" }},
			} {
				t.Run(change.name, func(t *testing.T) {
					p.cfg = NewStaticConfig(cfg)
					change.apply()
					_, _, _, _, _, _, err = p.current(t.Context(), job.ID, attempt)
					require.Error(t, err)
					change.restore()
					calls := 0
					p.cfg = kataConfigFunc(func() *config.Config {
						calls++
						if calls == 2 {
							change.apply()
						}
						return cfg
					})
					defer change.restore()
					sent := false
					err := p.post(t.Context(), job.ID, record, review, project, prepared, attempt, func(context.Context, *kata.APIClient, kata.PublicationProject, *kata.Publication) error {
						sent = true
						return nil
					})
					require.Error(t, err)
					assert.False(t, sent)
				})
			}
		})
	}
}
