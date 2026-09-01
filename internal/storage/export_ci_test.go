package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrationBackfillsPanelTerminalMetrics covers the startup backfill for
// panels finalized before outcome persistence existed: reopening the
// database reconstructs outcome from the retained MEMBER results, mirroring
// the posting decision (a failed synthesis whose member produced output
// still posted the raw fallback), and first_attempt_at/attempt_count from
// the surviving ci_pr_review_attempts row — the same source the finalizer
// snapshots — falling back to the run's earliest job enqueue when the
// attempt row was cleaned up.
func TestMigrationBackfillsPanelTerminalMetrics(t *testing.T) {
	tmpl, err := getTemplatePath()
	require.NoError(t, err)
	data, err := os.ReadFile(tmpl)
	require.NoError(t, err)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, os.WriteFile(dbPath, data, 0o644))
	db, err := Open(dbPath)
	require.NoError(t, err)

	// (a) Synthesis FAILED but a member produced real output: the raw
	// fallback was posted, so the outcome is review_posted, not a give-up.
	rawFallback := seedPostedPanel(t, db, 60, "sha-raw-fallback", PanelOutcomeReviewPosted)
	require.NotNil(t, rawFallback.SynthesisJobID)
	completeMemberWithOutput(t, db, rawFallback.PanelRunUUID, "Found a bug.")
	setStatus(t, db, *rawFallback.SynthesisJobID, JobStatusFailed)

	// (b) All members failed: the give-up note was posted.
	giveup := seedPostedPanel(t, db, 61, "sha-giveup", PanelOutcomeReviewPosted)
	setStatus(t, db, panelMemberID(t, db, giveup.PanelRunUUID), JobStatusFailed)

	// (c) All members skipped: the all-skip notice was posted.
	allSkip := seedPostedPanel(t, db, 62, "sha-all-skip", PanelOutcomeReviewPosted)
	setStatus(t, db, panelMemberID(t, db, allSkip.PanelRunUUID), JobStatusSkipped)

	// (d) Attempt row already cleaned up: first_attempt_at falls back to
	// the run's earliest job enqueue; attempt_count is unrecoverable.
	noAttempt := seedPostedPanel(t, db, 63, "sha-no-attempt", PanelOutcomeReviewPosted)
	completeMemberWithOutput(t, db, noAttempt.PanelRunUUID, "Looks good.")
	require.NoError(t, db.DeleteReviewAttempt("o/r", 63, "sha-no-attempt"))

	// (e) Member rows gone entirely: the outcome is unrecoverable.
	noJobs := seedPostedPanel(t, db, 64, "sha-no-jobs", PanelOutcomeReviewPosted)
	_, err = db.Exec(`DELETE FROM review_jobs WHERE panel_run_uuid = ?`, noJobs.PanelRunUUID)
	require.NoError(t, err)
	require.NoError(t, db.DeleteReviewAttempt("o/r", 64, "sha-no-jobs"))

	// (f) Synthesis failed on a transient outage AND a member produced
	// output: the live path deferred rather than post the degraded raw
	// fallback, so a posted row here is the exhausted transient give-up, not
	// a review — the synthesis-failure classification outranks member output.
	transientGiveup := seedPostedPanel(t, db, 65, "sha-transient-giveup", PanelOutcomeReviewPosted)
	require.NotNil(t, transientGiveup.SynthesisJobID)
	completeMemberWithOutput(t, db, transientGiveup.PanelRunUUID, "Partial output.")
	failSynthesisWithError(t, db, *transientGiveup.SynthesisJobID, "outage: provider unavailable")

	// (g) Same for a quota/session synthesis failure.
	quotaGiveup := seedPostedPanel(t, db, 66, "sha-quota-giveup", PanelOutcomeReviewPosted)
	require.NotNil(t, quotaGiveup.SynthesisJobID)
	completeMemberWithOutput(t, db, quotaGiveup.PanelRunUUID, "Partial output.")
	failSynthesisWithError(t, db, *quotaGiveup.SynthesisJobID, "quota: session limit reached")

	// Simulate rows finalized before the terminal-metrics columns existed.
	_, err = db.Exec(`UPDATE ci_pr_panels
		SET outcome = NULL, first_attempt_at = NULL, attempt_count = NULL`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reopened, err := Open(dbPath)
	require.NoError(t, err)
	defer reopened.Close()

	page, err := reopened.ExportCIMetrics(ExportCIMetricsOptions{})
	require.NoError(t, err)
	require.Len(t, page.Panels, 7)
	bySHA := map[string]ExportCIPanel{}
	for _, p := range page.Panels {
		bySHA[p.HeadSHA] = p
	}

	p := bySHA["sha-raw-fallback"]
	assert.Equal(t, PanelOutcomeReviewPosted, p.Outcome,
		"a member with output posted the raw fallback despite the failed synthesis")
	require.NotNil(t, p.FirstAttemptAt)
	assert.Equal(t, "2026-07-01T10:00:00Z", *p.FirstAttemptAt,
		"first_attempt_at snapshots the surviving attempt row")
	require.NotNil(t, p.AttemptCount, "attempt_count snapshots the surviving attempt row")
	assert.Equal(t, int64(1), *p.AttemptCount)

	assert.Equal(t, PanelOutcomeGiveupPosted, bySHA["sha-giveup"].Outcome)
	assert.Equal(t, PanelOutcomeNoReviewPosted, bySHA["sha-all-skip"].Outcome)

	p = bySHA["sha-no-attempt"]
	assert.Equal(t, PanelOutcomeReviewPosted, p.Outcome)
	require.NotNil(t, p.FirstAttemptAt,
		"first_attempt_at falls back to the run's earliest job enqueue")
	assert.Nil(t, p.AttemptCount, "attempt counts are unrecoverable without the attempt row")

	p = bySHA["sha-no-jobs"]
	assert.Equal(t, PanelOutcomeUnknown, p.Outcome,
		"no surviving member rows leaves the outcome unknown")
	assert.Nil(t, p.FirstAttemptAt)

	assert.Equal(t, PanelOutcomeGiveupPosted, bySHA["sha-transient-giveup"].Outcome,
		"a transient-failed synthesis is a give-up even with member output")
	assert.Equal(t, PanelOutcomeGiveupPosted, bySHA["sha-quota-giveup"].Outcome,
		"a quota-failed synthesis is a give-up even with member output")
}

