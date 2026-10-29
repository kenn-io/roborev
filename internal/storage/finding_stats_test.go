package storage

import (
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/rebuttal"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestFindingStatsSyncedRebuttal(t *testing.T) {
	t.Parallel()
	checks := assert.New(t)
	source, target := openTestDB(t), openTestDB(t)
	t.Cleanup(func() { source.Close(); target.Close() })
	repo := createRepo(t, source, "/tmp/outcome-repo")
	peerRepo := createRepo(t, target, "/tmp/outcome-repo")
	// Local numeric IDs differ, and sync intentionally omits parent_job_id.
	unrelated := findingStatsReview(t, target, peerRepo.ID, JobTypeTask, nil)
	parent := findingStatsReview(t, source, repo.ID, JobTypeReview, []structuredreview.Finding{{Severity: "high", Problem: "unique bug", Fix: "repair"}})
	review, err := source.GetReviewByJobID(parent)
	require.NoError(t, err)
	const assessmentFinished = "2026-10-20T12:00:00.123456789Z"
	statsLabel(t, source, parent, findingVersionText(review), "unique bug", "confirmed", assessmentFinished)
	machine, err := source.GetMachineID()
	require.NoError(t, err)
	jobs, err := source.GetJobsToSync(machine, 10)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	for _, j := range jobs {
		require.NoError(t, target.UpsertPulledJob(PulledJob{
			UUID: j.UUID, GitRef: j.GitRef, Branch: j.Branch, Agent: j.Agent,
			JobType: j.JobType, Source: j.Source, Status: j.Status,
			EnqueuedAt: j.EnqueuedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
			Prompt: j.Prompt, SourceMachineID: j.SourceMachineID, UpdatedAt: j.UpdatedAt,
		}, peerRepo.ID, nil))
		require.NoError(t, source.MarkJobsSynced([]JobSyncMark{NewJobSyncMark(j)}))
	}
	reviews, err := source.GetReviewsToSync(machine, 10)
	require.NoError(t, err)
	require.Len(t, reviews, 2)
	for _, r := range reviews {
		require.NoError(t, target.UpsertPulledReview(PulledReview{
			UUID: r.UUID, JobUUID: r.JobUUID, JobEnqueuedAt: r.JobEnqueuedAt,
			JobSourceMachineID: r.JobSourceMachineID, Agent: r.Agent, Output: r.Output,
			StructuredOutput: r.StructuredOutput, VerdictBool: r.VerdictBool,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, UpdatedByMachineID: r.UpdatedByMachineID,
		}))
	}
	var localParent sql.NullInt64
	require.NoError(t, target.QueryRow("SELECT parent_job_id FROM review_jobs WHERE source='rebuttal'").Scan(&localParent))
	checks.False(localParent.Valid)
	var finished string
	require.NoError(t, target.QueryRow("SELECT finished_at FROM review_jobs WHERE source='rebuttal'").Scan(&finished))
	checks.Equal(assessmentFinished, finished, "sync preserves assessment completion precision")
	stats, err := target.GetFindingStats(SummaryOptions{RepoPath: peerRepo.RootPath, Branch: "topic"}, "severity")
	require.NoError(t, err)
	checks.Equal(1, stats.Total.Positive)
	checks.Equal(1, stats.Rebuttals.Valid)
	require.Len(t, stats.Evidence, 1)
	checks.Equal(findingVersionText(review), stats.Evidence[0].ReviewUUID)
	checks.Equal("confirmed", stats.Evidence[0].Outcome)

	otherRepo := createRepo(t, target, "/tmp/other-repo")
	for _, tc := range []struct {
		name    string
		parent  *int64
		repoID  int64
		invalid int
	}{
		{"explicit unrelated parent", &unrelated, peerRepo.ID, 0},
		{"different repository", nil, otherRepo.ID, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			_, err := target.Exec("UPDATE review_jobs SET parent_job_id=?,repo_id=? WHERE source='rebuttal'", tc.parent, tc.repoID)
			require.NoError(t, err)
			stats, err := target.GetFindingStats(SummaryOptions{RepoPath: peerRepo.RootPath, Branch: "topic"}, "severity")
			require.NoError(t, err)
			assert.Equal(0, stats.Total.Positive)
			assert.Equal(1, stats.Total.Unlabeled)
			assert.Equal(tc.invalid, stats.Rebuttals.Invalid)
		})
	}
}

type findingAssessmentQueryCounter struct {
	querier
	queries int
}

func (q *findingAssessmentQueryCounter) Query(query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "'rebuttal'") {
		q.queries++
	}
	return q.querier.Query(query, args...)
}

