package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/client/generated"
)

// fakeDoctorDaemon serves canned daemon responses. A nil ping means the
// daemon is down.
type fakeDoctorDaemon struct {
	ping   *daemon.PingInfo
	agents *doctorDaemonAgents
	jobs   []generated.ReviewJob
}

func (f fakeDoctorDaemon) Ping() (*daemon.PingInfo, error) {
	if f.ping == nil {
		return nil, errors.New("connection refused")
	}
	return f.ping, nil
}

func (f fakeDoctorDaemon) Agents(context.Context, string, []string) (*doctorDaemonAgents, error) {
	return f.agents, nil
}

func (f fakeDoctorDaemon) Status(context.Context) (*generated.DaemonStatus, error) {
	return &generated.DaemonStatus{}, nil
}

func (f fakeDoctorDaemon) Health(context.Context) (*generated.HealthStatus, error) {
	return &generated.HealthStatus{Healthy: true}, nil
}

func (f fakeDoctorDaemon) FailedJobs(context.Context, time.Time) ([]generated.ReviewJob, error) {
	return f.jobs, nil
}

func (f fakeDoctorDaemon) RepoTracked(context.Context, string) (bool, error) {
	return true, nil
}

func findDoctorCheck(t *testing.T, checks []doctorCheck, id string) doctorCheck {
	t.Helper()
	idx := slices.IndexFunc(checks, func(c doctorCheck) bool { return c.ID == id })
	require.GreaterOrEqual(t, idx, 0, "no check %q in %+v", id, checks)
	return checks[idx]
}

// writeFakeAgentBinary puts an executable named name in a fresh directory
// and returns that directory, for use as PATH.
func writeFakeAgentBinary(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		path += ".cmd"
		script = "@echo off\r\nexit /b 0\r\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return dir
}

func TestDoctorAgentsDaemonPathMismatch(t *testing.T) {
	t.Setenv("PATH", writeFakeAgentBinary(t, "claude"))

	env := &doctorEnv{
		ctx:    t.Context(),
		global: &config.Config{DefaultAgent: "claude-code"},
		ping:   &daemon.PingInfo{OK: true},
		daemonAgents: &doctorDaemonAgents{
			PathEnv: "/usr/bin:/bin",
			Agents: []agent.Diagnosis{
				{Name: "claude-code", Command: "claude", Error: `agent "claude-code" unavailable`},
				{Name: "codex", Command: "codex", Error: `agent "codex" unavailable`},
			},
			Requested: []agent.Diagnosis{
				{Name: "claude-code", Command: "claude", Error: `agent "claude-code" unavailable`},
			},
		},
	}
	checks := checkDoctorAgents(env)

	assert := assert.New(t)
	installed := findDoctorCheck(t, checks, "agents.installed")
	assert.Equal(doctorFail, installed.Status)
	assert.Contains(installed.Summary, "the daemon")

	mismatch := findDoctorCheck(t, checks, "agents.daemon_path")
	assert.Equal(doctorWarn, mismatch.Status)
	require.Len(t, mismatch.Details, 2)
	assert.Contains(mismatch.Details[0], "claude-code: found at ")
	assert.Equal("daemon PATH: /usr/bin:/bin", mismatch.Details[1])

	review := findDoctorCheck(t, checks, "agents.review")
	assert.Equal(doctorFail, review.Status)
	assert.Contains(review.Fix, "roborev daemon restart")

	// The review agent is already reported; the configured-agents check must
	// not repeat it.
	assert.False(slices.ContainsFunc(checks, func(c doctorCheck) bool { return c.ID == "agents.configured" }), "%+v", checks)
}

func TestDoctorAgentsTrustsDaemonOverShell(t *testing.T) {
	// The shell can run claude, but a daemon that did not resolve the name
	// (for example an older daemon that predates the agent) must not be
	// reported as able to run it.
	t.Setenv("PATH", writeFakeAgentBinary(t, "claude"))
	env := &doctorEnv{
		ctx:          t.Context(),
		global:       &config.Config{DefaultAgent: "claude-code"},
		ping:         &daemon.PingInfo{OK: true},
		daemonAgents: &doctorDaemonAgents{},
	}
	review, _ := checkDoctorReviewAgent(env)
	assert.Equal(t, doctorFail, review.Status)
	assert.Contains(t, review.Details[0], "did not resolve this agent")
}

