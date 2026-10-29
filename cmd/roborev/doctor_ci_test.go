package main

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
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

// These cases catch inclusive thresholds, a missing denominator guard, an
// unsupported sample floor, and losing one warning when both conditions hold.
func TestDoctorCILoadThresholds(t *testing.T) {
	tests := []struct {
		name               string
		total, closed, max int64
		wantStatus         doctorStatus
		wantThrottle       bool
		wantWorkers        bool
	}{
		{name: "five runs is healthy", total: 10, max: 5, wantStatus: doctorOK},
		{name: "six runs warns", total: 10, max: 6, wantStatus: doctorWarn, wantThrottle: true},
		{name: "exactly ten percent is healthy", total: 20, closed: 2, max: 5, wantStatus: doctorOK},
		{name: "above ten percent warns", total: 20, closed: 3, max: 5, wantStatus: doctorWarn, wantWorkers: true},
		{name: "low sample above ten percent warns", total: 9, closed: 1, max: 2, wantStatus: doctorWarn, wantWorkers: true},
		{name: "low sample exactly ten percent is healthy", total: 10, closed: 1, max: 2, wantStatus: doctorOK},
		{name: "one closed panel warns", total: 1, closed: 1, max: 1, wantStatus: doctorWarn, wantWorkers: true},
		{name: "both warnings survive", total: 9, closed: 1, max: 6, wantStatus: doctorWarn, wantThrottle: true, wantWorkers: true},
		{name: "large denominator does not overflow", total: math.MaxInt64, closed: math.MaxInt64 / 10, max: 5, wantStatus: doctorOK},
		{name: "large numerator does not overflow", total: math.MaxInt64, closed: math.MaxInt64/10 + 1, max: 5, wantStatus: doctorWarn, wantWorkers: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := doctorCIEnv(t, &generated.CISummary{
				TotalPanels: tt.total, ClosedBeforePost: tt.closed, RunsPerPrMax: tt.max,
			})
			got := findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.ci_load")
			assert := assert.New(t)
			assert.Equal(tt.wantStatus, got.Status)
			assert.Equal("integrations", got.Category)
			assert.Equal([]string{
				fmt.Sprintf("maximum runs per PR: %d", tt.max),
				fmt.Sprintf("%d of %d CI panel runs were closed before posting", tt.closed, tt.total),
			}, got.Details)
			assert.Contains(got.Summary, "7 days")
			assert.Equal(tt.wantThrottle, strings.Contains(got.Fix, "throttle_interval"))
			assert.Equal(tt.wantWorkers, strings.Contains(got.Fix, "max_workers"))
			assert.NotContains(got.Fix, "skip_drafts")
			if tt.wantWorkers {
				assert.Contains(got.Fix, "panel")
			}
		})
	}
}

func TestDoctorCILoadUnavailable(t *testing.T) {
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
		{name: "no panels", ci: &generated.CISummary{}, wantCalls: 1},
		{name: "API unavailable", apiErr: errors.New("summary unavailable"), wantCalls: 1, wantInfo: true},
		{name: "settings and load warnings coexist", ci: &generated.CISummary{TotalPanels: 6, RunsPerPrMax: 6}, noRepos: true, wantCalls: 1},
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
				assert.Equal(t, doctorWarn, findDoctorCheck(t, checks, "integrations.ci_load").Status)
				return
			}
			if tt.wantInfo {
				got := findDoctorCheck(t, checks, "integrations.ci_load")
				assert.Equal(t, doctorInfo, got.Status)
				assert.Contains(t, strings.Join(got.Details, "\n"), "summary unavailable")
				return
			}
			assert.False(t, slices.ContainsFunc(checks, func(c doctorCheck) bool { return c.ID == "integrations.ci_load" }))
		})
	}
}

func TestDoctorCILoadReadOnlyReportKeepsFailures(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	configPath := filepath.Join(dataDir, "config.toml")
	configText := []byte("default_agent = \"codex\"\n[ci]\nenabled = true\nrepos = [\"example/project\"]\n")
	require.NoError(t, os.WriteFile(configPath, configText, 0o600))
	jobs := make([]generated.ReviewJob, 3)
	for i := range jobs {
		jobs[i] = generated.ReviewJob{Agent: "codex", ErrorData: new("provider unavailable"), EnqueuedAt: time.Now()}
	}
	env := loadDoctorEnv(t.Context(), "", fakeDoctorDaemon{
		ping: &daemon.PingInfo{OK: true}, agents: &doctorDaemonAgents{}, jobs: jobs,
		summary: &generated.Summary{Ci: &generated.CISummary{TotalPanels: 6, RunsPerPrMax: 6}},
	})
	report := buildDoctorReport(env)
	assert := assert.New(t)
	assert.Equal(doctorWarn, findDoctorCheck(t, report.Checks, "integrations.ci_load").Status)
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
func TestLiveDoctorCILoadRequestsAllRepos(t *testing.T) {
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

func FuzzDoctorCILoadThresholds(f *testing.F) {
	for _, seed := range [][3]int64{
		{1, 0, 1},
		{1, 1, 1},
		{10, 1, 5},
		{10, 2, 6},
		{9, 1, 2},
		{20, 2, 5},
		{20, 3, 6},
		{math.MaxInt64, math.MaxInt64 / 10, 5},
		{math.MaxInt64, math.MaxInt64/10 + 1, 6},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, total, closed, max int64) {
		if total <= 0 || closed < 0 || closed > total || max < 1 || max > total {
			t.Skip("summary counters must respect panel aggregation invariants")
		}
		got := doctorCILoadSummary(&generated.CISummary{
			TotalPanels: total, ClosedBeforePost: closed, RunsPerPrMax: max,
		})
		require.Len(t, got, 1)
		// Arbitrary precision multiplication provides an independent oracle
		// for strict share comparisons, including at the int64 boundaries.
		numerator := new(big.Int).Mul(big.NewInt(closed), big.NewInt(10))
		wantClosed := numerator.Cmp(big.NewInt(total)) > 0
		wantRuns := max > 5
		wantStatus := doctorOK
		if wantClosed || wantRuns {
			wantStatus = doctorWarn
		}
		assert := assert.New(t)
		assert.Equal(wantStatus, got[0].Status)
		assert.Equal(wantRuns, strings.Contains(got[0].Fix, "throttle_interval"))
		assert.Equal(wantClosed, strings.Contains(got[0].Fix, "max_workers"))
	})
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
