package storage

import (
	"database/sql"
	"encoding/json/v2"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reportAssignment(arm, subject, plan string) *ExperimentAssignmentInput {
	return &ExperimentAssignmentInput{ExperimentID: "report-v1", DefinitionHash: "definition", DefinitionJSON: `{"ratio":0.5}`, Arm: arm, SubjectHash: subject, EffectiveConfigHash: "frozen", EffectiveConfigJSON: plan}
}

func reportJob(t *testing.T, db *DB, repoID int64, arm, subject string, cost *float64) *ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repoID, GitRef: subject, Agent: "test", Experiment: reportAssignment(arm, subject, `{"Agent":"test"}`)})
	require.NoError(t, err)
	reportFinish(t, db, job.ID, "done", cost, `{"schema_version":2,"summary":"clean","verdict":"pass","findings":[]}`)
	return job
}

func reportFinish(t *testing.T, db *DB, id int64, status string, cost *float64, output string) {
	t.Helper()
	usage := ""
	if cost != nil {
		b, err := json.Marshal(map[string]any{"has_cost": true, "cost_usd": *cost})
		require.NoError(t, err)
		usage = string(b)
	}
	_, err := db.Exec(`UPDATE review_jobs SET status=?, started_at='2026-01-01T00:00:00Z', finished_at='2026-01-01T00:00:10Z', agent_invoked=1, token_usage=? WHERE id=?`, status, usage, id)
	require.NoError(t, err)
	if output != "" {
		_, err = db.Exec(`INSERT INTO reviews(job_id,agent,output,structured_output,verdict_bool) VALUES(?,'test','',?,1)`, id, output)
		require.NoError(t, err)
	}
}

func reportMetric(t *testing.T, r *ExperimentReport, name string) ExperimentMetricComparison {
	t.Helper()
	for _, m := range r.Metrics {
		if m.Metric == name {
			return m
		}
	}
	require.Contains(t, r.Metrics, ExperimentMetricComparison{Metric: name})
	return ExperimentMetricComparison{}
}

func TestExperimentReportSubsecondCompletion(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"done", "failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				db, repo := setupDBAndRepo(t, "subsecond-duration")
				_, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "topic", Agent: "test", Experiment: reportAssignment("default", "topic", `{"Agent":"test"}`)})
				require.NoError(t, err)
				time.Sleep(250 * time.Millisecond)
				job, err := db.ClaimJob("worker")
				require.NoError(t, err)
				require.NotNil(t, job)
				time.Sleep(250 * time.Millisecond)
				switch status {
				case "done":
					require.NoError(t, db.CompleteJob(job.ID, "test", `{"schema_version":2,"summary":"clean","verdict":"pass","findings":[]}`))
				case "failed":
					changed, err := db.FailJob(job.ID, "worker", "agent failed")
					require.NoError(t, err)
					require.True(t, changed)
				case "canceled":
					require.NoError(t, db.CancelJob(job.ID))
				}
				r, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{})
				require.NoError(t, err)
				for _, name := range []string{"duration_p50_seconds", "duration_p90_seconds"} {
					metric := reportMetric(t, r, name)
					require.NotNil(t, metric.Default.Value)
					assert.InDelta(t, .25, *metric.Default.Value, 1e-12)
					assert.True(t, metric.Default.Complete)
				}
			})
		})
	}
}

func TestExperimentReportLegacyDurationResolution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, finish string
		available    bool
	}{
		{name: "same second rounded down", finish: "2026-01-01T00:00:00Z", available: true},
		{name: "earlier second", finish: "2025-12-31T23:59:59Z"},
		{name: "precise earlier finish", finish: "2026-01-01T00:00:00.100000000Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, repo := setupDBAndRepo(t, "legacy-duration")
			job := reportJob(t, db, repo.ID, "default", "topic", new(1.0))
			_, err := db.Exec(`UPDATE review_jobs SET started_at='2026-01-01T00:00:00.250000000Z', finished_at=? WHERE id=?`, tc.finish, job.ID)
			require.NoError(t, err)
			r, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{})
			require.NoError(t, err)
			metric := reportMetric(t, r, "duration_p50_seconds")
			assert.Equal(t, tc.available, metric.Default.Complete)
			if tc.available {
				require.NotNil(t, metric.Default.Value)
				assert.Zero(t, *metric.Default.Value)
			} else {
				assert.Nil(t, metric.Default.Value)
			}
		})
	}
}