// failSynthesisWithError marks a synthesis job failed with a classified
// error string (e.g. "outage: ..." or "quota: ..."), the signal the outcome
// backfill uses to tell a transient synthesis give-up from a genuine
// synthesis failure that still posted the raw member fallback.
func failSynthesisWithError(t *testing.T, db *DB, jobID int64, errText string) {
	t.Helper()
	_, err := db.Exec(`UPDATE review_jobs SET status = 'failed', error = ? WHERE id = ?`,
		errText, jobID)
	require.NoError(t, err)
}

// TestMigrationBackfillsSynthesisSnapshotSurvivesCascade covers the synthesis
// agent/model backfill: a panel finalized before those snapshot columns
// existed relied on the live review_jobs join, so a later cascade repo
// deletion would erase its model attribution. The startup backfill snapshots
// the pair from the synthesis job, so the export still reports the model
// after the repo (and its review_jobs rows) are deleted.
func TestMigrationBackfillsSynthesisSnapshotSurvivesCascade(t *testing.T) {
	tmpl, err := getTemplatePath()
	require.NoError(t, err)
	data, err := os.ReadFile(tmpl)
	require.NoError(t, err)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, os.WriteFile(dbPath, data, 0o644))
	db, err := Open(dbPath)
	require.NoError(t, err)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	created, err := db.ReserveReviewAttempt("o/r", 70, "sha-cascade-pre", now)
	require.NoError(t, err)
	require.True(t, created)
	members := []EnqueueOpts{{RepoID: repo.ID, GitRef: "b..h", Agent: "m", PanelMemberIndex: 0}}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "b..h", Agent: "test-synth", Model: "test-model"}
	runCreated, _, _, err := db.CreateCIPanelRun("o/r", 70, "sha-cascade-pre", members, synthesis)
	require.NoError(t, err)
	require.True(t, runCreated)
	panel, err := db.GetCIPanelByPRSHA("o/r", 70, "sha-cascade-pre")
	require.NoError(t, err)
	require.NoError(t, db.MarkPanelPosted(panel.ID, PanelOutcomeReviewPosted))
	// Simulate a row finalized before the snapshot columns existed: clear
	// the snapshot so only the live synthesis job carries the model.
	_, err = db.Exec(`UPDATE ci_pr_panels
		SET synthesis_agent = NULL, synthesis_model = NULL WHERE id = ?`, panel.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Reopen to run the backfill, THEN cascade-delete the repo.
	reopened, err := Open(dbPath)
	require.NoError(t, err)
	defer reopened.Close()
	require.NoError(t, reopened.DeleteRepo(repo.ID, true))

	page, err := reopened.ExportCIMetrics(ExportCIMetricsOptions{})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1)
	p := page.Panels[0]
	require.NotNil(t, p.SynthesisAgent, "synthesis_agent survives cascade via the backfilled snapshot")
	assert.Equal(t, "test-synth", *p.SynthesisAgent)
	require.NotNil(t, p.SynthesisModel, "synthesis_model survives cascade via the backfilled snapshot")
	assert.Equal(t, "test-model", *p.SynthesisModel)
	assert.Empty(t, p.Jobs, "the underlying review_jobs rows are gone")
}

