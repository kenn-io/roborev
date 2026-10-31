package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func findingSummaryFixture(t *testing.T) (*Server, *storage.DB, storage.FindingKataSource) {
	t.Helper()
	s, db, dir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(dir)
	require.NoError(t, err)
	job := enqueueCompletedInsightsReviewJob(t, db, repo.ID, "base..head", "main", storage.JobTypeReview, failingInsightsOutput("Optional operation may need a retry"))
	// Existing helper emits structured review output.
	stats, err := db.GetFindingStats(storage.SummaryOptions{}, "severity")
	require.NoError(t, err)
	require.NotEmpty(t, stats.KataSources(), "job %d", job.ID)
	return s, db, stats.KataSources()[0]
}

func findingSummaryResult(source storage.FindingKataSource) storage.FindingKataReview {
	return storage.FindingKataReview{Source: source, Complete: true, ReadAt: time.Now(), Findings: []storage.FindingKataFinding{{Index: 0, RootUID: "root-a", VersionValid: true, Replies: []storage.FindingKataReply{{UID: "reply-a", TargetUID: "root-a", Author: "reviewer", Teammate: "worker", Kind: "refute", CreatedAt: time.Now(), Reason: "risk-accepted", ReasonState: "explicit", Evidence: strings.Repeat("e", 40), EvidenceValid: true}}}}}
}

func TestFindingSummaryAndInsightsKata(t *testing.T) {
	s, _, source := findingSummaryFixture(t)
	var calls atomic.Int32
	s.findingKataReader = func(ctx context.Context, got storage.FindingKataSource) (storage.FindingKataReview, error) {
		calls.Add(1)
		assert.Equal(t, source, got)
		return findingSummaryResult(got), nil
	}
	req := httptest.NewRequest(http.MethodGet, "/api/summary?findings=true&all=true", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"risk_accepted":1`)
	assert.Contains(t, w.Body.String(), `"state":"available"`)
	assert.EqualValues(t, 1, calls.Load())
	prompt, _, err := s.buildInsightsPrompt(context.Background(), sourceRepo(t, s, source), "main", time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Contains(t, prompt, `"Reason": "risk-accepted"`)
	assert.Contains(t, prompt, `"Author": "reviewer"`)
	assert.EqualValues(t, 2, calls.Load())
}

func sourceRepo(t *testing.T, s *Server, source storage.FindingKataSource) string {
	t.Helper()
	job, err := s.db.GetJobByID(source.JobID)
	require.NoError(t, err)
	return job.RepoPath
}

func TestFindingSummaryKataUnavailableAndRerun(t *testing.T) {
	for _, mode := range []string{"nil", "outage", "access", "cancel", "rerun", "document changed", "CI", "passing"} {
		t.Run(mode, func(t *testing.T) {
			s, db, source := findingSummaryFixture(t)
			calls := 0
			s.findingKataReader = func(ctx context.Context, got storage.FindingKataSource) (storage.FindingKataReview, error) {
				calls++
				switch mode {
				case "outage":
					return storage.FindingKataReview{}, errors.New("provider unavailable")
				case "access":
					return storage.FindingKataReview{}, ErrFindingKataAccessDenied
				case "rerun":
					_, err := db.Exec("UPDATE review_jobs SET status='queued' WHERE id=?", source.JobID)
					if err != nil {
						return storage.FindingKataReview{}, err
					}
				case "document changed":
					_, err := db.Exec("UPDATE reviews SET structured_output=structured_output || ' ' WHERE job_id=?", source.JobID)
					if err != nil {
						return storage.FindingKataReview{}, err
					}
				}
				return findingSummaryResult(got), nil
			}
			switch mode {
			case "nil":
				s.findingKataReader = nil
			case "CI":
				_, err := db.Exec("UPDATE review_jobs SET source='ci' WHERE id=?", source.JobID)
				require.NoError(t, err)
			case "passing":
				_, err := db.Exec("UPDATE reviews SET verdict_bool=1 WHERE job_id=?", source.JobID)
				require.NoError(t, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			stats, err := s.findingStats(ctx, storage.SummaryOptions{}, "severity")
			if mode == "access" || mode == "cancel" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Zero(t, stats.Total.Negative)
			if mode == "nil" || mode == "CI" || mode == "passing" {
				assert.Zero(t, calls)
			} else {
				assert.Equal(t, 1, calls)
			}
			if mode == "rerun" || mode == "document changed" {
				assert.Equal(t, 1, stats.Kata.Stale)
			}
			assert.Contains(t, stats.UnavailableTiers, "K")
		})
	}
}

func TestFindingKataReadBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var active, peak, calls atomic.Int32
		sources := []storage.FindingKataSource{{ReviewUUID: "a"}, {ReviewUUID: "b"}, {ReviewUUID: "c"}}
		reader := func(ctx context.Context, source storage.FindingKataSource) (storage.FindingKataReview, error) {
			calls.Add(1)
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			<-ctx.Done()
			return storage.FindingKataReview{}, ctx.Err()
		}
		started := time.Now()
		results, err := readFindingKata(context.Background(), sources, reader)
		require.NoError(t, err)
		a := assert.New(t)
		a.Equal(2*time.Second, time.Since(started))
		a.Len(results, 3)
		a.EqualValues(2, peak.Load())
		a.EqualValues(2, calls.Load())
		a.Zero(active.Load())
		for _, result := range results {
			a.True(result.Unavailable)
		}
	})
}

func TestHumaFindingSummaryOptions(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	testutil.CreateCompletedReview(t, db, repo.ID, "finding-sha", "test", "No issues found.")
	plain := serveHuma(t, srv, http.MethodGet, "/api/summary", nil)
	require.Equal(t, http.StatusOK, plain.Code)
	assert.NotContains(t, plain.Body.String(), `"findings"`)
	for _, query := range []string{"by=agent", "min_samples=5", "findings=true&by=unknown", "findings=true&min_samples=-1", "findings=true&min_samples=0", "findings=true&min_samples=bad"} {
		rr := serveHuma(t, srv, http.MethodGet, "/api/summary?"+query, nil)
		assert.Equal(t, http.StatusBadRequest, rr.Code, query)
	}
	rr := serveHuma(t, srv, http.MethodGet, "/api/summary?findings=true&by=agent&min_samples=1", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var s storage.Summary
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &s))
	require.NotNil(t, s.Findings)
	assert.Equal(t, "agent", s.Findings.GroupBy)
	assert.Equal(t, 1, s.Findings.LogicalReviews)
}

func TestFindingKataReadBindsRequestedSource(t *testing.T) {
	source := storage.FindingKataSource{JobID: 1, ReviewUUID: "source-a", Document: "document-a"}
	results, err := readFindingKata(context.Background(), []storage.FindingKataSource{source}, func(ctx context.Context, s storage.FindingKataSource) (storage.FindingKataReview, error) {
		return storage.FindingKataReview{Source: storage.FindingKataSource{JobID: 2, ReviewUUID: "source-b", Document: "document-b"}, Complete: true}, nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, source, results[0].Source)
	assert.True(t, results[0].Stale)
}
