package storage

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrationBackfillsPanelTerminalMetrics covers the startup backfill for
// panels finalized before outcome persistence existed: reopening the
// database reconstructs outcome from the retained synthesis job's terminal
// status and first_attempt_at from the panel run's earliest job enqueue, so
// pre-existing posted panels export with real terminal metrics instead of
// "unknown"/NULL.
func TestMigrationBackfillsPanelTerminalMetrics(t *testing.T) {
	tmpl, err := getTemplatePath()
	require.NoError(t, err)
	data, err := os.ReadFile(tmpl)
	require.NoError(t, err)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, os.WriteFile(dbPath, data, 0o644))
	db, err := Open(dbPath)
	require.NoError(t, err)

	panel := seedPostedPanel(t, db, 60, "sha-backfill", PanelOutcomeReviewPosted)
	require.NotNil(t, panel.SynthesisJobID)
	setStatus(t, db, *panel.SynthesisJobID, JobStatusDone)
	// Simulate a row finalized before the terminal-metrics columns existed.
	_, err = db.Exec(`UPDATE ci_pr_panels
		SET outcome = NULL, first_attempt_at = NULL, attempt_count = NULL
		WHERE id = ?`, panel.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reopened, err := Open(dbPath)
	require.NoError(t, err)
	defer reopened.Close()

	page, err := reopened.ExportCIMetrics(ExportCIMetricsOptions{})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1)
	p := page.Panels[0]
	assert.Equal(t, PanelOutcomeReviewPosted, p.Outcome,
		"outcome reconstructed from the synthesis job's terminal status")
	require.NotNil(t, p.FirstAttemptAt,
		"first_attempt_at reconstructed from the run's earliest job enqueue")
	assert.Nil(t, p.AttemptCount, "attempt counts are unrecoverable")
}

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

// seedLegacyCIJob inserts one completed pre-panel CI review job:
// source='ci', no panel run, explicit enqueue/finish timestamps. The
// pre-panel era wrote no PR linkage rows that survive, so the legacy export
// groups these jobs by (repo, git_ref) into pseudopanel units.
func seedLegacyCIJob(t *testing.T, db *DB, repoID int64, gitRef, agent string, enqueued, finished string) *ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repoID, GitRef: gitRef,
		Agent: agent, Model: "legacy-model", Provider: "legacy-provider",
		Source: JobSourceCI,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs
		SET status = 'done', enqueued_at = ?, started_at = ?, finished_at = ?
		WHERE id = ?`, enqueued, enqueued, finished, job.ID)
	require.NoError(t, err)
	return job
}

func TestExportCIMetricsLegacyCombinesPseudopanel(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	jobA := seedLegacyCIJob(t, db, repo.ID, "base..head-legacy", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")
	jobB := seedLegacyCIJob(t, db, repo.ID, "base..head-legacy", "gemini",
		"2026-03-01 10:00:00", "2026-03-01 10:08:00")
	// A failed sibling contributes neither to the wall clock nor to Jobs.
	failed := seedLegacyCIJob(t, db, repo.ID, "base..head-legacy", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:30:00")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'failed' WHERE id = ?`, failed.ID)
	require.NoError(t, err)

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1, "one pseudopanel unit per (repo, git_ref)")
	p := page.Panels[0]

	assert.Equal(t, repo.Name, p.GithubRepo)
	assert.Equal(t, int64(0), p.PRNumber, "the PR linkage is unrecoverable for legacy units")
	assert.Equal(t, "head-legacy", p.HeadSHA)
	assert.Equal(t, PanelOutcomeLegacyReview, p.Outcome)
	assert.Equal(t, "2026-03-01T10:08:00Z", p.PostedAt, "latest finish of the unit's done jobs")
	require.NotNil(t, p.FirstAttemptAt)
	assert.Equal(t, "2026-03-01T10:00:00Z", *p.FirstAttemptAt, "earliest enqueue of the unit")
	assert.Equal(t, p.PanelCreatedAt, *p.FirstAttemptAt)
	assert.Nil(t, p.AttemptCount, "legacy rows predate retry-attempt tracking")
	assert.Nil(t, p.SynthesisAgent, "pseudopanels had no synthesis")
	assert.Nil(t, p.SynthesisModel, "pseudopanels had no synthesis")

	require.Len(t, p.Jobs, 2)
	assert.Equal(t, jobA.UUID, p.Jobs[0].JobUUID)
	assert.Equal(t, jobB.UUID, p.Jobs[1].JobUUID)
	for _, j := range p.Jobs {
		assert.Equal(t, "review", j.Role)
		assert.Equal(t, string(JobStatusDone), j.Status)
		require.NotNil(t, j.Model)
		assert.Equal(t, "legacy-model", *j.Model)
		require.NotNil(t, j.Provider)
		assert.Equal(t, "legacy-provider", *j.Provider)
		assert.NotNil(t, j.StartedAt)
		assert.NotNil(t, j.FinishedAt)
	}
	assert.NotNil(t, page.NextCursor)
}

func TestExportCIMetricsLegacyPagination(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedLegacyCIJob(t, db, repo.ID, "sha-legacy-a", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")
	seedLegacyCIJob(t, db, repo.ID, "sha-legacy-b", "codex",
		"2026-03-02 10:00:00", "2026-03-02 10:05:00")

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
	seedLegacyCIJob(t, db, repo.ID, "sha-legacy", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")

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
	seedLegacyCIJob(t, db, repo.ID, "sha-legacy-only", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")

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