func TestFindingStatsScansAssessmentsOnce(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/outcome-repo")
	for range 3 {
		parent := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{{Severity: "high", Problem: "unique bug", Fix: "repair"}})
		review, err := db.GetReviewByJobID(parent)
		require.NoError(t, err)
		statsLabel(t, db, parent, findingVersionText(review), "unique bug", "confirmed", "2026-10-20 12:00:00")
	}
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	q := &findingAssessmentQueryCounter{querier: tx}
	stats, err := findingStats(q, SummaryOptions{AsOf: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}, "severity")
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Total.Positive)
	assert.Equal(t, 1, q.queries, "selected reviews share one assessment scan")
}

func TestFindingStatsPendingRebuttalsAreNotInvalid(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/outcome-repo")
	parent := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{{Severity: "high", Problem: "unique bug", Fix: "repair"}})
	review, err := db.GetReviewByJobID(parent)
	require.NoError(t, err)
	for _, status := range []string{"queued", "running"} {
		task := statsLabel(t, db, parent, findingVersionText(review), "unique bug", "confirmed", "2026-10-20 12:00:00")
		_, err = db.Exec("UPDATE review_jobs SET status=?,finished_at=NULL WHERE id=?", status, task)
		require.NoError(t, err)
	}
	stats, err := db.GetFindingStats(SummaryOptions{}, "severity")
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Total.Unlabeled)
	assert.Equal(t, RebuttalCoverage{}, stats.Rebuttals)
}

func findingStatsReview(t *testing.T, db *DB, repoID int64, typ string, fs []structuredreview.Finding) int64 {
	t.Helper()
	j, err := db.EnqueueJob(EnqueueOpts{RepoID: repoID, GitRef: "a..b", JobType: typ, Agent: "logical-agent", Model: "logical-model", Branch: "topic"})
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET status='running', started_at='2026-10-20 09:00:00', enqueued_at='2026-10-01 09:00:00' WHERE id=?", j.ID)
	require.NoError(t, err)
	require.NoError(t, completeReviewFixture(db, j.ID, "display-agent", "prompt", "- High — Bug"))
	raw, err := json.Marshal(structuredreview.Document{SchemaVersion: 2, Summary: "review", Verdict: "fail", Findings: fs})
	require.NoError(t, err)
	_, err = db.Exec("UPDATE reviews SET structured_output=? WHERE job_id=?", string(raw), j.ID)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET finished_at='2026-10-20 10:00:00',agent_invoked=1,token_usage=? WHERE id=?", `{"has_cost":true,"cost_usd":2}`, j.ID)
	require.NoError(t, err)
	return j.ID
}

func findingVersionText(review *Review) string {
	return review.UUID.String() //nolint:forbidigo // The stored selection prompt is a text boundary.
}

func statsLabel(t *testing.T, db *DB, parent int64, version, excerpt, outcome, finished string) int64 {
	t.Helper()
	prompt, err := rebuttal.EncodeSelection(rebuttal.Selection{ReviewUUID: version, Finding: excerpt}, "instructions")
	require.NoError(t, err)
	j, err := db.EnqueueJob(EnqueueOpts{RepoID: createRepo(t, db, "/tmp/outcome-repo").ID, GitRef: "a..b", JobType: JobTypeTask, Agent: "test", ParentJobID: parent, Source: JobSourceRebuttal, Prompt: prompt})
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET status='running',enqueued_at='2026-10-20 11:00:00',started_at='2026-10-20 11:00:00' WHERE id=?", j.ID)
	require.NoError(t, err)
	require.NoError(t, completeReviewFixture(db, j.ID, "test", prompt, `{"outcome":"`+outcome+`","explanation":"verified evidence"}`))
	_, err = db.Exec("UPDATE review_jobs SET finished_at=? WHERE id=?", finished, j.ID)
	require.NoError(t, err)
	return j.ID
}