func TestExperimentReportSubjectWeighting(t *testing.T) {
	t.Parallel()
	db, repo := setupDBAndRepo(t, "report-weight")
	for _, tc := range []struct {
		subject string
		cost    float64
		seconds int
	}{
		{"a", 1, 10}, {"a", 3, 30}, {"b", 8, 80},
	} {
		job := reportJob(t, db, repo.ID, "default", tc.subject, new(tc.cost))
		_, err := db.Exec(`UPDATE review_jobs SET finished_at=? WHERE id=?`, time.Date(2026, 1, 1, 0, 0, tc.seconds, 0, time.UTC).Format(time.RFC3339), job.ID)
		require.NoError(t, err)
	}
	r, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal(2, r.Default.Subjects)
	assert.Equal(3, r.Default.ReviewUnits)
	m := reportMetric(t, r, "cost_per_review")
	require.NotNil(t, m.Default.Value)
	assert.InDelta(5.0, *m.Default.Value, 1e-12)
	assert.Equal(2, m.Default.Subjects)
	assert.Equal(3, m.Default.Units)
	assert.Nil(m.Experiment.Value)
	assert.Equal("not enough data", m.State)
	assert.InDelta(50.0, *reportMetric(t, r, "duration_p50_seconds").Default.Value, 1e-12)
	assert.InDelta(54.0, *reportMetric(t, r, "duration_p90_seconds").Default.Value, 1e-12)
}