// panelMemberID returns the id of a panel run's single seeded member job.
func panelMemberID(t *testing.T, db *DB, runUUID uuid.UUID) int64 {
	t.Helper()
	var id int64
	require.NoError(t, db.QueryRow(`SELECT id FROM review_jobs
		WHERE panel_run_uuid = ? AND panel_role = 'member'`, runUUID).Scan(&id))
	return id
}

// completeMemberWithOutput marks a panel run's member job done and stores a
// review row with the given output — the evidence the outcome backfill keys
// on.
func completeMemberWithOutput(t *testing.T, db *DB, runUUID uuid.UUID, output string) {
	t.Helper()
	id := panelMemberID(t, db, runUUID)
	setStatus(t, db, id, JobStatusDone)
	_, err := db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output)
		VALUES (?, 'test', 'p', ?)`, id, output)
	require.NoError(t, err)
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

	posted := seedPostedPanel(t, db, 1, "sha-posted", PanelOutcomeReviewPosted)
	assignment := &ExperimentAssignmentInput{
		ExperimentID: "ci-v1", DefinitionHash: "definition-hash",
		DefinitionJSON: `{"ratio":1}`, Arm: "experiment",
		SubjectHash: "subject-hash", EffectiveConfigHash: "config-hash",
		EffectiveConfigJSON: `{"members":[]}`,
	}
	require.NoError(t, insertExperimentAssignmentTx(
		context.Background(), db, ReviewUnitPanel, posted.PanelRunUUID,
		assignment, testUUID("test-machine"), time.Now(),
	))
	members, err := db.GetPanelMembers(posted.PanelRunUUID)
	require.NoError(t, err)
	require.NotEmpty(t, members)
	_, err = db.Exec(`UPDATE review_jobs SET resume_source_job_uuid = ? WHERE id = ?`,
		testUUID("source-job"), members[0].ID)
	require.NoError(t, err)
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
	require.Len(t, p.Experiments, 1)
	assert.Equal(t, "ci-v1", p.Experiments[0].ID)
	var lineageFound bool
	for _, j := range p.Jobs {
		assert.NotEmpty(t, j.JobUUID)
		assert.Contains(t, []string{"member", "synthesis"}, j.Role)
		assert.NotEmpty(t, j.Agent)
		assert.NotEmpty(t, j.Status)
		if j.ResumeSourceJobUUID != nil {
			assert.Equal(t, testUUID("source-job"), *j.ResumeSourceJobUUID)
			lineageFound = true
		}
	}
	assert.True(t, lineageFound)
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
		DatabaseID: testUUID("some-other-database"),
		PostedAt:   "2026-07-01T00:00:00Z",
		PanelID:    1,
	})
	require.NoError(t, err)
	cursor := base64.RawURLEncoding.EncodeToString(raw)

	_, err = db.ExportCIMetrics(ExportCIMetricsOptions{Cursor: cursor})
	require.ErrorIs(t, err, ErrExportCursorDatabaseMismatch)
}

// seedLegacyCIJob inserts one completed pre-panel CI review job with no
// panel run and explicit enqueue/finish timestamps. Rows from that era
// predate all CI tagging (source and ci_base_branch stay empty), so the
// legacy export identifies pseudopanels structurally: two or more terminal
// jobs sharing (repo, git_ref), at least one done.
func seedLegacyCIJob(t *testing.T, db *DB, repoID int64, gitRef, agent string, enqueued, finished string) *ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repoID, GitRef: gitRef,
		Agent: agent, Model: "legacy-model", Provider: "legacy-provider",
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs
		SET status = 'done', enqueued_at = ?, started_at = ?, finished_at = ?
		WHERE id = ?`, enqueued, enqueued, finished, job.ID)
	require.NoError(t, err)
	return job
}