func TestFindingStatsOutcomes(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/outcome-repo")
	fs := []structuredreview.Finding{{Severity: "high", Problem: "unique bug", Fix: "repair"}, {Severity: "low", Problem: "second bug", Fix: "repair"}}
	id := findingStatsReview(t, db, repo.ID, JobTypeReview, fs)
	rv, err := db.GetReviewByJobID(id)
	require.NoError(t, err)
	statsLabel(t, db, id, findingVersionText(rv), "unique\n bug", "confirmed", "2026-10-20 12:00:00")
	statsLabel(t, db, id, "stale-version", "unique bug", "refuted", "2026-10-20 13:00:00")
	statsLabel(t, db, id, findingVersionText(rv), "second bug", "superseded", "2026-10-20 14:00:00")
	require.NoError(t, db.MarkReviewClosedByJobID(id, true))
	opts := SummaryOptions{Since: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), AsOf: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), Findings: true, MinSamples: 5}
	s, err := db.GetSummary(opts)
	require.NoError(t, err)
	assert.Equal(t, 3, s.Overview.Total, "only rebuttal tasks were enqueued in the old summary window")
	f := s.Findings
	require.NotNil(t, f)
	assert.Equal(t, 1, f.LogicalReviews)
	assert.Equal(t, 1, f.StructuredReviews)
	assert.Equal(t, 2, f.Total.Findings)
	assert.Equal(t, 1, f.Total.Positive)
	assert.Equal(t, 1, f.Total.Excluded)
	assert.Equal(t, 1, f.Total.Labeled)
	assert.Equal(t, 0, f.Total.Unlabeled)
	assert.Nil(t, f.Total.ActedOnRate)
	assert.Equal(t, "below_minimum", f.Total.RateState)
	assert.Equal(t, 1, f.Total.FailingReviews)
	assert.Equal(t, 1, f.Total.ClosedReviews)
	require.Len(t, f.Rows, 2)
	assert.Equal(t, "high", f.Rows[0].Key)
	assert.Equal(t, 1, f.Rows[0].Cost.JobsTotal)
	assert.InDelta(t, 2.0, f.Rows[0].Cost.TotalUSD, 0.0001)
	require.Len(t, f.Evidence, 2)
	assert.Equal(t, "confirmed", f.Evidence[0].Outcome)
	for by, key := range map[string]string{"review-type": "default", "agent": "logical-agent", "model": "logical-model", "arm": "unassigned"} {
		grouped, err := db.GetFindingStats(opts, by)
		require.NoError(t, err)
		require.Len(t, grouped.Rows, 1)
		assert.Equal(t, key, grouped.Rows[0].Key)
		assert.Equal(t, 2, grouped.Rows[0].Findings)
		assert.InDelta(t, 2.0, grouped.Rows[0].Cost.TotalUSD, 0.0001)
	}
}

func TestFindingStatsPopulationAndCoverage(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/outcome-repo")
	fs := []structuredreview.Finding{{Severity: "low", Problem: "bug", Fix: "repair"}}
	for _, typ := range []string{JobTypeReview, JobTypeRange, JobTypeDirty, JobTypeSynthesis, JobTypeTask, JobTypeFix, JobTypeInsights, JobTypeCompact, JobTypeGoalReview} {
		findingStatsReview(t, db, repo.ID, typ, fs)
	}
	empty := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{})
	legacy := findingStatsReview(t, db, repo.ID, JobTypeReview, fs)
	_, err := db.Exec("UPDATE reviews SET structured_output=NULL WHERE job_id=?", legacy)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET panel_role='member' WHERE id=?", empty)
	require.NoError(t, err)
	f, err := db.GetFindingStats(SummaryOptions{}, "severity")
	require.NoError(t, err)
	assert.Equal(t, 5, f.LogicalReviews)
	assert.Equal(t, 4, f.StructuredReviews)
	assert.Equal(t, 4, f.Total.Findings)
	f, err = db.GetFindingStats(SummaryOptions{Branch: "absent"}, "agent")
	require.NoError(t, err)
	assert.Zero(t, f.LogicalReviews)
	_, err = db.GetFindingStats(SummaryOptions{}, "bad")
	require.Error(t, err)
}

func TestFindingStatsReviewTypeAliasesShareCell(t *testing.T) {
	t.Parallel()
	checks := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/outcome-repo")
	// Storage and sync retain the legacy names for the default review type.
	for _, reviewType := range []string{"", "default", "general", "review"} {
		parent := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{
			{Severity: "high", Problem: "lost result", Fix: "persist before return"},
		})
		_, err := db.Exec("UPDATE review_jobs SET review_type=? WHERE id=?", reviewType, parent)
		require.NoError(t, err)
		review, err := db.GetReviewByJobID(parent)
		require.NoError(t, err)
		statsLabel(t, db, parent, findingVersionText(review), "lost result", "confirmed", "2026-10-20 12:00:00")
	}
	stats, err := db.GetFindingStats(SummaryOptions{MinSamples: 3}, "review-type")
	require.NoError(t, err)
	require.Len(t, stats.Rows, 1, "default review aliases share one outcome denominator")
	checks.Equal("default", stats.Rows[0].Key)
	checks.Equal(4, stats.Rows[0].Labeled)
	checks.Equal("available", stats.Rows[0].RateState)
	require.NotNil(t, stats.Rows[0].ActedOnRate)
	checks.InDelta(1.0, *stats.Rows[0].ActedOnRate, 0.0001)
}

func FuzzFindingConservation(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, labels []byte) {
		var c FindingOutcomeCounts
		outcomes := []string{"confirmed", "refuted", "superseded", "unlabeled"}
		for _, label := range labels {
			addFindingOutcome(&c, outcomes[int(label)%len(outcomes)])
		}
		finishFindingCounts(&c, 5)
		assert.Equal(t, len(labels), c.Findings)
		assert.Equal(t, c.Findings, c.Labeled+c.Excluded+c.Unlabeled)
		assert.Equal(t, c.Positive+c.Negative, c.Labeled)
		if c.ActedOnRate != nil {
			assert.GreaterOrEqual(t, *c.ActedOnRate, 0.0)
			assert.LessOrEqual(t, *c.ActedOnRate, 1.0)
		}
	})
}

