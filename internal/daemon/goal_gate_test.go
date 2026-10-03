package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/testutil"
)

func TestGoalGateModes(t *testing.T) {
	for _, mode := range []string{"block", "warn", "off"} {
		t.Run(mode, func(t *testing.T) {
			server, _, _ := newTestServer(t)
			registerGoalReviewPi(t, server.configWatcher.Config())
			server.configWatcher.Config().GoalReview.KataGate.Default = new(mode)
			repo := testutil.NewGitRepo(t)
			goalArtifacts(t, repo.Path())
			require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("review_agent = \"pi\"\n[goal_review]\nenabled = false\n"), 0o600))
			server.goalReviewRunner = goalReviewTestRunner(func(_ context.Context, snapshot goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
				return goalreview.Check(snapshot), nil
			})
			// A mechanical finding survives the injected semantic result.
			require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), "docs/superpowers/plans/feature.md"), []byte("**Spec:** docs/superpowers/specs/feature-design.md\n### Task 1: Change\n"), 0o600))
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/goal-review", GoalGateRequest{RepoPath: repo.Path()})
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code, w.Body.String())
			var response GoalGateResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			assert.Equal(t, 1, response.Version)
			assert.Equal(t, mode, response.Mode)
			assert.Equal(t, mode == "block", response.Blocked)
			if mode == "off" {
				assert.Equal(t, "off", response.Status)
				assert.Empty(t, response.SnapshotID)
				assert.Empty(t, response.Findings)
			} else {
				assert.Equal(t, "findings", response.Status)
				require.Len(t, response.Findings, 1)
				assert.NotEmpty(t, response.SnapshotID)
			}
		})
	}
}

func TestGoalGatePreservesConfiguredModeForInvalidRepoConfig(t *testing.T) {
	for _, mode := range []string{"warn", "off"} {
		t.Run(mode, func(t *testing.T) {
			server, _, _ := newTestServer(t)
			server.configWatcher.Config().GoalReview.KataGate.Default = new(mode)
			repo := testutil.NewGitRepo(t)
			require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("review_agent = [\n"), 0o600))

			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/goal-review", GoalGateRequest{RepoPath: repo.Path()}))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var response GoalGateResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			assert.Equal(t, mode, response.Mode)
			assert.Equal(t, "error", response.Status)
			assert.False(t, response.Blocked)
			assert.Contains(t, response.Error, "unexpected EOF")
		})
	}
}

func TestGoalGateFailsClosedForCheckoutGateOverride(t *testing.T) {
	server, _, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("review_agent = \"pi\"\n[goal_review.kata_gate]\ndefault = \"off\"\n"), 0o600))
	runCount := 0
	server.goalReviewRunner = goalReviewTestRunner(func(_ context.Context, _ goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
		runCount++
		return []goalreview.Finding{{Severity: "medium", Message: "incomplete intent", Location: goalreview.Location{File: "spec.md"}}}, nil
	})

	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/goal-review", GoalGateRequest{RepoPath: repo.Path()}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response GoalGateResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Zero(t, runCount)
	assert.Equal(t, "block", response.Mode)
	assert.Equal(t, "error", response.Status)
	assert.True(t, response.Blocked)
	assert.Contains(t, response.Error, "global-only")
}

func TestGoalGateAcceptsPromptAboveInlineBudget(t *testing.T) {
	server, _, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	server.configWatcher.Config().DefaultMaxPromptSize = 1024
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())
	marker := "last complete requirement evidence"
	largeSpec := strings.Repeat("complete requirement evidence\n", 1000) + marker + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), "docs/superpowers/specs/feature-design.md"), []byte(largeSpec), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"),
		[]byte("review_agent = \"pi\"\n"), 0o600))
	server.goalReviewRunner = goalReviewTestRunner(func(_ context.Context, _ goalreview.Snapshot, prepared prompt.SnapshotResult) ([]goalreview.Finding, error) {
		complete := prepared.Prompt
		if prepared.FilePath != "" {
			data, readErr := os.ReadFile(prepared.FilePath)
			require.NoError(t, readErr)
			complete = string(data)
		}
		assert.Contains(t, complete, marker)
		return nil, nil
	})

	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/goal-review", GoalGateRequest{RepoPath: repo.Path()}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response GoalGateResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "pass", response.Status)
}

