package daemon

import (
	"encoding/json/v2"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// These requests catch missing route registration, cohort translation, validation,
// and accidental disclosure of the frozen execution configuration.
func TestExperimentReportRoute(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "topic", Agent: "test",
		Experiment: &storage.ExperimentAssignmentInput{
			ExperimentID: "example-v1", DefinitionHash: "definition",
			DefinitionJSON: `{"ratio":0.5,"private_marker":"definition-secret"}`,
			Arm:            "default", SubjectHash: "subject-a", EffectiveConfigHash: "frozen",
			EffectiveConfigJSON: `{"Agent":"test","private_marker":"plan-secret"}`,
		},
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE experiment_assignments SET assigned_at='2026-01-02T00:00:00Z' WHERE review_unit_uuid=?`, job.UUID)
	require.NoError(t, err)

	for _, tc := range []struct {
		name, path string
		status     int
		subjects   int
		minimum    int
		zeroBound  string
	}{
		{"all time", "/api/experiments/example-v1/report", 200, 1, 4, ""},
		{"inclusive since", "/api/experiments/example-v1/report?since=2026-01-02T00:00:00Z&min_subjects=1", 200, 1, 1, ""},
		{"exclusive until", "/api/experiments/example-v1/report?until=2026-01-02T00:00:00Z", 200, 0, 4, ""},
		{"offset bound", "/api/experiments/example-v1/report?since=" + url.QueryEscape("2026-01-02T02:00:00+02:00"), 200, 1, 4, ""},
		{"unknown", "/api/experiments/missing/report", 404, 0, 0, ""},
		{"malformed since", "/api/experiments/example-v1/report?since=yesterday", 400, 0, 0, ""},
		{"malformed until", "/api/experiments/example-v1/report?until=2026-01-02", 400, 0, 0, ""},
		{"empty since", "/api/experiments/example-v1/report?since=", 400, 0, 0, ""},
		{"empty until", "/api/experiments/example-v1/report?until=", 400, 0, 0, ""},
		{"reversed", "/api/experiments/example-v1/report?since=2026-01-03T00:00:00Z&until=2026-01-02T00:00:00Z", 400, 0, 0, ""},
		{"zero minimum", "/api/experiments/example-v1/report?min_subjects=0", 400, 0, 0, ""},
		{"negative minimum", "/api/experiments/example-v1/report?min_subjects=-1", 400, 0, 0, ""},
		{"malformed minimum", "/api/experiments/example-v1/report?min_subjects=no", 400, 0, 0, ""},
		{"empty minimum", "/api/experiments/example-v1/report?min_subjects=", 400, 0, 0, ""},
		{"zero until", "/api/experiments/example-v1/report?until=0001-01-01T00:00:00Z", 200, 0, 4, "until"},
		{"zero since", "/api/experiments/example-v1/report?since=0001-01-01T00:00:00Z", 200, 1, 4, "since"},
		{"offset zero until", "/api/experiments/example-v1/report?until=" + url.QueryEscape("0001-01-01T01:00:00+01:00"), 200, 0, 4, "until"},
		{"offset zero since", "/api/experiments/example-v1/report?since=" + url.QueryEscape("0001-01-01T01:00:00+01:00"), 200, 1, 4, "since"},
		{"reversed zero until", "/api/experiments/example-v1/report?since=2026-01-02T00:00:00Z&until=0001-01-01T00:00:00Z", 400, 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := serveHuma(t, srv, http.MethodGet, tc.path, nil)
			require.Equal(t, tc.status, rr.Code, rr.Body.String())
			if tc.status != http.StatusOK {
				return
			}
			var report storage.ExperimentReport
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &report))
			assert := assert.New(t)
			assert.Equal("example-v1", report.ID)
			assert.Equal(tc.subjects, report.Default.Subjects)
			assert.Equal(tc.subjects, report.Default.PendingUnits)
			assert.Equal(tc.minimum, report.MinSubjects)
			assert.False(report.AsOf.IsZero())
			if tc.zeroBound != "" {
				bound := report.Until
				if tc.zeroBound == "since" {
					bound = report.Since
				}
				require.NotNil(t, bound)
				assert.Equal(time.Time{}, *bound)
			}
			if report.Since != nil {
				assert.Equal(time.UTC, report.Since.Location())
			}
			assert.NotContains(rr.Body.String(), "definition-secret")
			assert.NotContains(rr.Body.String(), "plan-secret")
			assert.NotContains(rr.Body.String(), "effective_config")
			assert.NotContains(rr.Body.String(), "subject_hash")
			var wire map[string]any
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &wire))
			labels, ok := wire["labels"].(map[string]any)
			require.True(t, ok)
			refutedReviews, ok := labels["refuted_reviews"].([]any)
			require.True(t, ok, "unavailable refuted reviews must serialize as an array")
			assert.Empty(refutedReviews)
		})
	}
}

func TestExperimentReportRouteStorageError(t *testing.T) {
	srv, db, _ := newTestServer(t)
	require.NoError(t, db.Close())
	rr := serveHuma(t, srv, http.MethodGet, "/api/experiments/example-v1/report", nil)
	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	assert.NotContains(t, rr.Body.String(), "database is closed")
}

// Generated clients must accept the null interval emitted when inference is unavailable.
func TestExperimentReportRouteSchema(t *testing.T) {
	api := (&Server{}).registerHumaAPI(http.NewServeMux())
	schemas := api.OpenAPI().Components.Schemas.Map()
	comparison := schemas["ExperimentMetricComparison"]
	require.NotNil(t, comparison)
	interval := comparison.Properties["interval"]
	require.NotNil(t, interval)
	assert.True(t, interval.Nullable)
	for _, value := range []any{nil, map[string]any{"low": -1.0, "high": 1.0}} {
		result := &huma.ValidateResult{}
		huma.Validate(api.OpenAPI().Components.Schemas, interval, &huma.PathBuffer{}, huma.ModeReadFromServer, value, result)
		assert.Empty(t, result.Errors)
	}
	report := schemas["ExperimentReport"]
	require.NotNil(t, report)
	assert.True(t, report.Properties["since"].Nullable)
	assert.True(t, report.Properties["until"].Nullable)
}