func TestFindingStatsLatestValidAndIneligibleLabels(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/outcome-repo")
	fs := []structuredreview.Finding{{Severity: "high", Problem: "unique bug", Fix: "shared repair"}, {Severity: "low", Problem: "other bug", Fix: "shared repair"}}
	id := findingStatsReview(t, db, repo.ID, JobTypeReview, fs)
	rv, err := db.GetReviewByJobID(id)
	require.NoError(t, err)
	latest := statsLabel(t, db, id, findingVersionText(rv), "unique bug", "refuted", "2026-10-20 12:00:00")
	first := statsLabel(t, db, id, findingVersionText(rv), "unique bug", "confirmed", "2026-10-20 12:00:00")
	_, err = db.Exec("UPDATE review_jobs SET uuid=? WHERE id=?", "00000000-0000-4000-8000-000000000001", first)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET uuid=? WHERE id=?", "00000000-0000-4000-8000-000000000002", latest)
	require.NoError(t, err)
	bad := statsLabel(t, db, id, findingVersionText(rv), "unique bug", "confirmed", "2026-10-20 13:00:00")
	_, err = db.Exec("UPDATE reviews SET output='invalid assessment' WHERE job_id=?", bad)
	require.NoError(t, err)
	statsLabel(t, db, id, findingVersionText(rv), "shared repair", "confirmed", "2026-10-20 13:00:00")
	unbound := statsLabel(t, db, id, findingVersionText(rv), "unique bug", "confirmed", "2026-10-20 14:00:00")
	require.NoError(t, db.SaveJobPrompt(unbound, "instructions\nOriginal finding: counterfeit marker"))
	failed := statsLabel(t, db, id, findingVersionText(rv), "unique bug", "confirmed", "2026-10-20 15:00:00")
	_, err = db.Exec("UPDATE review_jobs SET status='failed' WHERE id=?", failed)
	require.NoError(t, err)
	early := statsLabel(t, db, id, findingVersionText(rv), "unique bug", "confirmed", "2026-10-20 09:00:00")
	_, err = db.Exec("UPDATE review_jobs SET enqueued_at='2026-10-20 08:00:00',started_at='2026-10-20 08:00:00' WHERE id=?", early)
	require.NoError(t, err)
	statsLabel(t, db, id, findingVersionText(rv), "unique bug", "confirmed", "2026-10-22 16:00:00")
	ci := findingStatsReview(t, db, repo.ID, JobTypeReview, fs)
	ciRV, err := db.GetReviewByJobID(ci)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET source='ci' WHERE id=?", ci)
	require.NoError(t, err)
	statsLabel(t, db, ci, findingVersionText(ciRV), "unique bug", "confirmed", "2026-10-20 12:00:00")
	panel := findingStatsReview(t, db, repo.ID, JobTypeSynthesis, fs)
	panelRV, err := db.GetReviewByJobID(panel)
	require.NoError(t, err)
	statsLabel(t, db, panel, findingVersionText(panelRV), "unique bug", "confirmed", "2026-10-20 12:00:00")
	f, err := db.GetFindingStats(SummaryOptions{AsOf: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), MinSamples: 1}, "severity")
	require.NoError(t, err)
	assert.Equal(t, 1, f.Total.Negative)
	assert.Zero(t, f.Total.Positive)
	assert.Equal(t, 5, f.Total.Unlabeled)
	require.NotNil(t, f.Total.ActedOnRate)
	assert.Zero(t, *f.Total.ActedOnRate)
	assert.Equal(t, 2, f.Rebuttals.Valid)
	assert.Equal(t, 1, f.Rebuttals.Ambiguous)
	assert.Equal(t, 1, f.Rebuttals.Unbound)
	assert.Equal(t, 3, f.Rebuttals.Invalid)
	assert.Equal(t, 2, f.Rebuttals.Ineligible)
	assert.Equal(t, latest, f.Evidence[0].AssessmentJobID)
	assert.Equal(t, 2, f.TierCoverage["R"].Findings)
	assert.Equal(t, 1, f.TierCoverage["R"].Negative)
	statsLabel(t, db, id, findingVersionText(rv), "unique bug", "superseded", "2026-10-20 16:00:00")
	f, err = db.GetFindingStats(SummaryOptions{AsOf: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}, "severity")
	require.NoError(t, err)
	assert.Zero(t, f.Total.Labeled)
	assert.Zero(t, f.Total.Negative)
	assert.Equal(t, 1, f.Total.Excluded)
}

