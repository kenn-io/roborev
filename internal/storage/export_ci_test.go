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

// TestExportCIMetricsSurvivesCascadeRepoDeletion covers the snapshot fix: a
// panel's synthesis_agent/synthesis_model are stamped onto ci_pr_panels at
// MarkPanelPosted time, so ExportCIMetrics still reports them (via the
// COALESCE(NULLIF(...)) fallback preferring the snapshot) after the repo and
// its review_jobs rows are cascade-deleted. Jobs, which is sourced live from
// review_jobs by panel_run_uuid, is expected to come back empty in that case.
func TestExportCIMetricsSurvivesCascadeRepoDeletion(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	created, err := db.ReserveReviewAttempt("o/r", 42, "sha-cascade", now)
	require.NoError(t, err)
	require.True(t, created)

	members := []EnqueueOpts{{
		RepoID: repo.ID, GitRef: "b..h", Agent: "test-member", PanelMemberIndex: 0,
	}}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "b..h", Agent: "test-synth", Model: "test-model"}
	runCreated, _, _, err := db.CreateCIPanelRun("o/r", 42, "sha-cascade", members, synthesis)
	require.NoError(t, err)
	require.True(t, runCreated)

	panel, err := db.GetCIPanelByPRSHA("o/r", 42, "sha-cascade")
	require.NoError(t, err)
	require.NoError(t, db.MarkPanelPosted(panel.ID, PanelOutcomeReviewPosted))

	require.NoError(t, db.DeleteRepo(repo.ID, true))

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1)
	p := page.Panels[0]
	require.NotNil(t, p.SynthesisAgent, "synthesis_agent survives from the panel-row snapshot")
	assert.Equal(t, "test-synth", *p.SynthesisAgent)
	require.NotNil(t, p.SynthesisModel, "synthesis_model survives from the panel-row snapshot")
	assert.Equal(t, "test-model", *p.SynthesisModel)
	assert.Empty(t, p.Jobs, "jobs reflect only currently retained review_jobs rows")
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