func TestExperimentReportPanelCompleteness(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"complete", "pending advisory", "missing advisory", "execution rates", "pending execution", "missing synthesis", "retained backup", "passthrough", "alias"} {
		t.Run(mode, func(t *testing.T) {
			db, repo := setupDBAndRepo(t, "report-panel")
			enqueue := func(subject, synthesisAgent string, advisory bool) ([]*ReviewJob, *ReviewJob) {
				run := testUUID(subject)
				planMembers := `{"Agent":"test","PanelMemberName":"voter","PanelMemberIndex":0}`
				opts := []EnqueueOpts{{RepoID: repo.ID, GitRef: "topic", Agent: "test", PanelRunUUID: &run, PanelMemberName: "voter"}}
				if advisory {
					planMembers += `,{"Agent":"test","PanelMemberName":"observer","PanelMemberIndex":1}`
					opts = append(opts, EnqueueOpts{RepoID: repo.ID, GitRef: "topic", Agent: "test", PanelRunUUID: &run, PanelMemberName: "observer", PanelMemberIndex: 1, NonVoting: true})
				}
				plan := `{"Members":[` + planMembers + `],"Synthesis":{"Agent":"` + synthesisAgent + `"}}`
				members, synth, err := db.EnqueuePanelRun(opts, EnqueueOpts{RepoID: repo.ID, GitRef: "topic", Agent: synthesisAgent, PanelRunUUID: &run, Experiment: reportAssignment("default", subject, plan)})
				require.NoError(t, err)
				return members, synth
			}
			const clean = `{"schema_version":2,"summary":"clean","verdict":"pass","findings":[]}`
			const finding = `{"schema_version":2,"summary":"issue","verdict":"fail","findings":[{"severity":"high","problem":"p","fix":"f","location":null}]}`
			if mode == "retained backup" || mode == "passthrough" || mode == "alias" {
				planned, executor := "codex", "claude-code"
				if mode == "passthrough" {
					executor = "test"
				}
				if mode == "alias" {
					planned = "claude"
				}
				members, synth := enqueue(mode, planned, false)
				member, err := db.ClaimJob("member-worker")
				require.NoError(t, err)
				require.NotNil(t, member)
				require.Equal(t, members[0].ID, member.ID)
				require.NoError(t, db.MarkJobAgentInvoked(member.ID, "member-worker", "test"))
				require.NoError(t, db.SaveJobSessionID(member.ID, "member-worker", "voter-session"))
				memberOutput := finding
				if mode == "passthrough" {
					memberOutput = clean
				}
				require.NoError(t, db.CompleteJob(member.ID, "test", memberOutput))
				require.NoError(t, db.SaveJobTokenUsage(member.ID, "voter-session", `{"has_cost":true,"cost_usd":1}`))
				require.NoError(t, db.MaybeReleasePanelSynthesis(*synth.PanelRunUUID))
				claimed, err := db.ClaimJob("synthesis-worker")
				require.NoError(t, err)
				require.NotNil(t, claimed)
				require.Equal(t, synth.ID, claimed.ID)
				// Retained results use the old invocation/completion writer sequence.
				if mode != "passthrough" {
					require.NoError(t, db.MarkJobAgentInvoked(synth.ID, "synthesis-worker", executor))
				}
				require.NoError(t, db.CompleteJob(synth.ID, executor, clean))
				stored, err := db.GetJobByID(synth.ID)
				require.NoError(t, err)
				require.Equal(t, planned, stored.Agent)
				review, err := db.GetReviewByJobID(synth.ID)
				require.NoError(t, err)
				require.Equal(t, executor, review.Agent)
				r, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{})
				require.NoError(t, err)
				offPlan := reportMetric(t, r, "off_plan_rate")
				require.NotNil(t, offPlan.Default.Value)
				want := 0.0
				if mode == "retained backup" {
					want = .5
				}
				assert.InDelta(t, want, *offPlan.Default.Value, 1e-12)
				assert.True(t, offPlan.Default.Complete)
				require.Len(t, r.Default.CostliestUnits, 1)
				require.Len(t, r.Default.CostliestUnits[0].Components, 2)
				component := r.Default.CostliestUnits[0].Components[1]
				wantAgent := executor
				if mode == "passthrough" {
					wantAgent = planned
				}
				assert.Equal(t, wantAgent, component.Agent)
				return
			}
			members, synth := enqueue(mode, "test", true)
			voterStatus, voterOutput := "done", finding
			if mode == "execution rates" || mode == "pending execution" {
				voterStatus, voterOutput = "failed", ""
			}
			reportFinish(t, db, members[0].ID, voterStatus, new(1.0), voterOutput)
			if mode != "missing synthesis" {
				reportFinish(t, db, synth.ID, "done", new(3.0), clean)
			}
			switch mode {
			case "complete", "missing synthesis":
				reportFinish(t, db, members[1].ID, "done", new(2.0), "")
			case "execution rates", "pending execution":
				status := "canceled"
				if mode == "pending execution" {
					status = "running"
				}
				reportFinish(t, db, members[1].ID, status, new(2.0), "")
				_, err := db.Exec(`UPDATE review_jobs SET agent='fallback', started_at='2025-12-31T23:00:00Z', finished_at=CASE WHEN status='running' THEN NULL ELSE finished_at END, token_usage='{"has_cost":true,"cost_usd":2,"provider_session_ids":["advisory-session"]}' WHERE id=?`, members[1].ID)
				require.NoError(t, err)
			case "missing advisory":
				_, err := db.Exec(`DELETE FROM review_jobs WHERE id=?`, members[1].ID)
				require.NoError(t, err)
			}
			if mode == "missing synthesis" {
				_, err := db.Exec(`DELETE FROM review_jobs WHERE id=?`, synth.ID)
				require.NoError(t, err)
				control, canonical := enqueue("complete-control", "test", true)
				reportFinish(t, db, control[0].ID, "done", new(1.0), finding)
				reportFinish(t, db, control[1].ID, "done", new(2.0), "")
				reportFinish(t, db, canonical.ID, "done", new(3.0), clean)
			}
			r, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{})
			require.NoError(t, err)
			assert := assert.New(t)
			if mode == "missing synthesis" {
				assert.Equal(2, r.Default.ReviewUnits)
				assert.Equal(1, r.Default.CanonicalStatuses.Missing)
				for _, name := range []string{"duration_p50_seconds", "duration_p90_seconds", "fail_verdict_rate", "findings_critical_per_review", "findings_high_per_review", "findings_medium_per_review", "findings_low_per_review"} {
					metric := reportMetric(t, r, name)
					assert.Equal(1, metric.Default.Units)
					assert.Equal(2, metric.Default.ExpectedUnits)
					require.NotNil(t, metric.Default.Coverage)
					assert.InDelta(.5, *metric.Default.Coverage, 1e-12)
					assert.False(metric.Default.Complete)
					assert.Equal("not enough data", metric.State)
				}
				return
			}
			assert.Equal(1, r.Default.ReviewUnits)
			assert.Zero(*reportMetric(t, r, "findings_high_per_review").Default.Value)
			assert.Zero(*reportMetric(t, r, "fail_verdict_rate").Default.Value)
			assert.InDelta(10.0, *reportMetric(t, r, "duration_p50_seconds").Default.Value, 1e-12)
			cost := reportMetric(t, r, "cost_per_review")
			if mode == "complete" || mode == "execution rates" {
				require.NotNil(t, cost.Default.Value)
				assert.InDelta(6.0, *cost.Default.Value, 1e-12)
				assert.Equal(3, r.Default.Cost.PricedComponents)
				assert.Equal(1, r.Default.TerminalUnits)
			} else {
				assert.Nil(cost.Default.Value)
				assert.Zero(r.Default.Cost.CompleteUnits)
				assert.Equal(1, r.Default.PendingUnits)
			}
			switch mode {
			case "execution rates":
				assert.InDelta(2.0/3, *reportMetric(t, r, "job_failure_rate").Default.Value, 1e-12)
				assert.InDelta(1.0/3, *reportMetric(t, r, "off_plan_rate").Default.Value, 1e-12)
				assert.Equal(1, r.Default.FailedUnits)
				assert.Equal(1, r.Default.CanceledUnits)
				encoded, err := json.Marshal(r.Default.CostliestUnits)
				require.NoError(t, err)
				assert.Contains(string(encoded), `"provider_session_ids":["advisory-session"]`)
			case "pending advisory", "pending execution":
				assert.Nil(reportMetric(t, r, "off_plan_rate").Default.Value)
				assert.Nil(reportMetric(t, r, "job_failure_rate").Default.Value)
			}
		})
	}
}

