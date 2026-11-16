package main

import (
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/client/generated"
)

// The threshold is per PR and exclusive; a PR with many runs spread over
// the week is healthy, and every PR above the threshold is listed.
func TestDoctorCIBursts(t *testing.T) {
	pr := func(number int64, runs, peak int64) generated.CIPRSummary {
		return generated.CIPRSummary{GithubRepo: "acme/api", PrNumber: number, Runs: runs, PeakHourRuns: peak}
	}
	tests := []struct {
		name        string
		prs         []generated.CIPRSummary
		wantStatus  doctorStatus
		wantDetails []string
	}{
		{
			name: "many runs over the week is healthy", prs: []generated.CIPRSummary{pr(1, 40, 3)},
			wantStatus: doctorOK, wantDetails: []string{"busiest: acme/api#1 with 3 runs within an hour"},
		},
		{
			name: "ten in an hour is healthy", prs: []generated.CIPRSummary{pr(1, 12, 10), pr(2, 2, 2)},
			wantStatus: doctorOK, wantDetails: []string{"busiest: acme/api#1 with 10 runs within an hour"},
		},
		{
			name: "eleven in an hour warns", prs: []generated.CIPRSummary{pr(1, 11, 11)},
			wantStatus: doctorWarn, wantDetails: []string{"acme/api#1: 11 runs within an hour"},
		},
		{
			name: "every burst PR is listed by peak", prs: []generated.CIPRSummary{pr(1, 50, 4), pr(2, 20, 12), pr(3, 15, 15)},
			wantStatus: doctorWarn, wantDetails: []string{"acme/api#3: 15 runs within an hour", "acme/api#2: 12 runs within an hour"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := doctorCIEnv(t, &generated.CISummary{TotalPanels: 1, Prs: tt.prs})
			got := findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.ci_bursts")
			assert := assert.New(t)
			assert.Equal(tt.wantStatus, got.Status)
			assert.Equal("integrations", got.Category)
			assert.Equal(tt.wantDetails, got.Details)
			assert.Contains(got.Summary, "7 days")
			assert.Equal(tt.wantStatus == doctorWarn, got.Fix != "")
		})
	}
}

func TestDoctorCIBurstsUnavailable(t *testing.T) {
	tests := []struct {
		name      string
		ci        *generated.CISummary
		apiErr    error
		disabled  bool
		down      bool
		noRepos   bool
		wantCalls int
		wantInfo  bool
	}{
		{name: "disabled", disabled: true},
		{name: "daemon down", down: true},
		{name: "old daemon omits CI", wantCalls: 1},
		{name: "no PRs", ci: &generated.CISummary{}, wantCalls: 1},
		{name: "API unavailable", apiErr: errors.New("summary unavailable"), wantCalls: 1, wantInfo: true},
		{name: "settings and burst warnings coexist", ci: &generated.CISummary{TotalPanels: 11, Prs: []generated.CIPRSummary{{GithubRepo: "acme/api", PrNumber: 1, Runs: 11, PeakHourRuns: 11}}}, noRepos: true, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := doctorCIEnv(t, tt.ci)
			calls := 0
			f := env.daemon.(fakeDoctorDaemon)
			f.summaryCalls, f.summaryErr = &calls, tt.apiErr
			env.daemon = f
			env.global.CI.Enabled = !tt.disabled
			if tt.noRepos {
				env.global.CI.Repos = nil
			}
			if tt.down {
				env.ping, env.pingErr = nil, errors.New("connection refused")
			}
			checks := checkDoctorIntegrations(env)
			assert.Equal(t, tt.wantCalls, calls)
			if tt.noRepos {
				assert.Equal(t, doctorWarn, findDoctorCheck(t, checks, "integrations.ci").Status)
				assert.Equal(t, doctorWarn, findDoctorCheck(t, checks, "integrations.ci_bursts").Status)
				return
			}
			if tt.wantInfo {
				got := findDoctorCheck(t, checks, "integrations.ci_bursts")
				assert.Equal(t, doctorInfo, got.Status)
				assert.Contains(t, strings.Join(got.Details, "\n"), "summary unavailable")
				return
			}
			assert.False(t, slices.ContainsFunc(checks, func(c doctorCheck) bool { return c.ID == "integrations.ci_bursts" }))
		})
	}
}

func TestDoctorCIBurstsReadOnlyReportKeepsFailures(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	configPath := filepath.Join(dataDir, "config.toml")
	configText := []byte("default_agent = \"codex\"\n[ci]\nenabled = true\nrepos = [\"example/project\"]\n")
	require.NoError(t, os.WriteFile(configPath, configText, 0o600))
	jobs := make([]generated.ReviewJob, 3)
	for i := range jobs {
		jobs[i] = generated.ReviewJob{Agent: "codex", ErrorData: new("provider unavailable"), EnqueuedAt: time.Now()}
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	env := loadDoctorEnv(t.Context(), "", fakeDoctorDaemon{
		ping: &daemon.PingInfo{OK: true}, agents: &doctorDaemonAgents{}, jobs: jobs,
		summary: &generated.Summary{Ci: &generated.CISummary{TotalPanels: 11, Prs: []generated.CIPRSummary{{GithubRepo: "acme/api", PrNumber: 1, Runs: 11, PeakHourRuns: 11}}}},
	})
	report := buildDoctorReport(env)
	assert := assert.New(t)
	assert.Equal(doctorWarn, findDoctorCheck(t, report.Checks, "integrations.ci_bursts").Status)
	assert.Equal(doctorWarn, findDoctorCheck(t, report.Checks, "jobs.failed").Status)
	// A read-only report must not create a database, daemon runtime, logs,
	// or modified configuration in its isolated data directory.
	files, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal("config.toml", files[0].Name())
	contents, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(configText, contents)
}

// The live adapter must read the all-repository cohort with a duration the real
// daemon accepts. Summary aggregation and warning policy have their own tests.
func TestLiveDoctorCIBurstsRequestsAllRepos(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	db := testutil.OpenTestDB(t)
	server := daemon.NewServer(db, config.DefaultConfig(), "")
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	handler := server.Handler()
	var requests []string
	var all, since, repo, branch string
	var responseStatus int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		query := r.URL.Query()
		all, since, repo, branch = query.Get("all"), query.Get("since"), query.Get("repo"), query.Get("branch")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		responseStatus = response.Code
		maps.Copy(w.Header(), response.Header())
		w.WriteHeader(response.Code)
		_, err := w.Write(response.Body.Bytes())
		assert.NoError(t, err)
	}))
	defer srv.Close()
	summary, err := newDoctorDaemon(mustParseEndpoint(t, srv.URL)).Summary(t.Context())
	assert.Equal(t, http.StatusOK, responseStatus)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.NotNil(summary)
	assert.Equal([]string{"GET /api/summary"}, requests)
	assert.Equal("true", all)
	assert.Equal("168h", since)
	assert.Empty(repo)
	assert.Empty(branch)
}

func doctorCIEnv(t *testing.T, ci *generated.CISummary) *doctorEnv {
	t.Helper()
	return &doctorEnv{
		ctx: t.Context(), now: time.Date(2026, 1, 12, 12, 0, 0, 0, time.UTC),
		global: &config.Config{CI: config.CIConfig{Enabled: true, Repos: []string{"example/project"}}},
		ping:   &daemon.PingInfo{OK: true},
		daemon: fakeDoctorDaemon{summary: &generated.Summary{Ci: ci}},
	}
}