func TestGoalGateHonorsConfiguredJobTimeout(t *testing.T) {
	server, _, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	server.configWatcher.Config().JobTimeoutMinutes = 15
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("review_agent = \"pi\"\n"), 0o600))
	var deadline time.Time
	var hasDeadline bool
	server.goalReviewRunner = goalReviewTestRunner(func(ctx context.Context, _ goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
		deadline, hasDeadline = ctx.Deadline()
		return nil, nil
	})

	w := httptest.NewRecorder()
	started := time.Now()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/goal-review", GoalGateRequest{RepoPath: repo.Path()}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response GoalGateResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, "pass", response.Status)
	require.True(t, hasDeadline)
	assert.True(t, deadline.After(started.Add(10*time.Minute)), "agent context must honor the configured timeout beyond ten minutes")
}

func TestGoalGateRejectsBlankSelectionWhenOff(t *testing.T) {
	server, _, _ := newTestServer(t)
	server.configWatcher.Config().GoalReview.KataGate.Default = new("off")
	repo := testutil.NewGitRepo(t)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost,
		"/api/goal-review", GoalGateRequest{RepoPath: repo.Path(), SpecFile: new(" ")}))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestGoalGateCancelsAdmittedRequests(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			server, _, _ := newTestServer(t)
			registerGoalReviewPi(t, server.configWatcher.Config())
			repo := testutil.NewGitRepo(t)
			goalArtifacts(t, repo.Path())
			require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"),
				[]byte("review_agent = \"pi\"\n"), 0o600))
			started := make(chan struct{})
			server.goalReviewRunner = goalReviewTestRunner(func(ctx context.Context, _ goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/goal-review", GoalGateRequest{RepoPath: repo.Path()}).WithContext(ctx)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				server.httpServer.Handler.ServeHTTP(w, req)
			}()
			<-started
			require.Eventually(t, func() bool { return len(server.goalGate.slots) == 1 }, 5*time.Second, time.Millisecond)
			if shutdown {
				server.goalGate.stop()
			} else {
				cancel()
			}
			require.Eventually(t, func() bool {
				select {
				case <-done:
					return true
				default:
					return false
				}
			}, 5*time.Second, time.Millisecond)
			assert.Empty(t, server.goalGate.slots)
			var response GoalGateResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			assert.Equal(t, "error", response.Status)
			assert.True(t, response.Blocked)
		})
	}
}

func TestGoalGateErrorsAndAdmission(t *testing.T) {
	server, _, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("review_agent = \"test\"\n"), 0o600))
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/goal-review", GoalGateRequest{RepoPath: repo.Path()}))
	require.Equal(t, 200, w.Code, w.Body.String())
	var response GoalGateResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "error", response.Status)
	assert.True(t, response.Blocked)
	assert.NotEmpty(t, response.Error)
	for _, body := range []string{`{`, `{}`, `{"repo_path":"x","candidate":{}}`, `{"repo_path":"x","other":true}`} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/goal-review", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		server.httpServer.Handler.ServeHTTP(w, req)
		assert.Equal(t, 400, w.Code, w.Body.String())
		var response GoalGateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		assert.Equal(t, 1, response.Version)
		assert.Equal(t, "error", response.Status)
	}
	server.goalGate.stop()
	w = httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/goal-review", GoalGateRequest{RepoPath: repo.Path()}))
	assert.Equal(t, 503, w.Code, w.Body.String())
}

func TestGoalGateCapacityCancellation(t *testing.T) {
	gate := newGoalGate(1)
	release, err := gate.admit()
	require.NoError(t, err)
	_, err = gate.admit()
	require.Error(t, err)
	release()
	release, err = gate.admit()
	require.NoError(t, err)
	gate.cancel()
	require.ErrorIs(t, gate.ctx.Err(), context.Canceled)
	release()
	gate.stop()
	_, err = gate.admit()
	require.Error(t, err)
}

