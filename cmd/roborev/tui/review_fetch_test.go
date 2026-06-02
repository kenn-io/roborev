package tui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestTUIFetchReviewNotFound(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	cmd := m.fetchReview(999)
	msg := cmd()

	errMsg, ok := msg.(errMsg)
	assert.True(t, ok)
	assert.Equal(t, "no review found", errMsg.Error())
}

func TestTUIFetchReviewServerError(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	cmd := m.fetchReview(1)
	msg := cmd()

	errMsg, ok := msg.(errMsg)
	assert.True(t, ok)
	assert.Equal(t, "fetch review: 500 Internal Server Error", errMsg.Error())
}

func TestTUIFetchReviewFallbackSHAResponses(t *testing.T) {
	// Test that when job_id responses are empty, TUI falls back to SHA-based responses
	requestedPaths := []string{}
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.String())

		if r.URL.Path == "/api/review" {
			// Return a review for a single commit (not a range or dirty)
			review := storage.Review{
				ID:     1,
				JobID:  42,
				Agent:  "test",
				Output: "No issues found.",
				Job: &storage.ReviewJob{
					ID:       42,
					GitRef:   "abc123def456", // Single commit SHA (not a range)
					RepoPath: "/test/repo",
				},
			}
			json.NewEncoder(w).Encode(review)
			return
		}

		if r.URL.Path == "/api/comments" {
			jobID := r.URL.Query().Get("job_id")
			sha := r.URL.Query().Get("sha")

			if jobID != "" {
				// Job ID query returns empty responses
				json.NewEncoder(w).Encode(map[string]any{
					"responses": []storage.Response{},
				})
				return
			}
			if sha != "" {
				// SHA fallback query returns legacy responses
				json.NewEncoder(w).Encode(map[string]any{
					"responses": []storage.Response{
						{ID: 1, Responder: "user", Response: "Legacy response from SHA lookup"},
					},
				})
				return
			}
		}

		w.WriteHeader(http.StatusNotFound)
	})
	cmd := m.fetchReview(42)
	msg := cmd()

	reviewMsg, ok := msg.(reviewMsg)
	assert.True(t, ok)

	// Should have fetched both job_id and sha responses
	foundJobIDRequest := false
	foundSHARequest := false
	for _, path := range requestedPaths {
		if strings.Contains(path, "job_id=42") {
			foundJobIDRequest = true
		}
		if strings.Contains(path, "sha=abc123def456") {
			foundSHARequest = true
		}
	}

	assert.True(t, foundJobIDRequest)
	assert.True(t, foundSHARequest)

	// Should have the legacy response from SHA fallback
	assert.Len(t, reviewMsg.responses, 1)
	assert.Equal(t, "Legacy response from SHA lookup", reviewMsg.responses[0].Response)
}

func TestTUIFetchReviewNoFallbackForRangeReview(t *testing.T) {
	// Test that SHA fallback is NOT used for range reviews (abc..def format)
	requestedPaths := []string{}
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.String())

		if r.URL.Path == "/api/review" {
			// Return a review for a commit range (not a single commit)
			review := storage.Review{
				ID:     1,
				JobID:  42,
				Agent:  "test",
				Output: "No issues found.",
				Job: &storage.ReviewJob{
					ID:       42,
					GitRef:   "abc123..def456", // Range review
					RepoPath: "/test/repo",
				},
			}
			json.NewEncoder(w).Encode(review)
			return
		}

		if r.URL.Path == "/api/comments" {
			// Return empty responses for job_id
			json.NewEncoder(w).Encode(map[string]any{
				"responses": []storage.Response{},
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	})
	cmd := m.fetchReview(42)
	msg := cmd()

	_, ok := msg.(reviewMsg)
	assert.True(t, ok)

	// Should NOT have made a SHA fallback request for range review
	for _, path := range requestedPaths {
		assert.NotContains(t, path, "sha=")
	}
}

func TestTUIFetchReviewNoFallbackForDirtyReviewWithCommitID(t *testing.T) {
	requestedPaths := []string{}
	commitID := int64(42)
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.String())

		if r.URL.Path == "/api/review" {
			review := storage.Review{
				ID:     1,
				JobID:  42,
				Agent:  "test",
				Output: "Dirty review output",
				Job: &storage.ReviewJob{
					ID:       42,
					CommitID: &commitID,
					GitRef:   "dirty",
					JobType:  storage.JobTypeDirty,
					RepoPath: "/test/repo",
				},
			}
			json.NewEncoder(w).Encode(review)
			return
		}

		if r.URL.Path == "/api/comments" {
			if r.URL.Query().Get("commit_id") != "" {
				json.NewEncoder(w).Encode(map[string]any{
					"responses": []storage.Response{
						{ID: 2, Responder: "bob", Response: "base commit comment"},
					},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"responses": []storage.Response{
					{ID: 1, Responder: "alice", Response: "dirty job comment"},
				},
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	})
	cmd := m.fetchReview(42)
	msg := cmd()

	reviewMsg, ok := msg.(reviewMsg)
	assert.True(t, ok)

	for _, path := range requestedPaths {
		assert.NotContains(t, path, "commit_id=42")
		assert.NotContains(t, path, "sha=dirty")
	}
	require.Len(t, reviewMsg.responses, 1)
	assert.Equal(t, "dirty job comment", reviewMsg.responses[0].Response)
}