func TestFindingStatsDocumentCoverageAndFilters(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/outcome-repo")
	docs := []*string{nil, new(`{`), new(`{"schema_version":0,"legacy":{"markdown":"historical","recorded_verdict":null}}`), new(`{"schema_version":2,"summary":"cannot assess","verdict":"unable_to_review","findings":[]}`), new(`{"schema_version":2,"summary":"clean","verdict":"pass","findings":[]}`)}
	for _, doc := range docs {
		id := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{})
		_, err := db.Exec("UPDATE reviews SET structured_output=? WHERE job_id=?", doc, id)
		require.NoError(t, err)
	}
	f, err := db.GetFindingStats(SummaryOptions{RepoPath: repo.RootPath, Branch: "topic"}, "agent")
	require.NoError(t, err)
	assert.Equal(t, 5, f.LogicalReviews)
	assert.Equal(t, 1, f.StructuredReviews)
	assert.Equal(t, 1, f.MissingDocuments)
	assert.Equal(t, 1, f.InvalidDocuments)
	assert.Equal(t, 1, f.LegacyDocuments)
	assert.Equal(t, 1, f.UnableDocuments)
	require.Len(t, f.Rows, 1)
	assert.Equal(t, 5, f.Rows[0].Reviews)
	assert.Zero(t, f.Total.Findings)
	assert.Nil(t, f.Total.ActedOnRate)
	f, err = db.GetFindingStats(SummaryOptions{RepoPath: "/tmp/other-repo"}, "agent")
	require.NoError(t, err)
	assert.Zero(t, f.LogicalReviews)
	f, err = db.GetFindingStats(SummaryOptions{AsOf: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}, "agent")
	require.NoError(t, err)
	assert.Zero(t, f.LogicalReviews)
	f, err = db.GetFindingStats(SummaryOptions{Since: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}, "agent")
	require.NoError(t, err)
	assert.Zero(t, f.LogicalReviews)
}

func TestFindingStatsWholePanelRetainedCost(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/outcome-repo")
	parent := findingStatsReview(t, db, repo.ID, JobTypeSynthesis, []structuredreview.Finding{})
	run := "00000000-0000-4000-8000-000000000011"
	_, err := db.Exec("UPDATE review_jobs SET panel_run_uuid=?,panel_role='synthesis',agent_invoked=0,token_usage=NULL WHERE id=?", run, parent)
	require.NoError(t, err)
	for i := range 3 {
		member := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{})
		_, err = db.Exec("UPDATE review_jobs SET panel_run_uuid=?,panel_role='member',non_voting=?,status='failed',token_usage=NULL WHERE id=?", run, i == 1, member)
		require.NoError(t, err)
		switch i {
		case 0:
			_, err = db.Exec("UPDATE review_jobs SET token_usage=? WHERE id=?", `{"has_cost":true,"cost_usd":0}`, member)
		case 2:
			_, err = db.Exec("UPDATE review_jobs SET agent_invoked=0 WHERE id=?", member)
		}
		require.NoError(t, err)
	}
	f, err := db.GetFindingStats(SummaryOptions{}, "agent")
	require.NoError(t, err)
	assert.Equal(t, 1, f.LogicalReviews)
	require.Len(t, f.Rows, 1)
	assert.Equal(t, "logical-agent", f.Rows[0].Key, "passthrough review display-agent cannot replace parent configuration")
	assert.Equal(t, 2, f.Rows[0].Cost.JobsTotal)
	assert.Equal(t, 1, f.Rows[0].Cost.JobsWithCost)
	assert.Zero(t, f.Rows[0].Cost.TotalUSD)
	assert.False(t, f.Rows[0].Cost.Complete)
	assert.Nil(t, f.Rows[0].CostPerPositive)
	f, err = db.GetFindingStats(SummaryOptions{}, "severity")
	require.NoError(t, err)
	assert.Empty(t, f.Rows)
	assert.Equal(t, 2, f.Cost.JobsTotal)
}