func TestDoctorHookToolsUseDaemonView(t *testing.T) {
	t.Setenv("PATH", writeFakeAgentBinary(t, "kata"))
	hooks := []config.HookConfig{{Event: "review.failed", Type: "kata"}}
	env := &doctorEnv{
		ctx:    t.Context(),
		global: &config.Config{Hooks: hooks},
		ping:   &daemon.PingInfo{OK: true},
		daemonAgents: &doctorDaemonAgents{HookTools: []agent.Diagnosis{
			{Name: "kata", Command: "kata", Error: "not found"},
		}},
	}
	got := findDoctorCheck(t, checkDoctorIntegrations(env), "integrations.hooks")
	assert.Equal(t, doctorWarn, got.Status)
	assert.Equal(t, []string{`hook 1 (event "review.failed"): the kata CLI is not on the daemon's PATH`}, got.Details)

	env.daemonAgents.HookTools[0] = agent.Diagnosis{Name: "kata", Available: true}
	assert.Empty(t, checkDoctorIntegrations(env))
}

func TestDoctorAgentsUnknownPreferredIgnoresBackup(t *testing.T) {
	t.Setenv("PATH", writeFakeAgentBinary(t, "gemini"))

	env := &doctorEnv{
		ctx:    t.Context(),
		global: &config.Config{DefaultAgent: "acp.missing", ReviewBackupAgent: "gemini"},
	}
	review, _ := checkDoctorReviewAgent(env)
	assert.Equal(t, doctorFail, review.Status)
	assert.Equal(t, "reviews will fail: agent acp.missing is not a known agent", review.Summary)
}

func TestDoctorAgentsBackupFallback(t *testing.T) {
	t.Setenv("PATH", writeFakeAgentBinary(t, "gemini"))

	env := &doctorEnv{
		ctx:    t.Context(),
		global: &config.Config{DefaultAgent: "codex", ReviewBackupAgent: "gemini", CodexCmd: "codex"},
	}
	review, _ := checkDoctorReviewAgent(env)
	assert.Equal(t, doctorWarn, review.Status)
	assert.Contains(t, review.Summary, "backup agent gemini")
}

const securityPanelRepoConfig = `
[review]
hook_review_panel = "guard"

[review.subagents.sec]
agent = "codex"
review_type = "security"

[review.panels.guard]
members = ["sec"]
`

func TestDoctorGuidelines(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]string
		globalCfg  string
		wantStatus doctorStatus
		wantInSum  string
	}{
		{
			name:       "security panel without guidelines is critical",
			files:      map[string]string{".roborev.toml": securityPanelRepoConfig},
			wantStatus: doctorFail,
			wantInSum:  "security reviews are enabled",
		},
		{
			name: "security panel with guidelines that skip security",
			files: map[string]string{
				".roborev.toml": securityPanelRepoConfig,
				"REVIEW.md":     "Prefer small functions.\n",
			},
			wantStatus: doctorWarn,
			wantInSum:  "do not describe security",
		},
		{
			name: "security panel with a threat model",
			files: map[string]string{
				".roborev.toml": securityPanelRepoConfig,
				"REVIEW.md":     "## Threat model\nRequest bodies are untrusted.\n",
			},
			wantStatus: doctorOK,
		},
		{
			name:       "no security reviews and no guidelines",
			files:      map[string]string{"main.go": "package main\n"},
			wantStatus: doctorWarn,
			wantInSum:  "no review guidelines",
		},
		{
			name:       "guidelines without security reviews",
			files:      map[string]string{"REVIEW.md": "Prefer small functions.\n"},
			wantStatus: doctorOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dataDir)
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(tt.globalCfg), 0o600))

			repo := testutil.NewTestRepo(t)
			for name, content := range tt.files {
				repo.CommitFile(name, content, "add "+name)
			}

			env := loadDoctorEnv(t.Context(), repo.Root, fakeDoctorDaemon{})
			require.NoError(t, env.globalErr)
			require.NoError(t, env.repoErr)
			got := findDoctorCheck(t, checkDoctorGuidelines(env), "repo.guidelines")
			assert.Equal(t, tt.wantStatus, got.Status, "%+v", got)
			assert.Contains(t, got.Summary, tt.wantInSum)
		})
	}
}

func TestDoctorFailedJobs(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour)
	old := now.Add(-8 * 24 * time.Hour)
	job := func(agentName, errMsg string, enqueued time.Time) generated.ReviewJob {
		return generated.ReviewJob{Agent: agentName, ErrorData: &errMsg, EnqueuedAt: enqueued}
	}

	tests := []struct {
		name       string
		jobs       []generated.ReviewJob
		wantStatus doctorStatus
		wantFirst  string
	}{
		{
			name: "repeated failures for one agent warn",
			jobs: []generated.ReviewJob{
				job("codex", "quota exceeded\nretry later", recent),
				job("codex", "quota exceeded", recent),
				job("codex", "timeout", recent),
				job("gemini", "auth expired", recent),
				job("codex", "quota exceeded", old),
			},
			wantStatus: doctorWarn,
			wantFirst:  "codex: 3 failures; most common (2x): quota exceeded",
		},
		{
			name:       "occasional failures are informational",
			jobs:       []generated.ReviewJob{job("gemini", "auth expired", recent)},
			wantStatus: doctorInfo,
			wantFirst:  "gemini: 1 failure; most common (1x): auth expired",
		},
		{
			name:       "only old failures",
			jobs:       []generated.ReviewJob{job("codex", "boom", old)},
			wantStatus: doctorOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := &doctorEnv{
				ctx:    t.Context(),
				now:    now,
				ping:   &daemon.PingInfo{OK: true},
				daemon: fakeDoctorDaemon{ping: &daemon.PingInfo{OK: true}, jobs: tt.jobs},
			}
			got := findDoctorCheck(t, checkDoctorFailedJobs(env), "jobs.failed")
			assert.Equal(t, tt.wantStatus, got.Status)
			if tt.wantFirst != "" {
				require.NotEmpty(t, got.Details)
				assert.Equal(t, tt.wantFirst, got.Details[0])
			}
		})
	}
}

