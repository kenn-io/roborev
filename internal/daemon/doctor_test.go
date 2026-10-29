package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

func TestDoctorAgentsReportsDaemonView(t *testing.T) {
	binDir := t.TempDir()
	gooseBin := filepath.Join(binDir, "goose-acp")
	script := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		gooseBin += ".cmd"
		script = "@echo off\r\nexit /b 0\r\n"
	}
	require.NoError(t, os.WriteFile(gooseBin, []byte(script), 0o755))
	t.Setenv("PATH", binDir)

	server, _, _ := newTestServer(t)
	get := func(repo string, agents ...string) (int, DoctorAgentsOutput) {
		query := url.Values{"repo": {repo}, "agent": agents}
		req := httptest.NewRequest(http.MethodGet, "/api/doctor/agents?"+query.Encode(), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		var out DoctorAgentsOutput
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out.Body))
		return w.Code, out
	}

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"),
		[]byte("[acp.goose]\ncommand = \"goose-acp\"\n\n[[hooks]]\nevent = \"review.failed\"\ntype = \"kata\"\n"), 0o600))
	code, out := get(repo, "acp.goose", "no-such-agent")
	require.Equal(t, http.StatusOK, code)
	assert := assert.New(t)
	assert.Equal(binDir, out.Body.PathEnv)
	assert.Empty(out.Body.RepoConfigError)
	byName := map[string]agent.Diagnosis{}
	for _, d := range out.Body.Agents {
		byName[d.Name] = d
	}
	assert.True(byName["acp.goose"].Available, "repo ACP agent resolves on the daemon PATH")
	assert.False(byName["codex"].Available, "codex is not on the daemon PATH")

	require.Len(t, out.Body.Requested, 2)
	assert.True(out.Body.Requested[0].Available)
	assert.Equal("no-such-agent", out.Body.Requested[1].Name)
	assert.Contains(out.Body.Requested[1].Error, "unknown agent")

	require.Len(t, out.Body.HookTools, 1)
	assert.Equal("kata", out.Body.HookTools[0].Name)
	assert.False(out.Body.HookTools[0].Available, "kata is not on the daemon PATH")

	broken := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(broken, ".roborev.toml"), []byte("agent = [\n"), 0o600))
	code, out = get(broken)
	require.Equal(t, http.StatusOK, code)
	assert.NotEmpty(out.Body.RepoConfigError)
}

func TestResolveDoctorPanels(t *testing.T) {
	const base = `
[review]
hook_review_panel = "guard"
default_panel = "guard"

[review.subagents.ok]
agent = "test"

[review.panels.guard]
members = ["ok"]
synthesis_agent = "test"

[review.panels.alt]
members = ["ok"]
synthesis_agent = "test"
`
	const experiment = `
[experiments.try]
enabled = true
ratio = 0.5
workflows = [%q]

[experiments.try.config%s]
%s
`
	type want struct {
		name        string
		experiment  string
		usedFor     []string
		errContains string
	}
	both := []string{"post_commit", "manual"}
	tests := []struct {
		name   string
		config string
		want   []want
	}{
		{
			name:   "default config only",
			config: base,
			want:   []want{{name: "guard", usedFor: both}},
		},
		{
			name:   "experiment selecting a different panel is listed separately",
			config: base + fmt.Sprintf(experiment, "review", ".review", `hook_review_panel = "alt"`),
			want: []want{
				{name: "guard", usedFor: both},
				{name: "alt", experiment: "try", usedFor: []string{"post_commit"}},
			},
		},
		{
			name:   "experiment changing a member under the same panel name is listed separately",
			config: base + fmt.Sprintf(experiment, "review", ".review.subagents.ok", `agent = "codex"`),
			want: []want{
				{name: "guard", usedFor: both},
				{name: "guard", experiment: "try", usedFor: both, errContains: `panel member "ok"`},
			},
		},
		{
			name:   "experiment that does not change the panel is merged",
			config: base + fmt.Sprintf(experiment, "review", "", `review_reasoning = "fast"`),
			want:   []want{{name: "guard", usedFor: both}},
		},
		{
			name:   "experiment for the CI workflow does not affect local reviews",
			config: base + fmt.Sprintf(experiment, "ci", ".review", `hook_review_panel = "alt"`),
			want:   []want{{name: "guard", usedFor: both}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			repo := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"), []byte(tt.config), 0o600))
			repoCfg, rawRepo, err := config.LoadRepoConfigWithRaw(repo)
			require.NoError(t, err)

			panels, err := ResolveDoctorPanels(repo, repoCfg, rawRepo, config.DefaultConfig())
			require.NoError(t, err)
			require.Len(t, panels, len(tt.want), "%+v", panels)
			for i, w := range tt.want {
				assert := assert.New(t)
				p := panels[i]
				assert.Equal(w.name, p.Name)
				assert.Equal(w.experiment, p.Experiment)
				assert.Equal(w.usedFor, p.UsedFor)
				if w.errContains == "" {
					assert.Empty(p.Error)
					assert.True(p.Synthesis.Available)
				} else {
					assert.Contains(p.Error, w.errContains)
				}
			}
		})
	}
}

