package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/kata/katafake"
)

func kataDoctorEnv(local config.KataIntegrationConfig, status daemon.KataStatusBody) *doctorEnv {
	return &doctorEnv{
		ctx:          context.Background(),
		global:       &config.Config{Integrations: config.IntegrationsConfig{Kata: local}},
		ping:         &daemon.PingInfo{OK: true},
		daemonAgents: &doctorDaemonAgents{},
		kata:         &status,
	}
}

func readyKata() daemon.KataStatusBody {
	return daemon.KataStatusBody{
		Enabled: true, State: "ready", Project: "reviews", DefaultProject: "reviews", Teammate: "roborev",
		ServerVersion: "v0.19.0", APISchemaVersion: "0.28.0", SupportsReplies: true, Message: "The Kata connection is ready.",
	}
}

func TestDoctorKataStatesFromTheDaemon(t *testing.T) {
	ready := readyKata()
	oldServer := readyKata()
	oldServer.APISchemaVersion, oldServer.SupportsReplies = "0.27.0", false
	withState := func(state, message string) daemon.KataStatusBody {
		s := readyKata()
		s.State, s.Message = state, message
		return s
	}
	tests := []struct {
		name       string
		status     daemon.KataStatusBody
		wantStatus doctorStatus
		wantIn     []string
		wantFix    string
	}{
		{"ready with replies", ready, doctorOK, []string{`project "reviews"`, "v0.19.0", "typed replies supported"}, ""},
		{"ready on an older server", oldServer, doctorInfo, []string{"typed replies are not supported", "0.27.0", "0.28.0"}, "upgrade Kata"},
		{"unreachable", withState("unreachable", "The Kata server is unavailable."), doctorWarn, []string{"unreachable"}, "endpoint"},
		{"authentication required", withState("authentication_required", "Kata rejected the credential."), doctorWarn, []string{"authentication_required"}, "api_key_file"},
		{"incompatible", withState("incompatible", "not a compatible Kata API"), doctorWarn, []string{"incompatible"}, "0.21.0"},
		{"project not found", withState("project_not_found", "The Kata project was not found on the server."), doctorWarn, []string{"project_not_found", `project: "reviews"`}, "default_project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := kataDoctorEnv(config.KataIntegrationConfig{Enabled: true}, tt.status)
			got := findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.kata")
			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Equal(t, "integrations", got.Category)
			text := got.Summary + " " + strings.Join(got.Details, " ")
			for _, want := range tt.wantIn {
				assert.Contains(t, text, want)
			}
			if tt.wantFix == "" {
				assert.Empty(t, got.Fix)
			} else {
				assert.Contains(t, got.Fix, tt.wantFix)
			}
		})
	}
}

func TestDoctorKataIsQuietWhenNothingEnablesIt(t *testing.T) {
	off := daemon.KataStatusBody{State: "disabled", Message: "disabled"}
	env := kataDoctorEnv(config.KataIntegrationConfig{}, off)
	assert.Empty(t, checkDoctorIntegrations(env))

	env.kata, env.kataErr = nil, assert.AnError // an older daemon: no status at all
	assert.Empty(t, checkDoctorIntegrations(env))
}

func TestDoctorKataMismatchBetweenConfigAndDaemon(t *testing.T) {
	env := kataDoctorEnv(config.KataIntegrationConfig{Enabled: true}, daemon.KataStatusBody{State: "disabled", Message: "disabled"})
	got := findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.kata")
	assert.Equal(t, doctorWarn, got.Status)
	assert.Contains(t, got.Summary, "daemon has it disabled")

	env.kata, env.kataErr = nil, assert.AnError
	got = findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.kata")
	assert.Equal(t, doctorInfo, got.Status)
	assert.Contains(t, got.Summary, "could not be read")
}

func TestDoctorKataWhenTheDaemonCannotBeAsked(t *testing.T) {
	env := kataDoctorEnv(config.KataIntegrationConfig{Enabled: true, DefaultProject: "reviews"}, daemon.KataStatusBody{})
	env.daemonAgents, env.kata, env.ping, env.pingErr = nil, nil, nil, assert.AnError
	got := findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.kata")
	assert.Equal(t, doctorInfo, got.Status)
	assert.Contains(t, got.Summary, "checked when the daemon is running")

	env.global.Integrations.Kata.Enabled = false
	assert.Empty(t, checkDoctorIntegrations(env))
}

// The daemon talks to Kata itself, so a kata hook does not need the kata CLI.
func TestDoctorKataHookDoesNotNeedTheCLIWhenTheConnectionIsOn(t *testing.T) {
	hooks := []config.HookConfig{{Event: "review.failed", Type: "kata"}}
	env := kataDoctorEnv(config.KataIntegrationConfig{Enabled: true}, readyKata())
	env.global.Hooks = hooks
	env.daemonAgents.HookTools = []agent.Diagnosis{{Name: "kata", Command: "kata", Error: "not found"}}
	checks := checkDoctorIntegrations(env)
	for _, c := range checks {
		assert.NotEqual(t, "integrations.hooks", c.ID, "%+v", c)
	}

	env.kata = &daemon.KataStatusBody{State: "disabled"}
	env.global.Integrations.Kata.Enabled = false
	got := findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.hooks")
	assert.Contains(t, got.Details[0], "the kata CLI is not on the daemon's PATH")
}