func TestExperimentReportMissingData(t *testing.T) {
	t.Parallel()
	db, repo := setupDBAndRepo(t, "report-missing")
	reportJob(t, db, repo.ID, "default", "free", new(0.0))
	unknown := reportJob(t, db, repo.ID, "default", "unknown", nil)
	_, err := db.Exec(`UPDATE experiment_assignments SET effective_config_json='{}' WHERE review_unit_uuid=?`, unknown.UUID)
	require.NoError(t, err)
	for _, status := range []string{"failed", "canceled", "skipped", "queued"} {
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: status, Agent: "test", Experiment: reportAssignment("default", status, `{"Agent":"test"}`)})
		require.NoError(t, err)
		if status != "queued" {
			reportFinish(t, db, job.ID, status, nil, "")
		}
	}
	r, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal(6, r.Default.ReviewUnits)
	assert.Equal(1, r.Default.PendingUnits)
	assert.Equal(1, r.Default.FailedUnits)
	assert.Equal(1, r.Default.CanceledUnits)
	assert.Equal(1, r.Default.SkippedUnits)
	duration := reportMetric(t, r, "duration_p50_seconds")
	require.NotNil(t, duration.Default.Value)
	assert.InDelta(10.0, *duration.Default.Value, 1e-12)
	assert.Equal(5, duration.Default.Units)
	cost := reportMetric(t, r, "cost_per_review")
	require.NotNil(t, cost.Default.Value)
	assert.InDelta(0.0, *cost.Default.Value, 1e-12)
	assert.Equal(1, cost.Default.Units)
	assert.Equal("not enough data", cost.State)
	assert.Equal(2, reportMetric(t, r, "findings_high_per_review").Default.Units)
	assert.Equal(2, reportMetric(t, r, "fail_verdict_rate").Default.Units)
	assert.Nil(reportMetric(t, r, "refuted_per_100").Default.Value)
	assert.Contains(r.Labels.Reason, "durable")
	assert.False(r.Integrity.Complete)
	b, err := json.Marshal(r)
	require.NoError(t, err)
	assert.NotContains(string(b), "effective_config")
	assert.NotContains(string(b), "subject_hash")
}