func TestFindingStatsCompletionOrderKeepsSubsecondPrecision(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		db := openTestDB(t)
		defer db.Close()
		repo := createRepo(t, db, "/tmp/outcome-repo")
		id := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{{Severity: "high", Problem: "bug", Fix: "repair"}})
		_, err := db.Exec("UPDATE review_jobs SET enqueued_at=?,started_at=?,finished_at=? WHERE id=?",
			preciseTimestampAt(time.Now().Add(-2*time.Second)), preciseTimestampAt(time.Now().Add(-2*time.Second)), preciseTimestampAt(time.Now().Add(-time.Second)), id)
		require.NoError(t, err)
		rv, err := db.GetReviewByJobID(id)
		require.NoError(t, err)
		prompt, err := rebuttal.EncodeSelection(rebuttal.Selection{ReviewUUID: findingVersionText(rv), Finding: "bug"}, "instructions")
		require.NoError(t, err)
		var jobs []int64
		var completed []time.Time
		for _, tc := range []struct{ outcome, uuid string }{
			{"confirmed", "00000000-0000-4000-8000-000000000099"},
			{"refuted", "00000000-0000-4000-8000-000000000001"},
		} {
			job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "a..b", JobType: JobTypeTask, Agent: "test", ParentJobID: id, Source: JobSourceRebuttal, Prompt: prompt})
			require.NoError(t, err)
			// SQLite's enqueue default uses its real clock outside synctest.
			_, err = db.Exec("UPDATE review_jobs SET uuid=?,enqueued_at=? WHERE id=?", tc.uuid, preciseTimestampAt(time.Now()), job.ID)
			require.NoError(t, err)
			claimed, err := db.ClaimJob("worker")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, job.ID, claimed.ID)
			time.Sleep(100*time.Microsecond + time.Nanosecond)
			finished := time.Now()
			require.NoError(t, db.CompleteJob(job.ID, "test", `{"outcome":"`+tc.outcome+`","explanation":"verified evidence"}`))
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			require.NotNil(t, stored.FinishedAt)
			assert.Equal(finished.UnixNano(), stored.FinishedAt.UnixNano(), "CompleteJob must retain assessment precision")
			jobs = append(jobs, job.ID)
			completed = append(completed, finished)
		}
		assert.Equal(completed[0].Truncate(time.Second), completed[1].Truncate(time.Second))
		first, err := db.GetFindingStats(SummaryOptions{AsOf: completed[0]}, "severity")
		require.NoError(t, err)
		require.Len(t, first.Evidence, 1)
		assert.Equal(1, first.Total.Positive)
		assert.Equal(jobs[0], first.Evidence[0].AssessmentJobID)
		latest, err := db.GetFindingStats(SummaryOptions{AsOf: completed[1]}, "severity")
		require.NoError(t, err)
		require.Len(t, latest.Evidence, 1)
		assert.Equal(2, latest.Rebuttals.Valid)
		assert.Equal(1, latest.Total.Negative)
		assert.Equal(jobs[1], latest.Evidence[0].AssessmentJobID)
	})
}

func TestFindingStatsExactAsOfAndDistinctExecutionRows(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/outcome-repo")
	asOf := time.Date(2026, 10, 20, 10, 0, 0, 1, time.UTC)
	parent := findingStatsReview(t, db, repo.ID, JobTypeSynthesis, []structuredreview.Finding{
		{Severity: "high", Problem: "bug", Fix: "repair"},
		{Severity: "high", Problem: "another bug", Fix: "repair"},
	})
	run := "00000000-0000-4000-8000-000000000011"
	_, err := db.Exec("UPDATE review_jobs SET panel_run_uuid=?,panel_role='synthesis',agent_invoked=0,token_usage=NULL WHERE id=?", run, parent)
	require.NoError(t, err)
	member := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{})
	_, err = db.Exec("UPDATE review_jobs SET panel_run_uuid=?,panel_role='member' WHERE id=?", run, member)
	require.NoError(t, err)
	future := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{})
	_, err = db.Exec("UPDATE review_jobs SET finished_at='2026-10-20T10:00:00.000000002Z' WHERE id=?", future)
	require.NoError(t, err)
	f, err := db.GetFindingStats(SummaryOptions{AsOf: asOf}, "severity")
	require.NoError(t, err)
	assert.Equal(t, 1, f.LogicalReviews)
	assert.Equal(t, 2, f.Total.Findings)
	assert.Equal(t, 1, f.Cost.JobsTotal)
	assert.InDelta(t, 2.0, f.Cost.TotalUSD, 0.0001)
	require.Len(t, f.Rows, 1)
	assert.Equal(t, 1, f.Rows[0].Cost.JobsTotal)
	_, err = db.Exec("UPDATE review_jobs SET finished_at='2026-10-20T10:00:00.000000002Z' WHERE id=?", member)
	require.NoError(t, err)
	f, err = db.GetFindingStats(SummaryOptions{AsOf: asOf}, "severity")
	require.NoError(t, err)
	assert.Zero(t, f.Cost.JobsTotal)
}