func TestDoctorKataReportNeverCarriesTheCredentialOrEndpoint(t *testing.T) {
	const secret = "synthetic-kata-secret-9f3a"
	local := config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.internal.example", APIKey: secret}
	for _, status := range []daemon.KataStatusBody{readyKata(), {Enabled: true, State: "authentication_required", Message: "Kata rejected the credential."}} {
		env := kataDoctorEnv(local, status)
		report := doctorReport{Checks: checkDoctorIntegrations(env)}
		var human strings.Builder
		renderDoctorReport(&human, report)
		encoded, err := json.Marshal(report)
		require.NoError(t, err)
		for _, text := range []string{human.String(), string(encoded)} {
			assert.NotContains(t, text, secret)
			assert.NotContains(t, text, "kata.internal.example")
		}
	}
}

// An unbound repository leaves project selection to the daemon.
func TestDoctorLeavesUnboundProjectSelectionToTheDaemon(t *testing.T) {
	status := readyKata()
	var asked []string
	d := fakeDoctorDaemon{ping: &daemon.PingInfo{OK: true}, agents: &doctorDaemonAgents{}, kata: &status, kataProjects: &asked}
	env := loadDoctorEnv(t.Context(), bindRepo(t, ""), d)
	assert.Equal(t, []string{""}, asked)
	assert.Empty(t, env.kataProject)
	require.NotNil(t, env.kata)
}

func TestDoctorDoesNotAskAboutKataWhenTheDaemonIsDown(t *testing.T) {
	var asked []string
	env := loadDoctorEnv(t.Context(), bindRepo(t, "other"), fakeDoctorDaemon{kataProjects: &asked})
	assert.Empty(t, asked)
	assert.Nil(t, env.kata)
}

func TestDoctorShowsTheProjectTheDaemonEvaluatedForTheLocalBinding(t *testing.T) {
	fake := katafake.New(t)
	var queries []string
	ep := kataDaemonObserved(t, enabledKata(fake), func(r *http.Request) { queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery) })
	live := newDoctorDaemon(ep)

	env := loadDoctorEnv(t.Context(), bindRepo(t, "other"), live)
	require.NotNil(t, env.kata, "%v", env.kataErr)
	assert.Equal(t, "ready", env.kata.State, env.kata.Message)
	assert.Equal(t, "other", env.kata.Project, "not the daemon's default, reviews")
	got := findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.kata")
	assert.Equal(t, doctorOK, got.Status)
	assert.Contains(t, got.Summary, `project "other"`)
	for _, q := range queries {
		if strings.HasPrefix(q, "/api/kata/status") {
			assert.Equal(t, "/api/kata/status?project=other", q, "only the project is sent, never a path on this machine")
		}
	}

	env = loadDoctorEnv(t.Context(), bindRepo(t, "ghost"), live)
	require.NotNil(t, env.kata)
	assert.Equal(t, "project_not_found", env.kata.State)
	got = findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.kata")
	assert.Equal(t, doctorWarn, got.Status)
	assert.Contains(t, strings.Join(got.Details, " "), `project: "ghost"`)

	env = loadDoctorEnv(t.Context(), bindRepo(t, ""), live)
	require.NotNil(t, env.kata)
	assert.Equal(t, "reviews", env.kata.Project, "the daemon's default project")
	assert.True(t, env.kata.SupportsReplies)
}

func TestDoctorReportsAnUnreadableLocalBinding(t *testing.T) {
	repo := bindRepo(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".kata.local.toml"), []byte("not = = toml"), 0o600))
	status := readyKata()
	d := fakeDoctorDaemon{ping: &daemon.PingInfo{OK: true}, agents: &doctorDaemonAgents{}, kata: &status}
	env := loadDoctorEnv(t.Context(), repo, d)
	env.global.Integrations.Kata.Enabled = true
	require.Error(t, env.kataBindingErr)
	checks := checkDoctorIntegrations(env)
	got := findDoctorCheck(t, checks, "integrations.kata.binding")
	assert.Equal(t, doctorWarn, got.Status)
	assert.Contains(t, got.Summary, "binding")
	assert.Contains(t, got.Fix, ".kata")
}

// A daemon that answers the status request with plain-text 404 predates it.
func TestDoctorKataStatusFromAnOldDaemonIsUnknown(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	ep, err := daemon.ParseEndpoint(ts.URL)
	require.NoError(t, err)
	got, err := newDoctorDaemon(ep).KataStatus(context.Background(), "")
	require.Error(t, err)
	assert.Nil(t, got)
}
