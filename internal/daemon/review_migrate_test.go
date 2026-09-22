package daemon

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

func TestMigrateReviewEndpoint(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(t, db, repo.ID, "legacy-head", "test", "No issues found.")
	raw := `{"schema_version":0,"legacy":{"markdown":"The write loses data.","recorded_verdict":false}}`
	_, err := db.Exec(`UPDATE reviews SET structured_output = ?, closed = 1, verdict_bool = 0 WHERE job_id = ?`, raw, job.ID)
	require.NoError(t, err)
	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	for _, path := range []string{"/api/review?job_id=%d", "/api/ui/review-projection?job_id=%d"} {
		response := serveHuma(t, srv, http.MethodGet, fmt.Sprintf(path, job.ID), nil)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "Unstructured historical review")
		assert.Contains(t, response.Body.String(), "The write loses data.")
		assert.NotContains(t, response.Body.String(), "No issues found")
	}
	invalid := []byte(fmt.Sprintf(`{"review_id":%d,"document":{"schema_version":2,"summary":"Incomplete"}}`, review.ID))
	rejected := serveHuma(t, srv, http.MethodPost, "/api/review/migrate", invalid)
	require.Equal(t, http.StatusBadRequest, rejected.Code, rejected.Body.String())
	unchanged, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, review.Output, unchanged.Output)
	body := []byte(fmt.Sprintf(`{"review_id":%d,"document":{"schema_version":1,"summary":"Converted.","findings":[{"severity":"high","problem":"The write loses data.","fix":"Keep the old file until rename.","location":null}]}}`, review.ID))
	accepted := serveHuma(t, srv, http.MethodPost, "/api/review/migrate", body)
	require.Equal(t, http.StatusOK, accepted.Code, accepted.Body.String())
	converted, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, review.ID, converted.ID)
	assert.True(t, converted.Closed)
	assert.Contains(t, converted.Output, "Keep the old file until rename.")
	conflict := serveHuma(t, srv, http.MethodPost, "/api/review/migrate", body)
	assert.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
}
