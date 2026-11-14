package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/pkg/client/generated"
)

func TestDoctorKataRubrics(t *testing.T) {
	audience := "Vault rubrics are readable by all issue readers and metadata writers."
	for _, tc := range []struct {
		name, mode   string
		probe        *daemon.DoctorRubricSetup
		status       doctorStatus
		summary, fix string
		details      []string
	}{
		{name: "off", mode: "off", probe: &daemon.DoctorRubricSetup{Mode: "off", State: "off"}, status: doctorInfo, summary: "Kata rubric archiving: mode off"},
		{name: "off without daemon", mode: "off", status: doctorInfo, summary: "Kata rubric archiving: mode off"},
		{name: "daemon current overrides local off", mode: "off", probe: &daemon.DoctorRubricSetup{Mode: "current", State: "kata_missing"}, status: doctorFail, summary: "Kata rubric archiving: mode current, kata CLI unavailable on daemon PATH", fix: "install kata on the daemon's PATH and restart the daemon", details: []string{audience}},
		{name: "daemon off overrides local current", mode: "current", probe: &daemon.DoctorRubricSetup{Mode: "off", State: "off"}, status: doctorInfo, summary: "Kata rubric archiving: mode off"},
		{name: "key", mode: "current", probe: &daemon.DoctorRubricSetup{Mode: "current", State: "ok", Project: "example"}, status: doctorOK, summary: "Kata rubric archiving: mode current, Docbank key resolves on the selected Kata daemon; vault rubrics are readable by all issue readers and metadata writers", details: []string{audience}},
		{name: "kata", mode: "current", probe: &daemon.DoctorRubricSetup{Mode: "current", State: "kata_missing"}, status: doctorFail, summary: "Kata rubric archiving: mode current, kata CLI unavailable on daemon PATH", fix: "install kata on the daemon's PATH and restart the daemon", details: []string{audience}},
		{name: "binding", mode: "current", probe: &daemon.DoctorRubricSetup{Mode: "current", State: "binding_missing"}, status: doctorFail, summary: "Kata rubric archiving: mode current, project binding missing", fix: "bind this repository to a Kata project", details: []string{audience}},
		{name: "invalid binding", mode: "current", probe: &daemon.DoctorRubricSetup{Mode: "current", State: "binding_error", Error: "invalid TOML"}, status: doctorFail, summary: "Kata rubric archiving: mode current, project binding invalid", fix: "repair the Kata binding configuration", details: []string{audience, "invalid TOML"}},
		{name: "missing credential", mode: "current", probe: &daemon.DoctorRubricSetup{Mode: "current", State: "missing", Project: "example"}, status: doctorFail, summary: "Kata rubric archiving: mode current, Docbank credential missing", fix: "check kata docbank setup and the daemon's credential", details: []string{audience, "Kata project: example"}},
		{name: "unknown", mode: "current", probe: &daemon.DoctorRubricSetup{Mode: "current", State: "unknown", Project: "example"}, status: doctorInfo, summary: "Kata rubric archiving: mode current, Docbank state unknown", details: []string{audience, "Kata project: example"}},
		{name: "old daemon", mode: "current", status: doctorInfo, summary: "Kata rubric archiving: mode current, daemon setup unverified", fix: "start or restart the roborev daemon to verify its Kata setup", details: []string{audience}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := &doctorEnv{repoPath: t.TempDir(), global: &config.Config{KataRubrics: config.KataRubricsConfig{Mode: tc.mode}}, daemonAgents: &doctorDaemonAgents{RubricSetup: tc.probe}}
			checks := checkDoctorKataRubrics(env)
			require.Len(t, checks, 1)
			assert.Equal(t, tc.status, checks[0].Status)
			assert.Equal(t, tc.summary, checks[0].Summary)
			assert.Equal(t, tc.fix, checks[0].Fix)
			assert.Equal(t, tc.details, checks[0].Details)
		})
	}
	t.Run("no repository", func(t *testing.T) {
		env := &doctorEnv{global: &config.Config{KataRubrics: config.KataRubricsConfig{Mode: "current"}}}
		assert.Empty(t, checkDoctorKataRubrics(env))
	})
}

func TestDoctorRubricsTrustsDaemonOverShell(t *testing.T) {
	t.Setenv("PATH", writeFakeAgentBinary(t, "kata"))
	env := &doctorEnv{repoPath: t.TempDir(), global: &config.Config{KataRubrics: config.KataRubricsConfig{Mode: "current"}}, daemonAgents: &doctorDaemonAgents{RubricSetup: &daemon.DoctorRubricSetup{Mode: "current", State: "kata_missing"}}}
	checks := checkDoctorKataRubrics(env)
	require.Len(t, checks, 1)
	assert.Equal(t, doctorFail, checks[0].Status)
	assert.Equal(t, "Kata rubric archiving: mode current, kata CLI unavailable on daemon PATH", checks[0].Summary)
}

func TestDoctorRubricsReportOutput(t *testing.T) {
	for _, state := range []string{"ok", "missing", "unknown"} {
		t.Run(state, func(t *testing.T) {
			api := &generated.DoctorAgentsResponse{RubricSetup: &generated.DoctorRubricSetup{Mode: "current", State: state, Project: new("example")}}
			env := &doctorEnv{repoPath: t.TempDir(), global: &config.Config{KataRubrics: config.KataRubricsConfig{Mode: "current"}}, daemonAgents: doctorAgentsFromAPI(api)}
			checks := checkDoctorKataRubrics(env)
			require.Len(t, checks, 1)
			report := doctorReport{Checks: checks}
			var output bytes.Buffer
			renderDoctorReport(&output, report)
			wantScope := "example"
			if state == "ok" {
				wantScope = "Docbank key resolves on the selected Kata daemon"
			}
			assert.Contains(t, output.String(), wantScope)
			assert.Contains(t, output.String(), "readable by all issue readers and metadata writers")
			data, err := json.Marshal(report)
			require.NoError(t, err)
			assert.Contains(t, string(data), wantScope)
			assert.Contains(t, string(data), "readable by all issue readers and metadata writers")
		})
	}
}
