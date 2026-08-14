package daemon

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestWebAnalyticsNormalizesFilters(t *testing.T) {
	server, db, tempDir := newTestServer(t)
	repo, jobs := seedRepoWithJobs(t, db, filepath.Join(tempDir, "project-a"), 1, "project-a")
	job := jobs[0]
	base := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	_, err := db.Exec(`UPDATE review_jobs SET status='done', source='post_commit', agent='agent-a', model='model-a',
		enqueued_at=?, started_at=?, finished_at=?, agent_invoked=1,
		token_usage='{"cost_usd":0.5,"has_cost":true}' WHERE id=?`,
		base.Format(time.RFC3339), base.Add(time.Minute).Format(time.RFC3339),
		base.Add(2*time.Minute).Format(time.RFC3339), job.ID)
	require.NoError(t, err)

	query := url.Values{}
	query.Set("since", base.Format(time.RFC3339))
	query.Set("until", base.Add(time.Hour).Format(time.RFC3339))
	query.Add("project", repo.Name)
	query.Add("project", "unused-project")
	query.Add("source", storage.JobSourcePostCommit)
	query.Set("agent", "agent-a")
	query.Set("model", "model-a")
	query.Set("bucket", "hour")
	response := serveHuma(t, server, http.MethodGet, "/api/ui/analytics?"+query.Encode(), nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var snapshot storage.AnalyticsSnapshot
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &snapshot))
	assert.Equal(t, storage.AnalyticsSchemaVersion, snapshot.SchemaVersion)
	require.NotNil(t, snapshot.Filters.Since)
	assert.Equal(t, base, *snapshot.Filters.Since)
	assert.Equal(t, base.Add(time.Hour), snapshot.Filters.Until)
	assert.Equal(t, []string{repo.Name, "unused-project"}, snapshot.Filters.Projects)
	assert.Equal(t, []string{storage.JobSourcePostCommit}, snapshot.Filters.Sources)
	assert.Equal(t, []string{"agent-a"}, snapshot.Filters.Agents)
	assert.Equal(t, storage.AnalyticsBucketHour, snapshot.Filters.Bucket)
	assert.Equal(t, 1, snapshot.Summary.Reviews.Total)
	assert.Equal(t, 1, snapshot.Summary.Attempts.Eligible)
}

func TestWebAnalyticsDefaultsToThirtyDays(t *testing.T) {
	server, _, _ := newTestServer(t)
	before := time.Now().UTC()
	response := serveHuma(t, server, http.MethodGet, "/api/ui/analytics", nil)
	after := time.Now().UTC()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var snapshot storage.AnalyticsSnapshot
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &snapshot))
	require.NotNil(t, snapshot.Filters.Since)
	assert.WithinDuration(t, before.Add(-30*24*time.Hour), *snapshot.Filters.Since, time.Second)
	assert.WithinDuration(t, after, snapshot.Filters.Until, time.Second)
	assert.Equal(t, storage.AnalyticsBucketDay, snapshot.Filters.Bucket)
}

func TestWebAnalyticsRejectsInvalidBoundsAndBucket(t *testing.T) {
	server, _, _ := newTestServer(t)
	queries := []string{
		"since=yesterday",
		"until=tomorrow",
		"since=2026-08-02T00%3A00%3A00Z&until=2026-08-01T00%3A00%3A00Z",
		"bucket=quarter",
	}
	for _, query := range queries {
		response := serveHuma(t, server, http.MethodGet, "/api/ui/analytics?"+query, nil)
		assert.Equal(t, http.StatusBadRequest, response.Code, query)
	}
}

func TestWebAnalyticsRejectsExcessiveTimeBuckets(t *testing.T) {
	server, _, _ := newTestServer(t)
	since := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add((storage.MaxAnalyticsTimeBuckets + 1) * time.Hour)
	query := url.Values{
		"since":  {since.Format(time.RFC3339)},
		"until":  {until.Format(time.RFC3339)},
		"bucket": {"hour"},
	}

	response := serveHuma(t, server, http.MethodGet,
		"/api/ui/analytics?"+query.Encode(), nil)
	assert.Equal(t, http.StatusBadRequest, response.Code)
}

func TestWebAnalyticsExplicitUntilWithoutSinceIsAllTime(t *testing.T) {
	server, _, _ := newTestServer(t)
	until := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	response := serveHuma(t, server, http.MethodGet,
		"/api/ui/analytics?until="+url.QueryEscape(until.Format(time.RFC3339)), nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var snapshot storage.AnalyticsSnapshot
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &snapshot))
	assert.Nil(t, snapshot.Filters.Since)
	assert.Equal(t, storage.AnalyticsBucketMonth, snapshot.Filters.Bucket)
}

func TestWebAnalyticsReportsStorageFailureForValidBucket(t *testing.T) {
	server, db, _ := newTestServer(t)
	_, err := db.Exec(`DROP TABLE review_jobs`)
	require.NoError(t, err)

	response := serveHuma(t, server, http.MethodGet,
		"/api/ui/analytics?bucket=hour", nil)
	assert.Equal(t, http.StatusInternalServerError, response.Code)
}