func TestDoctorEnqueueFailures(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	ts := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	entry := func(ago time.Duration, repo, outcome, msg string) string {
		data, err := json.Marshal(postCommitLogEntry{TS: ts(ago), Repo: repo, Outcome: outcome, Message: msg})
		require.NoError(t, err)
		return string(data) + "\n"
	}

	tests := []struct {
		name       string
		log        string
		wantStatus doctorStatus
		wantDetail string
	}{
		{
			name: "latest commit in this repo was not queued",
			log: entry(10*24*time.Hour, "/repo", "fail", "old failure") +
				entry(2*time.Hour, "/repo", "ok", "enqueued job 1") +
				"not json\n" +
				entry(time.Hour, "/repo", "fail", "daemon not running"),
			wantStatus: doctorWarn,
			wantDetail: "the most recent commit in this repository was not queued",
		},
		{
			name: "one failure elsewhere is informational",
			log: entry(time.Hour, "/other", "fail", "daemon not running") +
				entry(time.Minute, "/repo", "ok", "enqueued job 2"),
			wantStatus: doctorInfo,
			wantDetail: "most common (1x): daemon not running",
		},
		{
			name:       "no recent failures",
			log:        entry(time.Hour, "/repo", "ok", "enqueued job 3"),
			wantStatus: doctorOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "post-commit.log")
			require.NoError(t, os.WriteFile(path, []byte(tt.log), 0o600))
			env := &doctorEnv{now: now, repoPath: "/repo", postCommitLog: path}
			got := findDoctorCheck(t, checkDoctorEnqueueFailures(env), "jobs.enqueue_failures")
			assert.Equal(t, tt.wantStatus, got.Status)
			if tt.wantDetail != "" {
				assert.Contains(t, got.Details, tt.wantDetail)
			}
		})
	}
}

func TestDoctorCommandJSONFailsOnBrokenConfig(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("default_agent = [\n"), 0o600))

	// Point at a port nothing listens on so the doctor never reaches a real
	// daemon.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	patchServerAddr(t, addr)

	cmd := doctorCmd()
	cmd.SilenceUsage = true // the root command sets this in production
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json", "--repo", t.TempDir()})
	err = cmd.Execute()

	exitErr, ok := errors.AsType[*exitError](err)
	require.True(t, ok, "want exitError, got %v", err)
	assert.Equal(t, 1, exitErr.code)

	var report doctorReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	assert.Equal(t, doctorFail, findDoctorCheck(t, report.Checks, "config.global").Status)
	assert.Equal(t, doctorWarn, findDoctorCheck(t, report.Checks, "daemon.running").Status)
	assert.Positive(t, report.Summary.Fail)
}

func TestLiveDoctorDaemonFailedJobsPagesToCutoff(t *testing.T) {
	cutoff := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	recent := cutoff.Add(time.Hour)
	jobs := func(n int, at time.Time) []storage.ReviewJob {
		out := make([]storage.ReviewJob, n)
		for i := range out {
			out[i] = storage.ReviewJob{Agent: "codex", EnqueuedAt: at}
		}
		return out
	}
	pages := map[string]map[string]any{
		"":   {"jobs": jobs(doctorFailedJobPage, recent), "has_more": true, "next_cursor": "c1"},
		"c1": {"jobs": append(jobs(50, recent), jobs(1, cutoff.Add(-time.Hour))...), "has_more": true, "next_cursor": "c2"},
	}
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		requested = append(requested, cursor)
		assert.Equal(t, "true", r.URL.Query().Get("include_panel_members"), "failed panel members must be listed")
		page, ok := pages[cursor]
		if !ok {
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, page))
	}))
	t.Cleanup(srv.Close)

	got, err := newDoctorDaemon(mustParseEndpoint(t, srv.URL)).FailedJobs(t.Context(), cutoff)
	require.NoError(t, err)
	assert.Len(t, got, doctorFailedJobPage+50)
	assert.Equal(t, []string{"", "c1"}, requested, "paging stops at the first job older than the cutoff")
}

func TestDoctorSnapshotDirSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs extra privileges on Windows")
	}
	repo := testutil.NewTestRepo(t)
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(repo.Root, ".roborev")))

	env := &doctorEnv{repoPath: repo.Root}
	got := findDoctorCheck(t, checkDoctorSnapshotDir(env), "repo.snapshot_dir")
	assert.Equal(t, doctorFail, got.Status)
	require.Len(t, got.Details, 1)
	assert.Contains(t, got.Details[0], "must not contain symlinks")
}

func TestDoctorDefaultBranchConfig(t *testing.T) {
	repo := testutil.NewTestRepo(t)
	repo.CommitFile(".roborev.toml", "agent = [\n", "broken config")
	branch := strings.TrimSpace(repo.Run("branch", "--show-current"))

	env := &doctorEnv{repoPath: repo.Root, repoBranch: branch}
	got, ok := checkDoctorDefaultBranchConfig(env)
	require.True(t, ok)
	assert.Equal(t, doctorFail, got.Status)
	assert.Equal(t, ".roborev.toml on "+branch+" is invalid", got.Summary)

	env.repoBranch = "no-such-branch"
	got, ok = checkDoctorDefaultBranchConfig(env)
	require.True(t, ok)
	assert.Equal(t, doctorWarn, got.Status)
}

func TestDoctorReviewPanels(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	okPanel := daemon.DoctorPanel{
		Name: "guard", UsedFor: []string{"post_commit", "manual"},
		Members:   []daemon.DoctorPanelMember{{Name: "sec", Agent: "claude-code"}},
		Synthesis: agent.Diagnosis{Name: "claude-code", Available: true},
	}

	t.Run("a working panel for every review replaces the single-agent check", func(t *testing.T) {
		env := &doctorEnv{
			ctx:          t.Context(),
			global:       &config.Config{DefaultAgent: "codex"},
			daemonAgents: &doctorDaemonAgents{Panels: []daemon.DoctorPanel{okPanel}},
		}
		checks, _ := checkDoctorReviewAgents(env)
		require.Len(t, checks, 1)
		assert.Equal(t, "agents.review_panel", checks[0].ID)
		assert.Equal(t, doctorOK, checks[0].Status)
	})

	t.Run("the single agent still covers reviews without a panel", func(t *testing.T) {
		post := okPanel
		post.UsedFor = []string{"post_commit"}
		env := &doctorEnv{
			ctx:          t.Context(),
			global:       &config.Config{DefaultAgent: "codex"},
			daemonAgents: &doctorDaemonAgents{Panels: []daemon.DoctorPanel{post}},
		}
		checks, _ := checkDoctorReviewAgents(env)
		single := findDoctorCheck(t, checks, "agents.review")
		assert.Equal(t, doctorFail, single.Status)
		assert.Equal(t, "manual reviews will fail: agent codex is not available", single.Summary)
	})

	t.Run("a member without an agent fails the panel", func(t *testing.T) {
		broken := okPanel
		broken.Members = []daemon.DoctorPanelMember{
			{Name: "sec", Agent: "claude-code"},
			{Name: "style", Error: `agent "gemini" unavailable`},
		}
		env := &doctorEnv{
			ctx:          t.Context(),
			global:       &config.Config{},
			daemonAgents: &doctorDaemonAgents{Panels: []daemon.DoctorPanel{broken}},
		}
		checks, _ := checkDoctorReviewAgents(env)
		require.Len(t, checks, 1)
		assert.Equal(t, doctorFail, checks[0].Status)
		assert.Contains(t, checks[0].Details, `member style: agent "gemini" unavailable`)
	})
}

func TestDoctorConfiguredAgentsIncludeEnabledExperiments(t *testing.T) {
	t.Setenv("PATH", writeFakeAgentBinary(t, "claude"))
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("default_agent = \"claude-code\"\n"), 0o600))

	repo := testutil.NewTestRepo(t)
	repo.CommitFile(".roborev.toml", `
[experiments.try-gemini]
enabled = true
ratio = 0.5
workflows = ["review"]

[experiments.try-gemini.config]
review_agent = "gemini"
`, "add experiment")

	env := loadDoctorEnv(t.Context(), repo.Root, fakeDoctorDaemon{})
	require.NoError(t, env.repoErr)
	got := findDoctorCheck(t, checkDoctorConfiguredAgents(env, nil), "agents.configured")
	assert.Equal(t, doctorWarn, got.Status)
	require.Len(t, got.Details, 1)
	assert.Contains(t, got.Details[0], "gemini (set by experiments.try-gemini: review_agent)")
}
