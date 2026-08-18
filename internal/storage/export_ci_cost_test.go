package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ciCostJobSeed struct {
	gitRef       string
	status       JobStatus
	source       string
	role         string
	agentInvoked bool
	tokenUsage   string
	finishedAt   string
	model        string
	provider     string
}

func seedExportCICostJob(t *testing.T, db *DB, repoID int64, seed ciCostJobSeed) *ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repoID, GitRef: seed.gitRef, Agent: "test-agent",
		Model: seed.model, Provider: seed.provider, Source: seed.source,
		PanelRunUUID: "run-" + seed.gitRef, PanelRole: seed.role,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs
		SET status = ?, started_at = '2026-08-01 00:00:00', finished_at = ?,
		    updated_at = ?, agent_invoked = ?, token_usage = ?
		WHERE id = ?`, seed.status, seed.finishedAt, seed.finishedAt, seed.agentInvoked,
		seed.tokenUsage, job.ID)
	require.NoError(t, err)
	return job
}

func TestExportCICostsIncludesMappedPanelJobWithoutSource(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	job := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "mapped-panel", status: JobStatusDone, role: PanelRoleMember,
		agentInvoked: true, tokenUsage: `{"has_cost":true,"cost_usd":0.5}`,
		finishedAt: "2026-08-01 01:00:00",
	})
	_, err := db.Exec(`INSERT INTO ci_pr_panels
		(github_repo, pr_number, head_sha, panel_run_uuid, created_at)
		VALUES ('owner/project', 1, 'head', ?, '2026-08-01 00:00:00')`, "run-mapped-panel")
	require.NoError(t, err)
	require.NoError(t, insertExperimentAssignmentTx(
		context.Background(), db, ReviewUnitPanel, "run-mapped-panel",
		&ExperimentAssignmentInput{
			ExperimentID: "ci-v1", DefinitionHash: "definition-hash",
			DefinitionJSON: `{"ratio":1}`, Arm: "experiment",
			SubjectHash: "subject-hash", EffectiveConfigHash: "config-hash",
			EffectiveConfigJSON: `{"members":[]}`,
		},
		"test-machine", time.Now(),
	))
	_, err = db.Exec(`UPDATE review_jobs SET resume_source_job_uuid = ? WHERE id = ?`,
		"source-job-uuid", job.ID)
	require.NoError(t, err)

	page, err := db.ExportCICosts(ExportCICostOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{job.UUID}, costJobUUIDs(page.Jobs))
	require.Len(t, page.Jobs[0].Experiments, 1)
	assert.Equal(t, "ci-v1", page.Jobs[0].Experiments[0].ID)
	require.NotNil(t, page.Jobs[0].ResumeSourceJobUUID)
	assert.Equal(t, "source-job-uuid", *page.Jobs[0].ResumeSourceJobUUID)

	require.NoError(t, db.DeleteCIPanelByRun("run-mapped-panel"))
	page, err = db.ExportCICosts(ExportCICostOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{job.UUID}, costJobUUIDs(page.Jobs))
}

func TestExportCICostsLatePricingAppearsOnFreshRescan(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	firstJob := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "first-unpriced", status: JobStatusDone, source: JobSourceCI,
		role: PanelRoleMember, agentInvoked: true, tokenUsage: `{}`,
		finishedAt: "2026-08-01 01:00:00",
	})
	secondJob := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "second", status: JobStatusDone, source: JobSourceCI,
		role: PanelRoleMember, agentInvoked: true, tokenUsage: `{}`,
		finishedAt: "2026-08-01 02:00:00",
	})

	first, err := db.ExportCICosts(ExportCICostOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{firstJob.UUID}, costJobUUIDs(first.Jobs))
	require.NotNil(t, first.NextCursor)

	_, err = db.Exec(`UPDATE review_jobs
		SET token_usage = '{"has_cost":true,"cost_usd":0.75}',
		    updated_at = '2026-08-01 01:00:00'
		WHERE id = ?`, firstJob.ID)
	require.NoError(t, err)

	resumed, err := db.ExportCICosts(ExportCICostOptions{Cursor: *first.NextCursor, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, []string{secondJob.UUID}, costJobUUIDs(resumed.Jobs))

	rescanned, err := db.ExportCICosts(ExportCICostOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{firstJob.UUID, secondJob.UUID}, costJobUUIDs(rescanned.Jobs))
	require.NotNil(t, rescanned.Jobs[0].CostUSD)
	assert.InDelta(t, 0.75, *rescanned.Jobs[0].CostUSD, 1e-12)
}

func TestExportCICostsCursorPreservesWindow(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	oldJob := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "old", status: JobStatusDone, source: JobSourceCI,
		role: PanelRoleMember, agentInvoked: true, tokenUsage: `{}`,
		finishedAt: "2026-07-01 01:00:00",
	})
	firstJob := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "first-in-window", status: JobStatusDone, source: JobSourceCI,
		role: PanelRoleMember, agentInvoked: true, tokenUsage: `{}`,
		finishedAt: "2026-08-01 01:00:00",
	})
	secondJob := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "second-in-window", status: JobStatusDone, source: JobSourceCI,
		role: PanelRoleMember, agentInvoked: true, tokenUsage: `{}`,
		finishedAt: "2026-08-01 02:00:00",
	})
	afterWindow := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "after-window", status: JobStatusDone, source: JobSourceCI,
		role: PanelRoleMember, agentInvoked: true, tokenUsage: `{}`,
		finishedAt: "2026-08-02 00:00:00",
	})
	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)

	first, err := db.ExportCICosts(ExportCICostOptions{Since: since, Until: until, Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{firstJob.UUID}, costJobUUIDs(first.Jobs))
	require.NotNil(t, first.NextCursor)

	_, err = db.Exec(`UPDATE review_jobs
		SET token_usage = '{"has_cost":true,"cost_usd":0.75}'
		WHERE id = ?`, oldJob.ID)
	require.NoError(t, err)

	second, err := db.ExportCICosts(ExportCICostOptions{
		Cursor: *first.NextCursor, Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{secondJob.UUID}, costJobUUIDs(second.Jobs))
	assert.NotContains(t, costJobUUIDs(second.Jobs), oldJob.UUID)
	assert.NotContains(t, costJobUUIDs(second.Jobs), afterWindow.UUID)
	assert.Equal(t, since, second.EffectiveSince)
	assert.Equal(t, until, second.EffectiveUntil)
}

func costJobsByUUID(jobs []ExportCICostJob) map[string]ExportCICostJob {
	out := make(map[string]ExportCICostJob, len(jobs))
	for _, job := range jobs {
		out[job.JobUUID] = job
	}
	return out
}

func costJobUUIDs(jobs []ExportCICostJob) []string {
	out := make([]string, len(jobs))
	for i, job := range jobs {
		out[i] = job.JobUUID
	}
	return out
}

func TestExportCICostsRegularEligibilityAndPricing(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))

	priced := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "priced", status: JobStatusDone, source: JobSourceCI,
		role: PanelRoleMember, agentInvoked: true,
		tokenUsage: `{"has_cost":true,"cost_usd":0.125}`,
		finishedAt: "2026-08-01 01:00:00", model: "model-a", provider: "provider-a",
	})
	unpriced := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "unpriced", status: JobStatusFailed, source: JobSourceCI,
		role: PanelRoleSynthesis, agentInvoked: true, tokenUsage: `{}`,
		finishedAt: "2026-08-01T02:00:00Z",
	})
	zero := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "zero", status: JobStatusCanceled, source: JobSourceCI,
		role: PanelRoleMember, agentInvoked: true,
		tokenUsage: `{"has_cost":true,"cost_usd":0}`,
		finishedAt: "2026-08-01 03:00:00", model: "model-b", provider: "provider-b",
	})
	usageProof := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "usage-proof", status: JobStatusFailed, source: JobSourceCI,
		role: PanelRoleMember, tokenUsage: `{"total_output_tokens":7}`,
		finishedAt: "2026-08-01 04:00:00",
	})
	malformedCost := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "malformed-cost", status: JobStatusDone, source: JobSourceCI,
		role: PanelRoleMember, agentInvoked: true,
		tokenUsage: `{"has_cost":true,"cost_usd":"unknown"}`,
		finishedAt: "2026-08-01 05:00:00",
	})
	skipped := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{
		gitRef: "skipped", status: JobStatusSkipped, source: JobSourceCI,
		role: PanelRoleMember, agentInvoked: true,
		tokenUsage: `{"has_cost":true,"cost_usd":1}`,
		finishedAt: "2026-08-01 06:00:00",
	})

	for _, seed := range []ciCostJobSeed{
		{gitRef: "passthrough", status: JobStatusDone, source: JobSourceCI, role: PanelRoleSynthesis, finishedAt: "2026-08-01 07:00:00"},
		{gitRef: "pre-agent", status: JobStatusFailed, source: JobSourceCI, role: PanelRoleMember, finishedAt: "2026-08-01 08:00:00"},
		{gitRef: "manual", status: JobStatusDone, role: PanelRoleMember, agentInvoked: true, tokenUsage: `{"has_cost":true,"cost_usd":1}`, finishedAt: "2026-08-01 09:00:00"},
	} {
		seedExportCICostJob(t, db, repo.ID, seed)
	}

	page, err := db.ExportCICosts(ExportCICostOptions{})
	require.NoError(t, err)
	require.Len(t, page.Jobs, 6)
	assert.False(t, page.Truncated)
	require.NotNil(t, page.NextCursor)

	byUUID := costJobsByUUID(page.Jobs)
	assert.Equal(t, []string{priced.UUID, unpriced.UUID, zero.UUID, usageProof.UUID, malformedCost.UUID, skipped.UUID}, costJobUUIDs(page.Jobs))

	got := byUUID[priced.UUID]
	assert.Equal(t, "2026-08-01T01:00:00Z", got.FinishedAt)
	assert.Equal(t, "test-agent", got.Agent)
	assert.Equal(t, PanelRoleMember, got.Role)
	assert.Equal(t, string(JobStatusDone), got.Status)
	require.NotNil(t, got.Model)
	assert.Equal(t, "model-a", *got.Model)
	require.NotNil(t, got.Provider)
	assert.Equal(t, "provider-a", *got.Provider)
	require.NotNil(t, got.CostUSD)
	assert.InDelta(t, 0.125, *got.CostUSD, 1e-12)

	assert.Equal(t, PanelRoleSynthesis, byUUID[unpriced.UUID].Role)
	assert.Nil(t, byUUID[unpriced.UUID].Model)
	assert.Nil(t, byUUID[unpriced.UUID].Provider)
	assert.Nil(t, byUUID[unpriced.UUID].CostUSD)
	require.NotNil(t, byUUID[zero.UUID].CostUSD)
	assert.Zero(t, *byUUID[zero.UUID].CostUSD)
	assert.Nil(t, byUUID[usageProof.UUID].CostUSD)
	assert.Nil(t, byUUID[malformedCost.UUID].CostUSD)
	require.NotNil(t, byUUID[skipped.UUID].CostUSD)
	assert.InDelta(t, 1, *byUUID[skipped.UUID].CostUSD, 1e-12)
}

func TestExportCICostsOrderingAndBounds(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))

	before := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{gitRef: "before", status: JobStatusDone, source: JobSourceCI, role: PanelRoleMember, agentInvoked: true, finishedAt: "2026-07-31 23:59:59"})
	atSince := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{gitRef: "at-since", status: JobStatusDone, source: JobSourceCI, role: PanelRoleMember, agentInvoked: true, finishedAt: "2026-08-01T00:00:00Z"})
	sameTimeA := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{gitRef: "same-a", status: JobStatusDone, source: JobSourceCI, role: PanelRoleMember, agentInvoked: true, finishedAt: "2026-08-01 12:00:00"})
	sameTimeB := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{gitRef: "same-b", status: JobStatusDone, source: JobSourceCI, role: PanelRoleMember, agentInvoked: true, finishedAt: "2026-08-01T12:00:00Z"})
	beforeUntil := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{gitRef: "before-until", status: JobStatusDone, source: JobSourceCI, role: PanelRoleMember, agentInvoked: true, finishedAt: "2026-08-01 23:59:59"})
	atUntil := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{gitRef: "at-until", status: JobStatusDone, source: JobSourceCI, role: PanelRoleMember, agentInvoked: true, finishedAt: "2026-08-02 00:00:00"})

	page, err := db.ExportCICosts(ExportCICostOptions{
		Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{atSince.UUID, sameTimeA.UUID, sameTimeB.UUID, beforeUntil.UUID}, costJobUUIDs(page.Jobs))
	assert.NotContains(t, costJobUUIDs(page.Jobs), before.UUID)
	assert.NotContains(t, costJobUUIDs(page.Jobs), atUntil.UUID)
}

func TestExportCICostsPaginationAndCursorSafety(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	firstJob := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{gitRef: "first", status: JobStatusDone, source: JobSourceCI, role: PanelRoleMember, agentInvoked: true, finishedAt: "2026-08-01 01:00:00"})
	secondJob := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{gitRef: "second", status: JobStatusDone, source: JobSourceCI, role: PanelRoleMember, agentInvoked: true, finishedAt: "2026-08-01 02:00:00"})
	thirdJob := seedExportCICostJob(t, db, repo.ID, ciCostJobSeed{gitRef: "third", status: JobStatusDone, source: JobSourceCI, role: PanelRoleMember, agentInvoked: true, finishedAt: "2026-08-01 03:00:00"})

	first, err := db.ExportCICosts(ExportCICostOptions{Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{firstJob.UUID, secondJob.UUID}, costJobUUIDs(first.Jobs))
	assert.True(t, first.Truncated)
	require.NotNil(t, first.NextCursor)
	assert.Regexp(t, `^[A-Za-z0-9_-]+$`, *first.NextCursor)

	second, err := db.ExportCICosts(ExportCICostOptions{Cursor: *first.NextCursor, Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{thirdJob.UUID}, costJobUUIDs(second.Jobs))
	assert.False(t, second.Truncated)
	require.NotNil(t, second.NextCursor)

	_, err = db.ExportCICosts(ExportCICostOptions{Cursor: *first.NextCursor, Legacy: true})
	require.ErrorIs(t, err, ErrExportCICostCursorModeMismatch)

	foreign, err := json.Marshal(ciCostCursor{
		Version: ciCostCursorVersion, DatabaseID: "foreign-database",
		FinishedAt: "2026-08-01T02:00:00Z", JobID: secondJob.ID,
	})
	require.NoError(t, err)
	_, err = db.ExportCICosts(ExportCICostOptions{
		Cursor: base64.RawURLEncoding.EncodeToString(foreign),
	})
	require.ErrorIs(t, err, ErrExportCursorDatabaseMismatch)

	_, err = db.Exec(`DELETE FROM review_jobs WHERE id = ?`, secondJob.ID)
	require.NoError(t, err)
	_, err = db.ExportCICosts(ExportCICostOptions{Cursor: *first.NextCursor})
	require.ErrorIs(t, err, ErrExportCursorNotFound)
}

func TestExportCICostsIncludesRetiredRetryJobs(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	members := []EnqueueOpts{{RepoID: repo.ID, GitRef: "base..head", Agent: "agent-a"}}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "base..head", Agent: "synthesis-agent"}

	created, firstMembers, _, err := db.CreateCIPanelRun("owner/project", 1, "head", members, synthesis)
	require.NoError(t, err)
	require.True(t, created)
	require.Len(t, firstMembers, 1)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'failed', started_at = '2026-08-01 01:00:00',
		finished_at = '2026-08-01 01:01:00', agent_invoked = 1,
		token_usage = '{"has_cost":true,"cost_usd":0.5}', source = NULL WHERE id = ?`, firstMembers[0].ID)
	require.NoError(t, err)
	firstPanel, err := db.GetCIPanelByPRSHA("owner/project", 1, "head")
	require.NoError(t, err)
	require.NoError(t, db.MarkPanelRetired(firstPanel.ID))

	created, replacementMembers, _, err := db.CreateCIPanelRun("owner/project", 1, "head", members, synthesis)
	require.NoError(t, err)
	require.True(t, created)
	require.Len(t, replacementMembers, 1)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'done', started_at = '2026-08-01 02:00:00',
		finished_at = '2026-08-01 02:01:00', agent_invoked = 1,
		token_usage = '{"has_cost":true,"cost_usd":0.25}' WHERE id = ?`, replacementMembers[0].ID)
	require.NoError(t, err)

	page, err := db.ExportCICosts(ExportCICostOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{firstMembers[0].UUID, replacementMembers[0].UUID}, costJobUUIDs(page.Jobs))
}

func seedLegacyCICostJob(t *testing.T, db *DB, repoID int64, gitRef, agent, enqueued, finished, usage string) *ReviewJob {
	t.Helper()
	job := seedLegacyCIJob(t, db, repoID, gitRef, agent, enqueued, finished)
	_, err := db.Exec(`UPDATE review_jobs SET agent_invoked = 1, token_usage = ? WHERE id = ?`, usage, job.ID)
	require.NoError(t, err)
	return job
}

func TestExportCICostsLegacyInfersDoneJobInvocation(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedPanelEraMarker(t, db, "2026-06-01 00:00:00")

	doneA := seedLegacyCIJob(t, db, repo.ID, "base..legacy", "agent-a",
		"2026-03-01 10:00:00", "2026-03-01 10:05:00")
	doneB := seedLegacyCIJob(t, db, repo.ID, "base..legacy", "agent-b",
		"2026-03-01 10:00:10", "2026-03-01 10:06:00")
	failed := seedLegacyCIJob(t, db, repo.ID, "base..legacy", "agent-c",
		"2026-03-01 10:00:20", "2026-03-01 10:07:00")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'failed' WHERE id = ?`, failed.ID)
	require.NoError(t, err)

	page, err := db.ExportCICosts(ExportCICostOptions{Legacy: true})
	require.NoError(t, err)
	assert.Equal(t, []string{doneA.UUID, doneB.UUID}, costJobUUIDs(page.Jobs))
	require.Len(t, page.Jobs, 2)
	assert.Nil(t, page.Jobs[0].CostUSD)
	assert.Nil(t, page.Jobs[1].CostUSD)
}

