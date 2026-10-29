package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func experimentReportFixture() storage.ExperimentReport {
	return storage.ExperimentReport{
		ID: "example-v1", Ratio: new(.5), AsOf: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), MinSubjects: 4,
		// Four standalone units and a panel with completed synthesis and a
		// pending advisory member account for six invoked components.
		Default: storage.ExperimentArmReport{
			Subjects: 4, ReviewUnits: 5, TerminalUnits: 4, PendingUnits: 1,
			AttributionCompleteUnits: 5, OutputUnits: 5, CanonicalStatuses: storage.ExperimentCanonicalStatuses{Done: 5},
			Cost: storage.ExperimentCostCoverage{EligibleComponents: 6, PricedComponents: 0, Reason: "Run backfill-tokens to fill missing cost."},
		},
		Experiment: storage.ExperimentArmReport{
			Subjects: 4, ReviewUnits: 4, TerminalUnits: 4, AttributionCompleteUnits: 4, OutputUnits: 4,
			CanonicalStatuses: storage.ExperimentCanonicalStatuses{Done: 4},
			Cost:              storage.ExperimentCostCoverage{EligibleComponents: 4, Reason: "Run backfill-tokens to fill missing cost."},
		},
		Integrity: storage.ExperimentIntegrity{Complete: true},
		Allocation: storage.ExperimentAllocation{
			DefaultSubjects: 4, ExperimentalSubjects: 4, ExpectedDefault: new(4.0), ExpectedExperimental: new(4.0),
			ChiSquare: new(0.0), PValue: new(1.0), Approximate: true,
		},
		Labels: storage.ExperimentLabelCoverage{Reason: "Labels lack durable finding identity."},
		Method: storage.ExperimentReportMethod{Resamples: 4096, Confidence: .95, Exploratory: true, OutcomeBasis: "latest retained attempts"},
		Metrics: []storage.ExperimentMetricComparison{
			{
				Metric: "cost_per_review", State: "not enough data", Reason: "Incomplete cost coverage",
				Default:    storage.ExperimentMetricValue{ExpectedUnits: 5, Coverage: new(0.0)},
				Experiment: storage.ExperimentMetricValue{ExpectedUnits: 4, Coverage: new(0.0)},
			},
			{
				Metric: "duration_p50_seconds", Default: storage.ExperimentMetricValue{Value: new(10.0), Subjects: 4, Units: 5, ExpectedUnits: 5, Coverage: new(1.0), Complete: true},
				Experiment: storage.ExperimentMetricValue{Value: new(5.0), Subjects: 4, Units: 4, ExpectedUnits: 4, Coverage: new(1.0), Complete: true},
				Delta:      new(-5.0), Interval: &storage.ExperimentInterval{Low: -6, High: -4}, PValue: new(2.0 / 70), RandomizationThreshold: new(3.0), Permutations: 70, State: "better",
			},
			{Metric: "refuted_per_100", State: "not enough data", Reason: "Labels lack durable finding identity."},
		},
	}
}

// The controlled daemon catches lost query options, bypassed endpoint auth,
// JSON polluted by prose, and swallowed HTTP/decoding errors.
func TestExperimentsReport(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		args                  []string
		status                int
		body                  string
		wantError             string
		since, until, minimum string
	}{
		{name: "human", status: 200},
		{name: "zero until", args: []string{"--until", "0001-01-01T00:00:00Z"}, status: 200, until: "0001-01-01T00:00:00Z"},
		{name: "zero since", args: []string{"--since", "0001-01-01T00:00:00Z"}, status: 200, since: "0001-01-01T00:00:00Z"},
		{name: "offset zero until", args: []string{"--until", "0001-01-01T01:00:00+01:00"}, status: 200, until: "0001-01-01T01:00:00+01:00"},
		{name: "json bounds", args: []string{"--json", "--since", "2026-01-01T02:00:00+02:00", "--until", "2026-01-03T00:00:00Z", "--min-subjects", "2"}, status: 200, since: "2026-01-01T02:00:00+02:00", until: "2026-01-03T00:00:00Z", minimum: "2"},
		{name: "unknown", status: 404, body: `{"detail":"unknown experiment"}`, wantError: "404 Not Found"},
		{name: "invalid daemon request", status: 400, body: `{"detail":"invalid cohort"}`, wantError: "400 Bad Request"},
		{name: "invalid response", status: 200, body: `{`, wantError: "parse response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "`+key+`"`), 0o600))
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
				if r.URL.Path == "/api/ping" {
					writeJSON(w, map[string]any{"ok": true, "service": "roborev"})
					return
				}
				requests++
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/experiments/example-v1/report", r.URL.Path)
				assert.Equal(t, tc.since, r.URL.Query().Get("since"))
				assert.Equal(t, tc.until, r.URL.Query().Get("until"))
				if tc.since == "" {
					assert.NotContains(t, r.URL.Query(), "since")
				}
				if tc.until == "" {
					assert.NotContains(t, r.URL.Query(), "until")
				}
				minimum := tc.minimum
				if minimum == "" {
					minimum = "4"
				}
				assert.Equal(t, minimum, r.URL.Query().Get("min_subjects"))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
					return
				}
				writeJSON(w, experimentReportFixture())
			}))
			t.Cleanup(srv.Close)
			patchServerAddr(t, srv.URL)
			cmd := experimentsCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs(append([]string{"report", "example-v1"}, tc.args...))
			err := cmd.Execute()
			assert.Equal(t, 1, requests)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			if tc.name == "json bounds" {
				var got storage.ExperimentReport
				require.NoError(t, json.Unmarshal(out.Bytes(), &got))
				expected, err := json.Marshal(experimentReportFixture())
				require.NoError(t, err)
				assert.JSONEq(t, string(expected), out.String())
			}
		})
	}
}

func TestExperimentsReportInvalidArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"one", "two"}} {
		cmd := experimentsReportCmd()
		require.EqualError(t, cmd.Args(cmd, args), fmt.Sprintf("accepts 1 arg(s), received %d", len(args)))
	}
	for _, tc := range []struct {
		name      string
		args      []string
		wantError string
	}{
		{name: "zero minimum", args: []string{"--min-subjects", "0"}, wantError: "min-subjects must be a positive integer"},
		{name: "negative minimum", args: []string{"--min-subjects", "-2"}, wantError: "min-subjects must be a positive integer"},
		{name: "invalid since", args: []string{"--since", "yesterday"}, wantError: "since must be an RFC3339 timestamp"},
		{name: "invalid until", args: []string{"--until", "2026-01-01"}, wantError: "until must be an RFC3339 timestamp"},
		{name: "reversed bounds", args: []string{"--since", "2026-01-02T00:00:00Z", "--until", "2026-01-01T00:00:00Z"}, wantError: "since must not be after until"},
		{name: "reversed zero until", args: []string{"--since", "2026-01-02T00:00:00Z", "--until", "0001-01-01T00:00:00Z"}, wantError: "since must not be after until"},
		{name: "reversed offset zero until", args: []string{"--since", "2026-01-02T00:00:00Z", "--until", "0001-01-01T01:00:00+01:00"}, wantError: "since must not be after until"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				writeJSON(w, map[string]any{"ok": true, "service": "roborev"})
			}))
			t.Cleanup(srv.Close)
			patchServerAddr(t, srv.URL)
			cmd := experimentsCmd()
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetArgs(append([]string{"report", "example-v1"}, tc.args...))
			require.EqualError(t, cmd.Execute(), tc.wantError)
			assert.Zero(t, requests, "invalid flags must fail before connecting to the daemon")
		})
	}
}

func TestFormatExperimentReport(t *testing.T) {
	report := experimentReportFixture()
	out := formatExperimentReport(report)
	assert := assert.New(t)
	assert.Contains(out, "example-v1")
	assert.Contains(out, "subjects=4")
	assert.Contains(out, "review units=5")
	assert.Contains(out, "pending=1")
	assert.Contains(out, "cost unknown")
	assert.Contains(out, "0/6 components priced")
	assert.Contains(out, "backfill-tokens")
	assert.Contains(out, "Labels lack durable finding identity.")
	assert.Contains(out, "cost_per_review")
	assert.Contains(out, "not enough data")
	assert.Contains(out, "duration_p50_seconds")
	assert.Contains(out, "subjects=4, units=5/5, coverage=100.0%")
	assert.Contains(out, "better")
	assert.Contains(out, "95% randomization threshold=3")
	assert.Contains(out, "exact, 70 permutations")
	assert.Contains(out, "bootstrap 95% interval=[-6, -4]")
	assert.Contains(out, "4096 resamples")
	assert.Contains(out, "Bootstrap intervals are approximate")
	assert.Contains(out, "latest retained attempts")
	assert.Contains(out, "exploratory, unadjusted")

	// An incomplete metric still displays coverage, with inference unavailable.
	metric := &report.Metrics[1]
	metric.Default.Units, metric.Default.Coverage, metric.Default.Complete = 4, new(.8), false
	metric.State, metric.Reason = "not enough data", "Required metric coverage is incomplete."
	metric.Delta, metric.Interval, metric.PValue, metric.RandomizationThreshold = nil, nil, nil, nil
	metric.Permutations = 0
	out = formatExperimentReport(report)
	assert.Contains(out, "subjects=4, units=4/5, coverage=80.0%")
	assert.Contains(out, "duration_p50_seconds: not enough data")
	assert.NotContains(out, "95% randomization threshold=")
	assert.NotContains(out, "bootstrap 95% interval")
}
