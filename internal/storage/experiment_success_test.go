package storage

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func successBar() *config.ExperimentSuccess {
	return &config.ExperimentSuccess{Metric: "cost_per_review", Direction: "lower", MinEffect: "20%", Guards: []string{"job_failure_rate <= +0"}}
}

func successAssignment(t *testing.T, bar *config.ExperimentSuccess, arm, subject string) *ExperimentAssignmentInput {
	t.Helper()
	selection, err := config.SelectReviewExperiment(config.ExperimentSelectionInput{
		Workflow: config.ExperimentWorkflowReview, Subject: config.ExperimentSubject{Repository: "example/project", Branch: subject},
		Global: &config.Config{Experiments: map[string]config.ExperimentDefinition{"success-v1": {Enabled: new(true), Ratio: new(0.5), Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowReview}, Config: map[string]any{"reuse_review_session": true}, Success: bar}}},
	})
	require.NoError(t, err)
	require.NotNil(t, selection.Assignment)
	return &ExperimentAssignmentInput{ExperimentID: "success-v1", DefinitionHash: selection.Assignment.DefinitionHash, DefinitionJSON: selection.Assignment.DefinitionJSON, Arm: arm, SubjectHash: subject, EffectiveConfigHash: "frozen", EffectiveConfigJSON: `{"Agent":"test"}`}
}

func successReportFixture(t *testing.T, bar *config.ExperimentSuccess, n int, defaultCost, experimentalCost float64) (*DB, int64) {
	t.Helper()
	db, repo := setupDBAndRepo(t, "success-report")
	for _, arm := range []string{"default", "experiment"} {
		for i := range n {
			subject := fmt.Sprintf("%s-%d", arm, i)
			job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: subject, Agent: "test", Experiment: successAssignment(t, bar, arm, subject)})
			require.NoError(t, err)
			cost := defaultCost
			if arm == "experiment" {
				cost = experimentalCost
			}
			reportFinish(t, db, job.ID, "done", &cost, `{"schema_version":2,"summary":"clean","verdict":"pass","findings":[]}`)
		}
	}
	return db, repo.ID
}

func TestExperimentReportSuccess(t *testing.T) {
	t.Parallel()
	db, _ := successReportFixture(t, successBar(), 4, 10, 5)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	encoded, err := json.Marshal(r)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	require.Contains(t, wire, "success")
	assert.Equal(t, "bar met", wire["success"].(map[string]any)["state"])
}

func TestExperimentReportSuccessMinimumNotEstablished(t *testing.T) {
	t.Parallel()
	bar := successBar()
	bar.MinEffect = "60%"
	db, _ := successReportFixture(t, bar, 4, 10, 5)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	require.NotNil(t, r.Success)
	assert.Equal(t, "bar not met", r.Success.State)
	assert.Contains(t, r.Success.Reason, "minimum improvement")
}

func TestExperimentReportSuccessZeroMinimumRequiresImprovement(t *testing.T) {
	t.Parallel()
	bar := successBar()
	bar.MinEffect = "0"
	db, _ := successReportFixture(t, bar, 4, 10, 10)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert.Equal(t, "bar not met", r.Success.State)
	assert.Contains(t, r.Success.Reason, "no statistically detectable improvement")
}

func TestExperimentReportSuccessTinyExactPermutation(t *testing.T) {
	t.Parallel()
	bar := successBar()
	bar.MinEffect = "0"
	db, _ := successReportFixture(t, bar, 3, 10, 5)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{MinSubjects: 1})
	require.NoError(t, err)
	m := reportMetric(t, r, "cost_per_review")
	assert.InDelta(t, 0.1, *m.PValue, 1e-12)
	assert.Less(t, m.Interval.High, 0.0)
	assert.Equal(t, "bar not met", r.Success.State)
}

func TestExperimentReportSuccessDeclaredDirection(t *testing.T) {
	t.Parallel()
	bar := successBar()
	bar.Direction = "higher"
	bar.MinEffect = "0"
	db, _ := successReportFixture(t, bar, 4, 5, 10)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert.Equal(t, "worse", reportMetric(t, r, "cost_per_review").State)
	assert.Equal(t, "bar met", r.Success.State)
}

func TestExperimentReportSuccessInsufficientSubjects(t *testing.T) {
	t.Parallel()
	db, _ := successReportFixture(t, successBar(), 3, 10, 5)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert.Equal(t, "not enough data", r.Success.State)
}

func TestExperimentReportSuccessIncompleteCost(t *testing.T) {
	t.Parallel()
	db, _ := successReportFixture(t, successBar(), 4, 10, 5)
	_, err := db.Exec(`UPDATE review_jobs SET token_usage='' WHERE git_ref='experiment-0'`)
	require.NoError(t, err)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert.Equal(t, "not enough data", r.Success.State)
	assert.Contains(t, r.Success.Reason, "cost_per_review")
}