// seedPanelEraMarker inserts a minimal ci_pr_panels row so the database has
// panel activity starting at createdAt: the pre-panel era ends there, and
// only legacy jobs enqueued earlier remain exportable.
func seedPanelEraMarker(t *testing.T, db *DB, createdAt string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO ci_pr_panels
		(github_repo, pr_number, head_sha, panel_run_uuid, created_at)
		VALUES ('era/marker', 999999, ?, ?, ?)`,
		"sha-era-"+createdAt, "era-"+createdAt, createdAt)
	require.NoError(t, err)
}

func TestExportCIMetricsLegacyCombinesPseudopanel(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedPanelEraMarker(t, db, "2026-06-01 00:00:00")
	jobA := seedLegacyCIJob(t, db, repo.ID, "base..head-legacy", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")
	jobB := seedLegacyCIJob(t, db, repo.ID, "base..head-legacy", "gemini",
		"2026-03-01 10:00:00", "2026-03-01 10:08:00")
	// A failed sibling is part of the unit: the batch posted only after
	// every member was terminal, so its finish extends the wall clock.
	failed := seedLegacyCIJob(t, db, repo.ID, "base..head-legacy", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:30:00")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'failed' WHERE id = ?`, failed.ID)
	require.NoError(t, err)
	// A singleton review of another ref is a manual one-off, not a
	// pseudopanel, and must not export.
	seedLegacyCIJob(t, db, repo.ID, "base..head-singleton", "codex",
		"2026-03-02 10:00:00", "2026-03-02 10:05:00")

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1, "one pseudopanel unit per (repo, git_ref); singletons excluded")
	p := page.Panels[0]

	assert.Equal(t, repo.Name, p.GithubRepo,
		"falls back to repos.name when no identity is recorded")
	assert.Equal(t, int64(0), p.PRNumber, "the PR linkage is unrecoverable for legacy units")
	assert.Equal(t, "head-legacy", p.HeadSHA)
	assert.Equal(t, PanelOutcomeLegacyReview, p.Outcome)
	assert.Equal(t, "2026-03-01T10:30:00Z", p.PostedAt,
		"latest finish of the unit's terminal jobs, including the failed sibling")
	require.NotNil(t, p.FirstAttemptAt)
	assert.Equal(t, "2026-03-01T10:00:00Z", *p.FirstAttemptAt, "earliest enqueue of the unit")
	assert.Equal(t, p.PanelCreatedAt, *p.FirstAttemptAt)
	assert.Nil(t, p.AttemptCount, "legacy rows predate retry-attempt tracking")
	assert.Nil(t, p.SynthesisAgent, "pseudopanels had no synthesis")
	assert.Nil(t, p.SynthesisModel, "pseudopanels had no synthesis")

	require.Len(t, p.Jobs, 3)
	assert.Equal(t, *jobA.UUID, p.Jobs[0].JobUUID)
	assert.Equal(t, *jobB.UUID, p.Jobs[1].JobUUID)
	assert.Equal(t, *failed.UUID, p.Jobs[2].JobUUID)
	assert.Equal(t, string(JobStatusDone), p.Jobs[0].Status)
	assert.Equal(t, string(JobStatusDone), p.Jobs[1].Status)
	assert.Equal(t, string(JobStatusFailed), p.Jobs[2].Status)
	for _, j := range p.Jobs {
		assert.Equal(t, "review", j.Role)
		require.NotNil(t, j.Model)
		assert.Equal(t, "legacy-model", *j.Model)
		require.NotNil(t, j.Provider)
		assert.Equal(t, "legacy-provider", *j.Provider)
		assert.NotNil(t, j.StartedAt)
		assert.NotNil(t, j.FinishedAt)
	}
	assert.NotNil(t, page.NextCursor)
}