func TestExperimentReportCohortBounds(t *testing.T) {
	t.Parallel()
	db, repo := setupDBAndRepo(t, "report-bounds")
	a := reportJob(t, db, repo.ID, "default", "a", new(1.0))
	b := reportJob(t, db, repo.ID, "experiment", "b", new(2.0))
	_, err := db.Exec(`UPDATE experiment_assignments SET assigned_at='2026-01-02T00:00:00+02:00' WHERE review_unit_uuid=?`, a.UUID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE experiment_assignments SET assigned_at='2026-01-02T00:00:00Z' WHERE review_unit_uuid=?`, b.UUID)
	require.NoError(t, err)
	since := time.Date(2026, 1, 1, 22, 0, 0, 0, time.UTC)
	until := since.Add(2 * time.Hour)
	for _, tc := range []struct {
		name, id                        string
		opts                            ExperimentReportOptions
		defaultUnits, experimentalUnits int
		since, until                    *time.Time
		wantError                       error
	}{
		{name: "omitted bounds", defaultUnits: 1, experimentalUnits: 1},
		{name: "inclusive since exclusive until", opts: ExperimentReportOptions{Since: since, Until: until}, defaultUnits: 1, since: new(since), until: new(until)},
		{name: "after enrollment", opts: ExperimentReportOptions{Since: until.Add(time.Hour)}, since: new(until.Add(time.Hour))},
		{name: "unknown experiment", id: "absent", wantError: sql.ErrNoRows},
		{name: "negative minimum", opts: ExperimentReportOptions{MinSubjects: -1}, wantError: ErrInvalidExperimentReportOptions},
		{name: "reversed bounds", opts: ExperimentReportOptions{Since: until, Until: since}, wantError: ErrInvalidExperimentReportOptions},
		{name: "explicit zero until", opts: ExperimentReportOptions{UntilProvided: true}, until: new(time.Time{})},
		{name: "explicit zero since", opts: ExperimentReportOptions{SinceProvided: true}, defaultUnits: 1, experimentalUnits: 1, since: new(time.Time{})},
		{name: "reversed zero until", opts: ExperimentReportOptions{Since: until, UntilProvided: true}, wantError: ErrInvalidExperimentReportOptions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.id
			if id == "" {
				id = "report-v1"
			}
			r, err := db.GetExperimentReport(id, tc.opts)
			if tc.wantError != nil {
				require.ErrorIs(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			assert := assert.New(t)
			assert.Equal(tc.defaultUnits, r.Default.ReviewUnits)
			assert.Equal(tc.defaultUnits, r.Default.Subjects)
			assert.Equal(tc.experimentalUnits, r.Experiment.ReviewUnits)
			assert.Equal(tc.since, r.Since)
			assert.Equal(tc.until, r.Until)
		})
	}
}

func TestExperimentReportPendingAndUnreadableOutputs(t *testing.T) {
	t.Parallel()
	db, repo := setupDBAndRepo(t, "output-coverage")
	reportJob(t, db, repo.ID, "default", "a", new(1.0))
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "pending", Agent: "test", Experiment: reportAssignment("default", "pending", `{"Agent":"test"}`)})
	require.NoError(t, err)
	r, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	m := reportMetric(t, r, "findings_high_per_review")
	assert.Equal(t, 2, m.Default.ExpectedUnits)
	assert.Equal(t, 1, m.Default.Units)
	assert.False(t, m.Default.Complete)
	offPlan := reportMetric(t, r, "off_plan_rate")
	assert.Equal(t, 2, offPlan.Default.ExpectedUnits)
	assert.False(t, offPlan.Default.Complete)
	reportFinish(t, db, job.ID, "done", new(1.0), `{"schema_version":2,"summary":"clean","verdict":"pass","findings":[]}`)
	_, err = db.Exec(`UPDATE review_jobs SET error='unreadable result' WHERE id=?`, job.ID)
	require.NoError(t, err)
	r, err = db.GetExperimentReport("report-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	m = reportMetric(t, r, "findings_high_per_review")
	assert.Equal(t, 2, m.Default.ExpectedUnits)
	assert.Equal(t, 1, m.Default.Units)
	assert.False(t, m.Default.Complete)
}

func TestExperimentReportReliabilityGate(t *testing.T) {
	t.Parallel()
	for _, failures := range []int{1, 4} {
		t.Run(string(rune('0'+failures)), func(t *testing.T) {
			db, repo := setupDBAndRepo(t, "reliability")
			for i := range 4 {
				reportJob(t, db, repo.ID, "default", string(rune('a'+i)), new(1.0))
				subject := string(rune('k' + i))
				if i < failures {
					job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: subject, Agent: "test", Experiment: reportAssignment("experiment", subject, `{"Agent":"test"}`)})
					require.NoError(t, err)
					reportFinish(t, db, job.ID, "failed", new(2.0), "")
				} else {
					reportJob(t, db, repo.ID, "experiment", subject, new(2.0))
				}
			}
			r, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{})
			require.NoError(t, err)
			failure := reportMetric(t, r, "job_failure_rate")
			cost := reportMetric(t, r, "cost_per_review")
			if failures == 4 {
				assert.Equal(t, "worse", failure.State)
				assert.Equal(t, "reliability worse", cost.State)
			} else {
				assert.Equal(t, "no detectable difference", failure.State)
				assert.Equal(t, "worse", cost.State)
			}
		})
	}
}

func TestExperimentReportConflictingSubjects(t *testing.T) {
	t.Parallel()
	db, repo := setupDBAndRepo(t, "conflict")
	reportJob(t, db, repo.ID, "default", "same", new(1.0))
	reportJob(t, db, repo.ID, "experiment", "same", new(2.0))
	r, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{MinSubjects: 1})
	require.NoError(t, err)
	assert.False(t, r.Integrity.Complete)
	assert.Equal(t, 1, r.Integrity.ConflictingSubjects)
	cost := reportMetric(t, r, "cost_per_review")
	assert.Equal(t, "not enough data", cost.State)
	assert.Nil(t, cost.PValue)
}

func TestExperimentReportTerminalOutputCoverage(t *testing.T) {
	t.Parallel()
	for _, version := range []struct {
		name, pass, fail string
	}{
		{name: "v1", pass: `{"schema_version":1,"summary":"clean","findings":[]}`, fail: `{"schema_version":1,"summary":"issue","findings":[{"severity":"high","problem":"p","fix":"f","location":null}]}`},
		{name: "v2", pass: `{"schema_version":2,"summary":"clean","verdict":"pass","findings":[]}`, fail: `{"schema_version":2,"summary":"issue","verdict":"fail","findings":[{"severity":"high","problem":"p","fix":"f","location":null}]}`},
	} {
		t.Run(version.name, func(t *testing.T) {
			db, repo := setupDBAndRepo(t, "terminal-output-coverage")
			for _, arm := range []string{"default", "experiment"} {
				for i := range 4 {
					subject := arm + string(rune('a'+i))
					job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: subject, Agent: "test", Experiment: reportAssignment(arm, subject, `{"Agent":"test"}`)})
					require.NoError(t, err)
					claimed, err := db.ClaimJob("report-worker")
					require.NoError(t, err)
					require.NotNil(t, claimed)
					require.Equal(t, job.ID, claimed.ID)
					output, wantVerdict := version.pass, VerdictPass
					if arm == "experiment" {
						output, wantVerdict = version.fail, VerdictFail
					}
					require.NoError(t, db.CompleteJob(job.ID, "test", output))
					stored, err := db.GetReviewByJobID(job.ID)
					require.NoError(t, err)
					require.NotNil(t, stored.VerdictBool)
					require.Equal(t, wantVerdict, stored.Verdict())
				}
			}
			known, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{})
			require.NoError(t, err)
			verdicts := reportMetric(t, known, "fail_verdict_rate")
			require.NotNil(t, verdicts.Default.Value)
			require.NotNil(t, verdicts.Experiment.Value)
			assert.Zero(t, *verdicts.Default.Value)
			assert.InDelta(t, 1.0, *verdicts.Experiment.Value, 1e-12)
			assert.True(t, verdicts.Default.Complete)
			assert.True(t, verdicts.Experiment.Complete)
			assert.Equal(t, "higher", verdicts.State)
			for _, arm := range []string{"default", "experiment"} {
				for _, status := range []string{"failed", "canceled", "skipped"} {
					job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: status, Agent: "test", Experiment: reportAssignment(arm, arm+status, `{"Agent":"test"}`)})
					require.NoError(t, err)
					reportFinish(t, db, job.ID, status, new(1.0), "")
				}
			}
			r, err := db.GetExperimentReport("report-v1", ExperimentReportOptions{})
			require.NoError(t, err)
			assert.Equal(t, "no detectable difference", reportMetric(t, r, "job_failure_rate").State)
			for _, name := range []string{"fail_verdict_rate", "findings_critical_per_review", "findings_high_per_review", "findings_medium_per_review", "findings_low_per_review"} {
				t.Run(name, func(t *testing.T) {
					metric := reportMetric(t, r, name)
					assert := assert.New(t)
					for _, arm := range []ExperimentMetricValue{metric.Default, metric.Experiment} {
						assert.Equal(4, arm.Subjects)
						assert.Equal(4, arm.Units)
						assert.Equal(7, arm.ExpectedUnits)
						require.NotNil(t, arm.Coverage)
						assert.InDelta(4.0/7, *arm.Coverage, 1e-12)
						assert.False(arm.Complete)
					}
					assert.Equal("not enough data", metric.State)
					assert.Nil(metric.Delta)
					assert.Nil(metric.Interval)
					assert.Nil(metric.PValue)
					assert.Nil(metric.RandomizationThreshold)
				})
			}
		})
	}
}
