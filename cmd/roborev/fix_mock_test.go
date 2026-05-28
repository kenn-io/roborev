package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestMockDaemonBuilderMultipleReviews(t *testing.T) {
	// This test verifies that multiple calls to WithReview for different job IDs
	// are correctly stored and retrieved by the mock server.
	builder := newMockDaemonBuilder(t).
		WithReview(10, "Review for Job 10").
		WithReview(20, "Review for Job 20").
		WithReview(30, "Review for Job 30")

	ts := builder.Build()

	tests := []struct {
		name       string
		query      string
		wantJobID  int64
		wantOutput string
		wantStatus int
	}{
		{"Valid-10", "job_id=10", 10, "Review for Job 10", http.StatusOK},
		{"Valid-20", "job_id=20", 20, "Review for Job 20", http.StatusOK},
		{"Valid-30", "job_id=30", 30, "Review for Job 30", http.StatusOK},
		{"NotFound-40", "job_id=40", 0, "", http.StatusNotFound},
		{"Invalid-abc", "job_id=abc", 0, "", http.StatusBadRequest},
		{"PartialNumeric", "job_id=10abc", 0, "", http.StatusBadRequest},
		{"Missing", "", 0, "", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(fmt.Sprintf("%s/api/review", ts.URL))
			require.NoError(t, err, "failed to parse base URL: %v")

			if tt.query != "" {
				u.RawQuery = tt.query
			}
			resp, err := http.Get(u.String())
			require.NoError(t, err, "failed to make request: %v")

			defer resp.Body.Close()

			assert.Equal(t, tt.wantStatus, resp.StatusCode)

			if tt.wantStatus != http.StatusOK {
				return
			}

			var review storage.Review
			if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
				require.NoError(t, err, "failed to decode response: %v")
			}

			assert.Equal(t, tt.wantJobID, review.JobID)
			assert.Equal(t, tt.wantOutput, review.Output)
		})
	}
}