func TestExportCICostsLegacyReconstructionAndPagination(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedPanelEraMarker(t, db, "2026-06-01 00:00:00")

	jobA := seedLegacyCICostJob(t, db, repo.ID, "base..head-a", "agent-a", "2026-03-01 10:00:00", "2026-03-01 10:05:00", `{"has_cost":true,"cost_usd":0.4}`)
	jobB := seedLegacyCICostJob(t, db, repo.ID, "base..head-a", "agent-b", "2026-03-01 10:00:10", "2026-03-01 10:06:00", `{}`)
	_, err := db.Exec(`UPDATE review_jobs SET status = 'failed' WHERE id = ?`, jobB.ID)
	require.NoError(t, err)
	jobC := seedLegacyCICostJob(t, db, repo.ID, "base..head-b", "agent-a", "2026-03-02 10:00:00", "2026-03-02 10:05:00", `{"has_cost":true,"cost_usd":0}`)
	jobD := seedLegacyCICostJob(t, db, repo.ID, "base..head-b", "agent-b", "2026-03-02 10:00:10", "2026-03-02 10:06:00", `{"total_output_tokens":3}`)
	seedLegacyCICostJob(t, db, repo.ID, "base..singleton", "agent-a", "2026-03-03 10:00:00", "2026-03-03 10:05:00", `{"has_cost":true,"cost_usd":2}`)
	seedLegacyCICostJob(t, db, repo.ID, "base..head-a", "agent-a", "2026-03-13 10:00:00", "2026-03-13 10:05:00", `{"has_cost":true,"cost_usd":3}`)
	seedLegacyCICostJob(t, db, repo.ID, "base..after-era", "agent-a", "2026-07-01 10:00:00", "2026-07-01 10:05:00", `{"has_cost":true,"cost_usd":4}`)
	seedLegacyCICostJob(t, db, repo.ID, "base..after-era", "agent-b", "2026-07-01 10:00:10", "2026-07-01 10:06:00", `{"has_cost":true,"cost_usd":4}`)

	first, err := db.ExportCICosts(ExportCICostOptions{Legacy: true, Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{jobA.UUID, jobB.UUID}, costJobUUIDs(first.Jobs))
	assert.True(t, first.Truncated)
	require.NotNil(t, first.NextCursor)
	assert.Regexp(t, `^[A-Za-z0-9_-]+$`, *first.NextCursor)
	for _, job := range first.Jobs {
		assert.Equal(t, "review", job.Role)
	}
	require.NotNil(t, first.Jobs[0].CostUSD)
	assert.InDelta(t, 0.4, *first.Jobs[0].CostUSD, 1e-12)
	assert.Nil(t, first.Jobs[1].CostUSD)

	second, err := db.ExportCICosts(ExportCICostOptions{Legacy: true, Cursor: *first.NextCursor, Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{jobC.UUID, jobD.UUID}, costJobUUIDs(second.Jobs))
	assert.False(t, second.Truncated)
	require.NotNil(t, second.Jobs[0].CostUSD)
	assert.Zero(t, *second.Jobs[0].CostUSD)
	assert.Nil(t, second.Jobs[1].CostUSD)

	regular, err := db.ExportCICosts(ExportCICostOptions{})
	require.NoError(t, err)
	assert.Empty(t, regular.Jobs)
}