// TestExportCIMetricsLegacyPartialSuccessUnit covers the batch flow's
// partial-success posting: one done job plus a failed or canceled sibling is
// a posted review and must export as a unit (the batch treated both failure
// modes as terminal and posted the available output), while a group with no
// successful job posted nothing reviewable and is excluded.
func TestExportCIMetricsLegacyPartialSuccessUnit(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedPanelEraMarker(t, db, "2026-06-01 00:00:00")
	seedLegacyCIJob(t, db, repo.ID, "base..head-partial", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")
	partialFailed := seedLegacyCIJob(t, db, repo.ID, "base..head-partial", "gemini",
		"2026-03-01 10:00:00", "2026-03-01 10:07:00")
	// A timed-out member was canceled, not failed: it is still a terminal
	// member of the batch, so it counts toward the unit and its finish
	// extends the wall clock.
	seedLegacyCIJob(t, db, repo.ID, "base..head-canceled", "codex",
		"2026-03-03 10:00:00", "2026-03-03 10:05:00")
	canceledSibling := seedLegacyCIJob(t, db, repo.ID, "base..head-canceled", "gemini",
		"2026-03-03 10:00:00", "2026-03-03 10:09:00")
	allFailedA := seedLegacyCIJob(t, db, repo.ID, "base..head-all-failed", "codex",
		"2026-03-02 10:00:00", "2026-03-02 10:05:00")
	allFailedB := seedLegacyCIJob(t, db, repo.ID, "base..head-all-failed", "gemini",
		"2026-03-02 10:00:00", "2026-03-02 10:06:00")
	for _, id := range []int64{partialFailed.ID, allFailedA.ID} {
		_, err := db.Exec(`UPDATE review_jobs SET status = 'failed' WHERE id = ?`, id)
		require.NoError(t, err)
	}
	for _, id := range []int64{canceledSibling.ID, allFailedB.ID} {
		_, err := db.Exec(`UPDATE review_jobs SET status = 'canceled' WHERE id = ?`, id)
		require.NoError(t, err)
	}

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	require.Len(t, page.Panels, 2,
		"partial-success units export; a group with no successful job posted no review")
	bySHA := map[string]ExportCIPanel{}
	for _, p := range page.Panels {
		bySHA[p.HeadSHA] = p
	}

	partial := bySHA["head-partial"]
	assert.Equal(t, "2026-03-01T10:07:00Z", partial.PostedAt,
		"wall clock ends at the failed sibling's finish")
	require.Len(t, partial.Jobs, 2)

	canceled := bySHA["head-canceled"]
	assert.Equal(t, "2026-03-03T10:09:00Z", canceled.PostedAt,
		"wall clock ends at the canceled sibling's finish")
	require.Len(t, canceled.Jobs, 2, "the canceled member belongs to the unit")
	assert.Equal(t, string(JobStatusCanceled), canceled.Jobs[1].Status)
}

// TestExportCIMetricsLegacyExcludesUnfinishedMigrationCancels covers the
// panel migration's own cleanup: it canceled leftover in-flight batch jobs
// without stamping finished_at, and those batches never posted a review. An
// unfinished canceled sibling must not join a unit (which would stretch its
// wall clock to the migration's timestamp), leaving its lone done job a
// singleton.
func TestExportCIMetricsLegacyExcludesUnfinishedMigrationCancels(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedPanelEraMarker(t, db, "2026-06-01 00:00:00")
	seedLegacyCIJob(t, db, repo.ID, "base..head-inflight", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")
	superseded := seedLegacyCIJob(t, db, repo.ID, "base..head-inflight", "gemini",
		"2026-03-01 10:00:00", "2026-03-01 10:06:00")
	_, err := db.Exec(`UPDATE review_jobs
		SET status = 'canceled', error = 'superseded by panel migration', finished_at = NULL
		WHERE id = ?`, superseded.ID)
	require.NoError(t, err)

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	assert.Empty(t, page.Panels,
		"a never-finished cancel leaves the surviving job a singleton, not a unit")
}

// TestExportCIMetricsLegacyUsesRepoIdentity covers github_repo naming: the
// export must use the repo's remote identity (mapped to owner/repo), not the
// checkout basename, so same-named clones of different repositories are not
// conflated.
func TestExportCIMetricsLegacyUsesRepoIdentity(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"),
		"git@github.com:owner/project.git")
	require.NoError(t, err)
	seedPanelEraMarker(t, db, "2026-06-01 00:00:00")
	seedLegacyCIJob(t, db, repo.ID, "base..head-ident", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")
	seedLegacyCIJob(t, db, repo.ID, "base..head-ident", "gemini",
		"2026-03-01 10:00:00", "2026-03-01 10:06:00")

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1)
	assert.Equal(t, "owner/project", page.Panels[0].GithubRepo)
}

// TestExportCIMetricsLegacyBoundedToPrePanelEra covers the era bound: only
// jobs enqueued before the database's first panel activity can form legacy
// units, so post-panel manual reviews of the same ref never appear as new
// pseudopanels, and a database with no panel activity exports nothing.
func TestExportCIMetricsLegacyBoundedToPrePanelEra(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedLegacyCIJob(t, db, repo.ID, "base..head-pre", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")
	seedLegacyCIJob(t, db, repo.ID, "base..head-pre", "gemini",
		"2026-03-01 10:00:00", "2026-03-01 10:06:00")

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	assert.Empty(t, page.Panels,
		"a database with no panel activity has no pre-panel era")
	assert.Nil(t, page.NextCursor)

	// Panel era starts 2026-06-01: the March unit exports, but two manual
	// reviews of one ref after that date must not form a pseudopanel.
	seedPanelEraMarker(t, db, "2026-06-01 00:00:00")
	seedLegacyCIJob(t, db, repo.ID, "base..head-post", "codex",
		"2026-07-01 10:00:00", "2026-07-01 10:05:00")
	seedLegacyCIJob(t, db, repo.ID, "base..head-post", "gemini",
		"2026-07-01 10:00:00", "2026-07-01 10:06:00")

	page, err = db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1)
	assert.Equal(t, "head-pre", page.Panels[0].HeadSHA,
		"post-panel-era jobs are excluded from legacy units")
}

// TestExportCIMetricsLegacyExcludesNonAdjacentReReview covers the adjacency
// window: a manual re-review of the same ref days after the pseudopanel ran
// must not stretch the unit's wall clock (a production ref re-reviewed 12
// days later inflated its turnaround to 290 hours before this bound).
func TestExportCIMetricsLegacyExcludesNonAdjacentReReview(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedPanelEraMarker(t, db, "2026-06-01 00:00:00")
	seedLegacyCIJob(t, db, repo.ID, "base..head-rerun", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")
	seedLegacyCIJob(t, db, repo.ID, "base..head-rerun", "gemini",
		"2026-03-01 10:00:10", "2026-03-01 10:07:00")
	// Re-reviewed 12 days later: outside the adjacency window.
	seedLegacyCIJob(t, db, repo.ID, "base..head-rerun", "codex",
		"2026-03-13 09:00:00", "2026-03-13 09:06:00")

	page, err := db.ExportCIMetrics(ExportCIMetricsOptions{Legacy: true})
	require.NoError(t, err)
	require.Len(t, page.Panels, 1)
	p := page.Panels[0]
	assert.Equal(t, "2026-03-01T10:07:00Z", p.PostedAt,
		"wall clock ends at the adjacent group's last finish, not the re-review's")
	require.Len(t, p.Jobs, 2, "the non-adjacent re-review job is not part of the unit")
}

func TestExportCIMetricsLegacyPagination(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedPanelEraMarker(t, db, "2026-06-01 00:00:00")
	seedLegacyCIJob(t, db, repo.ID, "sha-legacy-a", "codex",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")
	seedLegacyCIJob(t, db, repo.ID, "sha-legacy-a", "gemini",
		"2026-03-01 10:00:00", "2026-03-01 10:06:00")
	seedLegacyCIJob(t, db, repo.ID, "sha-legacy-b", "codex",
		"2026-03-02 10:00:00", "2026-03-02 10:05:00")
	seedLegacyCIJob(t, db, repo.ID, "sha-legacy-b", "gemini",
		"2026-03-02 10:00:00", "2026-03-02 10:06:00")

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
	seedLegacyCIJob(t, db, repo.ID, "sha-legacy", "gemini",
		"2026-03-01 10:00:00", "2026-03-01 10:06:00")

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
	seedLegacyCIJob(t, db, repo.ID, "sha-legacy-only", "gemini",
		"2026-03-01 10:00:00", "2026-03-01 10:06:00")

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