func TestResolveDoctorPanelsRejectsUnselectableMember(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"), []byte(`
[review]
hook_review_panel = "guard"

[review.subagents.ok]
agent = "test"

[review.subagents.missing]
agent = "codex"
allow_failure = true

[review.panels.guard]
members = ["ok", "missing"]
synthesis_agent = "test"
`), 0o600))
	repoCfg, rawRepo, err := config.LoadRepoConfigWithRaw(repo)
	require.NoError(t, err)

	panels, err := ResolveDoctorPanels(repo, repoCfg, rawRepo, config.DefaultConfig())
	require.NoError(t, err)
	require.Len(t, panels, 1)
	assert.Contains(t, panels[0].Error, `panel member "missing"`,
		"queueing selects every member's agent, so allow_failure does not help")
}

func TestDoctorAgentsUsageProvider(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		missingToken bool
	}{
		{name: "authenticated", status: 200}, {name: "rejected", status: 401}, {name: "file error", missingToken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTSVIEW_SERVER_TOKEN", "daemon-token")
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/base/api/ping", r.URL.Path)
				assert.Equal(t, "Bearer daemon-token", r.Header.Get("Authorization"))
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"ok":true,"service":"agentsview","detail":"private-server-body"}`)
			}))
			defer srv.Close()
			server, _, _ := newTestServer(t)
			cfg := config.DefaultConfig()
			cfg.AgentsView.URL = srv.URL + "/base"
			if tc.missingToken {
				cfg.AgentsView.TokenFile = filepath.Join(t.TempDir(), "missing")
			}
			server.configWatcher.cfgMu.Lock()
			server.configWatcher.cfg = cfg
			server.configWatcher.cfgMu.Unlock()
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/doctor/agents", nil))
			require.Equal(t, http.StatusOK, w.Code)
			var out struct {
				UsageProvider *struct {
					Provider string `json:"provider"`
					URL      string `json:"url"`
					Error    string `json:"error"`
				} `json:"usage_provider"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
			require.NotNil(t, out.UsageProvider)
			assert.Equal(t, "agentsview", out.UsageProvider.Provider)
			assert.Equal(t, cfg.AgentsView.URL, out.UsageProvider.URL)
			if tc.missingToken {
				assert.Contains(t, out.UsageProvider.Error, "token_file")
				assert.Zero(t, calls)
			} else if tc.status == 401 {
				assert.Contains(t, out.UsageProvider.Error, "rejected the token")
			} else {
				assert.Empty(t, out.UsageProvider.Error)
			}
			assert.NotContains(t, w.Body.String(), "daemon-token")
			assert.NotContains(t, w.Body.String(), "private-server-body")
		})
	}
}