func TestFindingStatsQualifiedArmGroupsDoNotMultiplyTotals(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/outcome-repo")
	machineID, err := db.GetMachineID()
	require.NoError(t, err)
	for _, definition := range []struct{ id, hash string }{{"alpha", "hash-one"}, {"beta", "hash-two"}} {
		id := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{{Severity: "low", Problem: "below threshold", Fix: "repair"}})
		_, err := db.Exec("UPDATE reviews SET verdict_bool=1,closed=1 WHERE job_id=?", id)
		require.NoError(t, err)
		job, err := db.GetJobByID(id)
		require.NoError(t, err)
		require.NotNil(t, job.UUID)
		require.NoError(t, insertExperimentAssignmentTx(t.Context(), db, ReviewUnitJob, *job.UUID, &ExperimentAssignmentInput{
			ExperimentID: definition.id, DefinitionHash: definition.hash, DefinitionJSON: "{}",
			Arm: "experiment", SubjectHash: "subject", EffectiveConfigHash: "config", EffectiveConfigJSON: "{}",
		}, machineID, time.Now()))
	}
	f, err := db.GetFindingStats(SummaryOptions{}, "arm")
	require.NoError(t, err)
	assert.Equal(t, 2, f.LogicalReviews)
	assert.Equal(t, 2, f.Total.Findings)
	assert.Equal(t, 2, f.Cost.JobsTotal)
	assert.Zero(t, f.Total.Positive)
	assert.Equal(t, 2, f.Total.Unlabeled)
	assert.Zero(t, f.Total.FailingReviews)
	require.Len(t, f.Rows, 2)
	assert.Equal(t, "alpha@hash-one/experiment", f.Rows[0].Key)
	assert.Equal(t, "beta@hash-two/experiment", f.Rows[1].Key)
	for _, row := range f.Rows {
		assert.Equal(t, 1, row.Findings)
		assert.Equal(t, 1, row.Cost.JobsTotal)
	}
}

func FuzzFindingCostConservation(f *testing.F) {
	f.Add(uint16(2), uint16(3), true)
	f.Fuzz(func(t *testing.T, first, second uint16, priced bool) {
		costs := []findingExecutionCost{{id: 1, cost: CostAggregate{JobsTotal: 1, JobsWithCost: 1, TotalUSD: float64(first)}}}
		other := CostAggregate{JobsTotal: 1}
		if priced {
			other.JobsWithCost = 1
			other.TotalUSD = float64(second)
		}
		costs = append(costs, findingExecutionCost{id: 2, cost: other})
		var total CostAggregate
		seen := map[int64]bool{}
		addDistinctFindingCost(&total, costs, seen)
		firstTotal := total
		addDistinctFindingCost(&total, costs, seen)
		assert.Equal(t, firstTotal, total, "repeated retained execution rows cannot multiply spend")
		assert.Equal(t, 2, total.JobsTotal)
		assert.LessOrEqual(t, total.JobsWithCost, total.JobsTotal)
		finishFindingCost(&total)
		assert.Equal(t, priced, total.Complete)
	})
}

func TestFindingRateDenominators(t *testing.T) {
	t.Parallel()
	counts := FindingOutcomeCounts{Findings: 10, Positive: 3, Negative: 2, Excluded: 4, Unlabeled: 1, FailingReviews: 3, ClosedReviews: 1}
	finishFindingCounts(&counts, 5)
	assert.Equal(t, 5, counts.Labeled)
	require.NotNil(t, counts.ActedOnRate)
	assert.InDelta(t, 0.6, *counts.ActedOnRate, 0.0001)
	require.NotNil(t, counts.ResolutionRate)
	assert.InDelta(t, 1.0/3, *counts.ResolutionRate, 0.0001)
	assert.Equal(t, "available", counts.RateState)
}

func TestFindingStatsLegacyJobSignals(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/outcome-repo")
	commit := createCommit(t, db, repo.ID, "legacy-sha")
	for _, kind := range []string{"commit", "range", "diff", "task"} {
		id := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{})
		_, err := db.Exec("UPDATE review_jobs SET job_type='',git_ref=? WHERE id=?", kind, id)
		require.NoError(t, err)
		switch kind {
		case "commit":
			_, err = db.Exec("UPDATE review_jobs SET commit_id=? WHERE id=?", commit.ID, id)
		case "range":
			_, err = db.Exec("UPDATE review_jobs SET git_ref='base..head' WHERE id=?", id)
		case "diff":
			_, err = db.Exec("UPDATE job_content SET diff_content=zstd_compress('snapshot diff') WHERE job_id=?", id)
		}
		require.NoError(t, err)
	}
	f, err := db.GetFindingStats(SummaryOptions{}, "agent")
	require.NoError(t, err)
	assert.Equal(t, 3, f.LogicalReviews)
	assert.Equal(t, 3, f.StructuredReviews)
}

