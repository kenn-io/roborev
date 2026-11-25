package daemon

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

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