type goalGateRoundTripFunc func(*http.Request) (*http.Response, error)

func (f goalGateRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGoalGateClientContext(t *testing.T) {
	var receivedDeadline time.Time
	transport := goalGateRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		assert.Equal(t, "/api/goal-review", request.URL.Path)
		if deadline, ok := request.Context().Deadline(); ok {
			receivedDeadline = deadline
		}
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"version":1,"mode":"block","status":"pass","findings":[]}`)),
			Request:    request,
		}, nil
	})
	client := &HTTPClient{baseURL: "http://goal-gate.test", httpClient: &http.Client{Timeout: time.Millisecond, Transport: transport}}
	deadline := time.Now().Add(15 * time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	response, err := client.GoalReview(ctx, GoalGateRequest{RepoPath: "checkout"})
	require.NoError(t, err)
	assert.Equal(t, "pass", response.Status)
	assert.Equal(t, deadline, receivedDeadline)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.GoalReview(canceled, GoalGateRequest{RepoPath: "checkout"})
	require.ErrorIs(t, err, context.Canceled)
}

func TestGoalGateCandidateIsInMemory(t *testing.T) {
	server, db, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())
	require.NoError(t, os.Remove(filepath.Join(repo.Path(), "docs/superpowers/plans/feature.md")))
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("review_agent = \"pi\"\n"), 0o600))
	server.goalReviewRunner = goalReviewTestRunner(func(_ context.Context, _ goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
		return nil, nil
	})
	evaluate := func(candidate *goalreview.Candidate) GoalGateResponse {
		t.Helper()
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost,
			"/api/goal-review", GoalGateRequest{RepoPath: repo.Path(), Candidate: candidate}))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var response GoalGateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.Equal(t, "pass", response.Status)
		return response
	}
	before, err := os.ReadFile(filepath.Join(repo.Path(), "docs/superpowers/specs/feature-design.md"))
	require.NoError(t, err)
	base := evaluate(nil)
	proposed := evaluate(&goalreview.Candidate{Title: "Proposed task", Body: "Implement the requirement"})
	assert.NotEqual(t, base.SnapshotID, proposed.SnapshotID)
	assert.Equal(t, base.SnapshotID, evaluate(nil).SnapshotID, "candidate must not persist in subsequent captures")
	after, err := os.ReadFile(filepath.Join(repo.Path(), "docs/superpowers/specs/feature-design.md"))
	require.NoError(t, err)
	assert.Equal(t, before, after)
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	assert.Empty(t, jobs, "gate must not enqueue background jobs")
}

func TestGoalGateHTTPAdmissionAndBodyLimit(t *testing.T) {
	server, _, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())
	var releases []func()
	for range cap(server.goalGate.slots) {
		release, err := server.goalGate.admit()
		require.NoError(t, err)
		releases = append(releases, release)
	}
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost,
		"/api/goal-review", GoalGateRequest{RepoPath: repo.Path()}))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	var response GoalGateResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, 1, response.Version)
	assert.Equal(t, "error", response.Status)
	assert.True(t, response.Blocked)
}

func TestGoalGatePreservesCandidateLargerThanOneMiB(t *testing.T) {
	server, _, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("review_agent = \"pi\"\n"), 0o600))
	candidateBody := strings.Repeat("complete proposed requirement ", 40000) + "final requirement line"
	var capturedBody string
	server.goalReviewRunner = goalReviewTestRunner(func(_ context.Context, snapshot goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
		for _, issue := range snapshot.Issues {
			if issue.ShortID == "proposed" {
				capturedBody = issue.Body
			}
		}
		return nil, nil
	})
	request := GoalGateRequest{
		RepoPath:  repo.Path(),
		Candidate: &goalreview.Candidate{Title: "Proposed task", Body: candidateBody},
	}
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/goal-review", request))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response GoalGateResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "pass", response.Status)
	assert.Equal(t, candidateBody, capturedBody, "the review must receive the complete candidate body")
}