func TestFindingStatsLargeStoredSelection(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/outcome-repo")
	finding := strings.Repeat("synthetic finding context ", 2048) + "unique ending"
	id := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{{Severity: "high", Problem: finding, Fix: "repair"}})
	rv, err := db.GetReviewByJobID(id)
	require.NoError(t, err)
	prompt, err := rebuttal.EncodeSelection(rebuttal.Selection{ReviewUUID: findingVersionText(rv), Finding: finding}, strings.Repeat("quoted synthetic explanation\n", 4096))
	require.NoError(t, err)
	task, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "a..b", JobType: JobTypeTask, Agent: "test", ParentJobID: id, Source: JobSourceRebuttal, Prompt: prompt})
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET status='running',enqueued_at='2026-10-20 11:00:00',started_at='2026-10-20 11:00:00' WHERE id=?", task.ID)
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(task.ID, "test", `{"outcome":"confirmed","explanation":"verified synthetic evidence"}`))
	_, err = db.Exec("UPDATE review_jobs SET finished_at='2026-10-20 12:00:00' WHERE id=?", task.ID)
	require.NoError(t, err)
	stats, err := db.GetFindingStats(SummaryOptions{}, "severity")
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Total.Positive)
	require.Len(t, stats.Evidence, 1)
	assert.Equal(t, finding, stats.Evidence[0].Finding.Problem)
	assert.Equal(t, task.ID, stats.Evidence[0].AssessmentJobID)
	assert.Equal(t, "verified synthetic evidence", stats.Evidence[0].Explanation)
}

func TestFindingStatsChronologyStoredPrecision(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/outcome-repo")
	parent := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{{Severity: "high", Problem: "unique bug", Fix: "repair"}})
	review, err := db.GetReviewByJobID(parent)
	require.NoError(t, err)
	task := statsLabel(t, db, parent, findingVersionText(review), "unique bug", "confirmed", "2026-10-20T12:00:00Z")
	const parentQueued = "2026-10-20T09:00:00Z"
	const parentFinished = "2026-10-20T10:00:00Z"
	const taskQueued = "2026-10-20T11:00:00Z"
	const taskStarted = "2026-10-20T11:00:00.250000000Z"
	const taskFinished = "2026-10-20T11:00:00Z"
	for _, tc := range []struct {
		name                                                    string
		parentQueued, parentFinished, queued, started, finished string
		positive                                                int
	}{
		{"rounded task completion", parentQueued, parentFinished, taskQueued, taskStarted, taskFinished, 1},
		{"rounded rerun parent completion", "2026-10-20T10:00:00.250000000Z", parentFinished, taskQueued, taskStarted, taskFinished, 1},
		{"rounded task enqueue after precise parent", parentQueued, "2026-10-20T11:00:00.250000000Z", taskQueued, "2026-10-20T11:00:00.500000000Z", taskFinished, 1},
		{"rounded claim in precise enqueue second", parentQueued, parentFinished, "2026-10-20T11:00:00.250000000Z", "2026-10-20T11:00:00Z", taskFinished, 1},
		{"task completion earlier second", parentQueued, parentFinished, taskQueued, taskStarted, "2026-10-20T10:59:59Z", 0},
		{"parent completion earlier second", "2026-10-20T10:00:01.250000000Z", parentFinished, taskQueued, taskStarted, taskFinished, 0},
		{"task enqueue earlier second", parentQueued, parentFinished, "2026-10-20T09:59:59Z", taskStarted, taskFinished, 0},
		{"claim earlier second", parentQueued, parentFinished, taskQueued, "2026-10-20T10:59:59.250000000Z", taskFinished, 0},
		{"precise task completion reversed", parentQueued, parentFinished, taskQueued, taskStarted, "2026-10-20T11:00:00.100000000Z", 0},
		{"precise zero task completion reversed", parentQueued, parentFinished, taskQueued, taskStarted, "2026-10-20T11:00:00.000000000Z", 0},
		{"precise zero parent completion reversed", "2026-10-20T10:00:00.250000000Z", "2026-10-20T10:00:00.000000000Z", taskQueued, taskStarted, taskFinished, 0},
		{"precise task enqueue reversed", parentQueued, "2026-10-20T11:00:00.500000000Z", "2026-10-20T11:00:00.250000000Z", taskStarted, taskFinished, 0},
		{"precise zero claim reversed", parentQueued, parentFinished, "2026-10-20T11:00:00.250000000Z", "2026-10-20T11:00:00.000000000Z", taskFinished, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			_, err := db.Exec("UPDATE review_jobs SET enqueued_at=?,started_at=?,finished_at=? WHERE id=?", tc.parentQueued, tc.parentQueued, tc.parentFinished, parent)
			require.NoError(t, err)
			_, err = db.Exec("UPDATE review_jobs SET enqueued_at=?,started_at=?,finished_at=? WHERE id=?", tc.queued, tc.started, tc.finished, task)
			require.NoError(t, err)
			stats, err := db.GetFindingStats(SummaryOptions{AsOf: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}, "severity")
			require.NoError(t, err)
			assert.Equal(1, stats.LogicalReviews, "parent remains in the completion-window population")
			assert.Equal(tc.positive, stats.Total.Positive)
			assert.Equal(tc.positive, stats.Rebuttals.Valid)
			assert.Equal(1-tc.positive, stats.Rebuttals.Invalid)
		})
	}
}
