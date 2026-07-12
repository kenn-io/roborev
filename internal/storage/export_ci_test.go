package storage

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedPostedPanel(t *testing.T, db *DB, pr int, sha, outcome string) *CIPanel {
	t.Helper()
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	created, err := db.ReserveReviewAttempt("o/r", pr, sha, now)
	require.NoError(t, err)
	require.True(t, created)
	panel, _ := seedPanelRunForRepo(t, db, repo.ID, "o/r", pr, sha)
	require.NoError(t, db.MarkPanelPosted(panel.ID, outcome))
	got, err := db.GetCIPanelByPRSHA("o/r", pr, sha)
	require.NoError(t, err)
	return got
}

func TestExportCIMetricsIncludesOnlyPostedPanels(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	seedPostedPanel(t, db, 1, "sha-posted", PanelOutcomeReviewPosted)
	// An unposted panel must not export.
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo2"))
	seedPanelRunForRepo(t, db, repo.ID, "o/r", 2, "sha-unposted")

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1)
	p := page.Panels[0]
	assert.Equal(t, "o/r", p.GithubRepo)
	assert.Equal(t, int64(1), p.PRNumber)
	assert.Equal(t, "sha-posted", p.HeadSHA)
	assert.Equal(t, PanelOutcomeReviewPosted, p.Outcome)
	require.NotNil(t, p.FirstAttemptAt)
	require.NotNil(t, p.AttemptCount)
	assert.Equal(t, int64(1), *p.AttemptCount)
	assert.NotEmpty(t, p.PostedAt)
	assert.NotEmpty(t, p.PanelCreatedAt)
	assert.NotEmpty(t, p.Jobs, "panel jobs must be included")
	for _, j := range p.Jobs {
		assert.NotEmpty(t, j.JobUUID)
		assert.Contains(t, []string{"member", "synthesis"}, j.Role)
		assert.NotEmpty(t, j.Agent)
		assert.NotEmpty(t, j.Status)
	}
	assert.False(t, page.Truncated)
	require.NotNil(t, page.NextCursor)
}

func TestExportCIMetricsLegacyRowsAreUnknown(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	panel := seedPostedPanel(t, db, 3, "sha-legacy", PanelOutcomeReviewPosted)
	// Simulate a row finalized before outcome persistence existed.
	_, err := db.Exec(`UPDATE ci_pr_panels
		SET outcome = NULL, first_attempt_at = NULL, attempt_count = NULL
		WHERE id = ?`, panel.ID)
	require.NoError(t, err)

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1)
	assert.Equal(t, PanelOutcomeUnknown, page.Panels[0].Outcome)
	assert.Nil(t, page.Panels[0].FirstAttemptAt)
	assert.Nil(t, page.Panels[0].AttemptCount)
}

func TestExportCIMetricsCursorPagination(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	seedPostedPanel(t, db, 10, "sha-a", PanelOutcomeReviewPosted)
	seedPostedPanel(t, db, 11, "sha-b", PanelOutcomeGiveupPosted)

	first, err := db.ExportCIMetrics(ExportCIMetricsOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, first.Panels, 1)
	assert.True(t, first.Truncated)
	require.NotNil(t, first.NextCursor)

	second, err := db.ExportCIMetrics(ExportCIMetricsOptions{Cursor: *first.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Panels, 1)
	assert.False(t, second.Truncated)
	assert.NotEqual(t, first.Panels[0].HeadSHA, second.Panels[0].HeadSHA)
}

func TestExportCIMetricsRejectsCursorFromDifferentDatabase(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	seedPostedPanel(t, db, 20, "sha-c", PanelOutcomeReviewPosted)
	raw, err := json.Marshal(ciMetricsCursor{
		Version:    ciMetricsCursorVersion,
		DatabaseID: "some-other-database",
		PostedAt:   "2026-07-01T00:00:00Z",
		PanelID:    1,
	})
	require.NoError(t, err)
	cursor := base64.RawURLEncoding.EncodeToString(raw)

	_, err = db.ExportCIMetrics(ExportCIMetricsOptions{Cursor: cursor})
	require.ErrorIs(t, err, ErrExportCursorDatabaseMismatch)
}