func TestExperimentReportSuccessUnavailableLabelGuard(t *testing.T) {
	t.Parallel()
	bar := successBar()
	bar.Guards = []string{"refuted_per_100 <= +0"}
	db, _ := successReportFixture(t, bar, 4, 10, 5)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert.Equal(t, "not enough data", r.Success.State)
	require.Len(t, r.Success.Guards, 1)
	assert.Equal(t, "not enough data", r.Success.Guards[0].State)
	assert.Contains(t, r.Success.Guards[0].Reason, "durable finding identity")
}

func TestExperimentReportSuccessUnavailableLabelPrimary(t *testing.T) {
	t.Parallel()
	bar := &config.ExperimentSuccess{Metric: "confirmed_per_review", Direction: "higher", MinEffect: "0"}
	db, _ := successReportFixture(t, bar, 4, 10, 5)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert.Equal(t, "not enough data", r.Success.State)
	assert.Contains(t, r.Success.Reason, "durable finding identity")
}

func TestExperimentReportSuccessZeroRelativeBaseline(t *testing.T) {
	t.Parallel()
	bar := successBar()
	bar.Direction = "higher"
	db, _ := successReportFixture(t, bar, 4, 0, 5)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert.Equal(t, "not enough data", r.Success.State)
	assert.Contains(t, r.Success.Reason, "zero or undefined")
}

func TestExperimentReportSuccessBothZeroRelativeGuard(t *testing.T) {
	t.Parallel()
	bar := successBar()
	bar.Guards = []string{"job_failure_rate <= +1%"}
	db, _ := successReportFixture(t, bar, 4, 10, 5)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert.Equal(t, "not enough data", r.Success.State)
	assert.Contains(t, r.Success.Guards[0].Reason, "zero or undefined")
}

func TestExperimentReportSuccessReliabilityRegression(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		bar         *config.ExperimentSuccess
		failDefault bool
	}{
		{name: "cost primary", bar: successBar()},
		{name: "reliability primary", bar: &config.ExperimentSuccess{Metric: "job_failure_rate", Direction: "lower", MinEffect: "0"}, failDefault: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := successReportFixture(t, tc.bar, 4, 10, 5)
			if tc.failDefault {
				_, err := db.Exec(`UPDATE review_jobs SET status='failed' WHERE git_ref LIKE 'default-%'`)
				require.NoError(t, err)
			}
			_, err := db.Exec(`UPDATE reviews SET agent='other' WHERE job_id IN (SELECT id FROM review_jobs WHERE git_ref LIKE 'experiment-%')`)
			require.NoError(t, err)
			r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
			require.NoError(t, err)
			if tc.failDefault {
				assert.Equal(t, "better", reportMetric(t, r, "job_failure_rate").State)
			}
			assert.Equal(t, "bar not met", r.Success.State)
			assert.Contains(t, r.Success.Reason, "job failure or off-plan")
		})
	}
}

func TestExperimentReportSuccessStoredDefinitionImmutable(t *testing.T) {
	t.Parallel()
	bar := successBar()
	db, repoID := successReportFixture(t, bar, 4, 10, 5)
	bar.MinEffect = "90%" // Editing today's config cannot rewrite the enrolled bar.
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert.Equal(t, "20%", r.Success.MinEffect)
	assert.Equal(t, "bar met", r.Success.State)
	_, err = db.EnqueueJob(EnqueueOpts{RepoID: repoID, GitRef: "new", Agent: "test", Experiment: successAssignment(t, bar, "experiment", "new")})
	require.ErrorContains(t, err, "experiment definition conflict")
}

func TestExperimentReportSuccessGuardConservativeEndpoints(t *testing.T) {
	t.Parallel()
	db, _ := successReportFixture(t, successBar(), 4, 10, 5)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	for i := range r.Metrics {
		if r.Metrics[i].Metric == "cost_per_review" {
			r.Metrics[i].Interval = &ExperimentInterval{Low: -6, High: -4}
		}
	}
	// The point estimate -5 satisfies these bounds. The CI does not.
	r.definitionJSON = `{"success":{"metric":"cost_per_review","direction":"lower","min_effect":"0","guards":["cost_per_review <= -45%","cost_per_review >= -55%"]}}`
	result := evaluateExperimentSuccess(r)
	assert.Equal(t, "bar not met", result.State)
	require.Len(t, result.Guards, 2)
	assert.Equal(t, "bar not met", result.Guards[0].State)
	assert.Equal(t, "bar not met", result.Guards[1].State)
}

func TestExperimentReportSuccessUnconfiguredOmitted(t *testing.T) {
	t.Parallel()
	db, _ := successReportFixture(t, nil, 4, 10, 5)
	r, err := db.GetExperimentReport("success-v1", ExperimentReportOptions{})
	require.NoError(t, err)
	assert.Nil(t, r.Success)
	encoded, err := json.Marshal(r)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"success"`)
}