// seedLegacyCIReview inserts a review_jobs row plus its ci_pr_reviews link,
// mirroring the frozen pre-panel schema (ci_pr_reviews.job_id -> review_jobs,
// created_at in SQLite's CURRENT_TIMESTAMP space format). No production
// writer remains for ci_pr_reviews, so tests populate it directly.
func seedLegacyCIReview(t *testing.T, db *DB, repoID int64, githubRepo string, pr int, headSHA, createdAt string) *ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repoID, GitRef: headSHA,
		Agent: "legacy-agent", Model: "legacy-model", Provider: "legacy-provider",
	})
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO ci_pr_reviews (github_repo, pr_number, head_sha, job_id, created_at)
		VALUES (?, ?, ?, ?, ?)`, githubRepo, pr, headSHA, job.ID, createdAt)
	require.NoError(t, err)
	return job
}

func TestExportCIMetricsLegacyRowShape(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	job := seedLegacyCIReview(t, db, repo.ID, "o/r", 7, "sha-legacy", "2026-03-01 10:00:00")
	setStatus(t, db, job.ID, JobStatusDone)
	setStartedAt(t, db, job.ID, time.Date(2026, 3, 1, 9, 55, 0, 0, time.UTC))
	_, err := db.Exec(`UPDATE review_jobs SET finished_at = ? WHERE id = ?`, "2026-03-01T10:05:00Z", job.ID)
	require.NoError(t, err)

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1)
	p := page.Panels[0]

	assert.Equal(t, "o/r", p.GithubRepo)
	assert.Equal(t, int64(7), p.PRNumber)
	assert.Equal(t, "sha-legacy", p.HeadSHA)
	assert.Equal(t, PanelOutcomeLegacyReview, p.Outcome)
	assert.NotEmpty(t, p.PostedAt)
	require.NotNil(t, p.FirstAttemptAt)
	assert.Equal(t, p.PanelCreatedAt, *p.FirstAttemptAt,
		"legacy panel_created_at and first_attempt_at both snapshot job.enqueued_at")
	assert.Nil(t, p.AttemptCount, "legacy rows predate retry-attempt tracking")
	require.NotNil(t, p.SynthesisAgent)
	assert.Equal(t, "legacy-agent", *p.SynthesisAgent)
	require.NotNil(t, p.SynthesisModel)
	assert.Equal(t, "legacy-model", *p.SynthesisModel)

	require.Len(t, p.Jobs, 1)
	j := p.Jobs[0]
	assert.Equal(t, job.UUID, j.JobUUID)
	assert.Equal(t, "review", j.Role)
	assert.Equal(t, "legacy-agent", j.Agent)
	require.NotNil(t, j.Model)
	assert.Equal(t, "legacy-model", *j.Model)
	require.NotNil(t, j.Provider)
	assert.Equal(t, "legacy-provider", *j.Provider)
	assert.Equal(t, string(JobStatusDone), j.Status)
	assert.NotNil(t, j.StartedAt)
	assert.NotNil(t, j.FinishedAt)
	assert.NotNil(t, page.NextCursor)
}

func TestExportCIMetricsLegacyPagination(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedLegacyCIReview(t, db, repo.ID, "o/r", 30, "sha-legacy-a", "2026-03-01 10:00:00")
	seedLegacyCIReview(t, db, repo.ID, "o/r", 31, "sha-legacy-b", "2026-03-02 10:00:00")

	first, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true, Limit: 1})
	require.NoError(t, err)
	require.Len(t, first.Panels, 1)
	assert.Equal(t, "sha-legacy-a", first.Panels[0].HeadSHA)
	assert.True(t, first.Truncated)
	require.NotNil(t, first.NextCursor)

	second, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true, Cursor: *first.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Panels, 1)
	assert.Equal(t, "sha-legacy-b", second.Panels[0].HeadSHA)
	assert.False(t, second.Truncated)
}

func TestExportCIMetricsRejectsCursorModeMismatch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedPostedPanel(t, db, 40, "sha-panel", PanelOutcomeReviewPosted)
	seedLegacyCIReview(t, db, repo.ID, "o/r", 41, "sha-legacy", "2026-03-01 10:00:00")

	panelPage, err := db.ExportCIMetrics(ExportCIMetricsOptions{})
	require.NoError(t, err)
	require.NotNil(t, panelPage.NextCursor)

	legacyPage, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	require.NotNil(t, legacyPage.NextCursor)

	_, err = db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true, Cursor: *panelPage.NextCursor})
	require.ErrorIs(t, err, ErrExportCIMetricsCursorModeMismatch,
		"a panel cursor must not be replayable against the legacy export")

	_, err = db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: false, Cursor: *legacyPage.NextCursor})
	require.ErrorIs(t, err, ErrExportCIMetricsCursorModeMismatch,
		"a legacy cursor must not be replayable against the panel export")
}

func TestExportCIMetricsLegacyAndPanelModesAreDisjoint(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedPostedPanel(t, db, 50, "sha-panel-only", PanelOutcomeReviewPosted)
	seedLegacyCIReview(t, db, repo.ID, "o/r", 51, "sha-legacy-only", "2026-03-01 10:00:00")

	panelPage, err := db.ExportCIMetrics(ExportCIMetricsOptions{})
	require.NoError(t, err)
	require.Len(t, panelPage.Panels, 1, "legacy rows must not appear in the default export")
	assert.Equal(t, "sha-panel-only", panelPage.Panels[0].HeadSHA)
	assert.NotEqual(t, PanelOutcomeLegacyReview, panelPage.Panels[0].Outcome)

	legacyPage, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	require.Len(t, legacyPage.Panels, 1, "posted panels must not appear in the legacy export")
	assert.Equal(t, "sha-legacy-only", legacyPage.Panels[0].HeadSHA)
	assert.Equal(t, PanelOutcomeLegacyReview, legacyPage.Panels[0].Outcome)
}
