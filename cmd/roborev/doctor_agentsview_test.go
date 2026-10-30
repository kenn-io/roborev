package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/pkg/client/generated"
)

func TestDoctorUsageProvider(t *testing.T) {
	for _, status := range []int{200, 401, 403, 409, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Setenv("AGENTSVIEW_SERVER_TOKEN", "fixture-token")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/base/api/ping", r.URL.Path)
				assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
				w.WriteHeader(status)
				if status == 200 {
					fmt.Fprint(w, `{"ok":true,"service":"agentsview"}`)
				} else {
					fmt.Fprint(w, `{"error":{"candidates":["private-server-body"]}}`)
				}
			}))
			defer srv.Close()
			cfg := config.DefaultConfig()
			cfg.AgentsView.URL = srv.URL + "/base"
			env := &doctorEnv{ctx: t.Context(), global: cfg}
			got := findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.usage")
			assert.Contains(t, got.Summary, "AgentsView")
			assert.Contains(t, got.Summary, srv.URL+"/base")
			assert.Equal(t, doctorWarn, got.Status)
			assert.Contains(t, got.Summary, "this shell only")
			if status == 401 || status == 403 {
				assert.Contains(t, fmt.Sprint(got.Details), "rejected the token")
			}
			assert.NotContains(t, fmt.Sprint(got), "fixture-token")
			assert.NotContains(t, fmt.Sprint(got), "private-server-body")
		})
	}
	for _, endpoint := range []string{"", "https://usage.example/{session_id}"} {
		cfg := config.DefaultConfig()
		cfg.Cost.Endpoint = endpoint
		got := findDoctorCheck(t, checkDoctorIntegrations(&doctorEnv{ctx: t.Context(), global: cfg}), "integrations.usage")
		if endpoint == "" {
			assert.Contains(t, got.Summary, "local CLI")
		} else {
			assert.Contains(t, got.Summary, "[cost] endpoint")
		}
	}
}

func TestDoctorUsageProviderUsesDaemonResult(t *testing.T) {
	for _, tc := range []struct {
		daemonError string
		want        doctorStatus
	}{
		{"", doctorOK}, {"AgentsView rejected the token (HTTP 401)", doctorWarn},
	} {
		t.Run(string(tc.want), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				fmt.Fprint(w, `{"ok":true,"service":"agentsview"}`)
			}))
			defer srv.Close()
			// Conversion must preserve the daemon's diagnostic; shell credentials and
			// its probe outcome cannot override the process that actually prices jobs.
			var response generated.DoctorAgentsResponse
			require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{"usage_provider":{"provider":"agentsview","url":"https://daemon.example","error":%q}}`, tc.daemonError)), &response))
			cfg := config.DefaultConfig()
			cfg.AgentsView.URL = srv.URL
			t.Setenv("AGENTSVIEW_SERVER_TOKEN", "shell-token")
			if tc.daemonError == "" {
				t.Setenv("AGENTSVIEW_SERVER_TOKEN", "")
				cfg.AgentsView.TokenFile = filepath.Join(t.TempDir(), "missing-shell-token")
			}
			env := &doctorEnv{ctx: t.Context(), global: cfg, daemonAgents: doctorAgentsFromAPI(&response)}
			got := checkDoctorUsageProvider(env)
			assert.Equal(t, tc.want, got.Status)
			assert.Contains(t, got.Summary, "daemon.example")
			assert.Contains(t, got.Summary, "daemon")
			assert.Equal(t, 0, calls)
			if tc.daemonError != "" {
				assert.Contains(t, fmt.Sprint(got.Details), tc.daemonError)
			}
		})
	}
}

func TestDoctorUsageProviderFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		agents     *doctorDaemonAgents
		requestErr error
		pingErr    error
		globalErr  error
		want       string
	}{
		{name: "unavailable", pingErr: errors.New("daemon is unreachable"), want: "daemon is unreachable"},
		{name: "old daemon", agents: doctorAgentsFromAPI(&generated.DoctorAgentsResponse{}), want: "restart"},
		{name: "request failed", requestErr: errors.New("diagnostic request failed"), want: "diagnostic request failed"},
		{name: "invalid config", globalErr: errors.New("configuration failed"), want: "configuration failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			env := &doctorEnv{ctx: t.Context(), global: cfg, globalErr: tc.globalErr, daemonAgents: tc.agents, daemonAgentsErr: tc.requestErr, pingErr: tc.pingErr}
			got := checkDoctorUsageProvider(env)
			assert.Equal(t, doctorWarn, got.Status)
			assert.Contains(t, got.Summary, "this shell only")
			assert.Contains(t, fmt.Sprint(got.Summary, got.Details), tc.want)
			if tc.globalErr != nil {
				assert.NotContains(t, got.Summary, "local CLI")
			}
		})
	}
}
