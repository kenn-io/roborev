package daemon

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/internal/storage"
)

func TestKataCreateRequestFailed(t *testing.T) {
	hook := config.HookConfig{Type: "kata"}
	event := Event{Type: "review.failed", JobID: 7, RepoName: "myrepo", SHA: "abc123def456", Agent: "codex", Error: "boom", TS: time.Unix(1700000000, 0).UTC()}

	req, ok := kataCreateRequest(hook, event)
	require.True(t, ok)
	assert.Contains(t, req.Title, "Review failed for myrepo")
	assert.Contains(t, req.Title, "roborev show 7")
	require.NotNil(t, req.Priority)
	assert.Equal(t, 1, *req.Priority)
	assert.Contains(t, req.Labels, "roborev")
	assert.Contains(t, req.Labels, "review-failed")
	assert.Equal(t, "roborev:7:review.failed:abc123def456", req.IdempotencyKey)
	assert.Contains(t, req.Body, "boom")
	assert.Contains(t, req.Body, "roborev show 7")
}

func TestKataCreateRequestPrefersUUID(t *testing.T) {
	hook := config.HookConfig{Type: "kata"}
	jobUUID := testUUID("uuid-abc")
	event := Event{Type: "review.failed", JobID: 7, JobUUID: &jobUUID, SHA: "deadbeef"}
	req, ok := kataCreateRequest(hook, event)
	require.True(t, ok)
	assert.Equal(t, "roborev:job:"+jobUUID.String()+":review.failed", req.IdempotencyKey) //nolint:forbidigo // Kata idempotency key text boundary.
}

func TestKataCreateRequestFindings(t *testing.T) {
	p := 4
	hook := config.HookConfig{Type: "kata", Project: "proj", Labels: []string{"team"}, Priority: &p}
	event := Event{Type: "review.completed", Verdict: "F", JobID: 9, RepoName: "r", SHA: "deadbeef", Findings: "## Bug\nbad"}

	req, ok := kataCreateRequest(hook, event)
	require.True(t, ok)
	assert.Equal(t, "proj", req.Project)
	assert.Equal(t, 4, *req.Priority) // configured priority wins
	assert.Contains(t, req.Labels, "team")
	assert.Contains(t, req.Labels, "review-finding")
	assert.Contains(t, req.Body, "roborev fix 9")
	assert.Contains(t, req.Body, "bad")
	assert.Contains(t, req.Body, "\n\nFix:")
}

func TestKataCreateRequestPassingSkipped(t *testing.T) {
	_, ok := kataCreateRequest(config.HookConfig{Type: "kata"}, Event{Type: "review.completed", Verdict: "P", JobID: 1})
	assert.False(t, ok)
}

func TestKataHookBodyTruncated(t *testing.T) {
	event := Event{Type: "review.completed", Verdict: "F", JobID: 2, SHA: "s", Findings: strings.Repeat("x", kataHookMaxBodyBytes*2)}
	req, ok := kataCreateRequest(config.HookConfig{Type: "kata"}, event)
	require.True(t, ok)
	assert.LessOrEqual(t, len(req.Body), kataHookMaxBodyBytes+64)
	assert.Contains(t, req.Body, "truncated")
}

func TestRunKataHookCreatesAndLogs(t *testing.T) {
	var buf strings.Builder
	fake := &katatest.FakeClient{CreateResult: kata.CreateResult{ShortID: "new1"}}
	hr := &HookRunner{
		logger:        log.New(&buf, "", 0),
		newKataClient: func(string) kata.Client { return fake },
	}
	hr.wg.Add(1)
	hr.runKataHook(config.HookConfig{Type: "kata", Project: "p"}, Event{Type: "review.failed", JobID: 5, SHA: "x"}, t.TempDir())

	require.Len(t, fake.CreateReqs, 1)
	assert.Contains(t, buf.String(), "created issue new1")
}

func TestLegacyKataHookEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, source, role, ciBase, jobType string
		eligible, suppressed                bool
	}{
		{"standalone", "", "", "", storage.JobTypeReview, true, false},
		{"synthesis", "", storage.PanelRoleSynthesis, "", storage.JobTypeSynthesis, true, false},
		{"ci source", storage.JobSourceCI, "", "", storage.JobTypeReview, false, false},
		{"ci base", "", "", "main", storage.JobTypeReview, false, false},
		{"ci synthesis", storage.JobSourceCI, storage.PanelRoleSynthesis, "", storage.JobTypeSynthesis, false, false},
		{"panel member", "", storage.PanelRoleMember, "", storage.JobTypeReview, false, false},
		{"rebuttal", storage.JobSourceRebuttal, "", "", storage.JobTypeReview, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newWorkerTestContext(t, 1)
			job := env.createAndClaimJob(t, "legacy-"+tc.name, "worker")
			_, err := env.DB.ExecContext(t.Context(), `UPDATE review_jobs SET source = ?, panel_role = ?, ci_base_branch = ?, job_type = ? WHERE id = ?`, tc.source, tc.role, tc.ciBase, tc.jobType, job.ID)
			require.NoError(t, err)
			job, err = env.DB.GetJobByIDContext(t.Context(), job.ID)
			require.NoError(t, err)
			for _, apiEnabled := range []bool{false, true} {
				for _, eventType := range []string{"review.completed", "review.failed"} {
					t.Run(fmt.Sprintf("api=%t/%s", apiEnabled, eventType), func(t *testing.T) {
						var webhooks atomic.Int32
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							webhooks.Add(1)
							w.WriteHeader(http.StatusNoContent)
						}))
						t.Cleanup(server.Close)
						cfg := config.DefaultConfig()
						cfg.Integrations.Kata.Enabled = apiEnabled
						cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Project: "reviews"}, {Type: "webhook", Event: "review.*", URL: server.URL}}
						fake := &katatest.FakeClient{CreateResult: kata.CreateResult{ShortID: "new1"}}
						runner := NewHookRunner(NewStaticConfig(cfg), env.Broadcaster, log.New(io.Discard, "", 0), env.DB)
						t.Cleanup(runner.Stop)
						runner.newKataClient = func(string) kata.Client { return fake }
						event := eventForJob(eventType, job, job.ID)
						event.Verdict, event.Findings, event.Error = "F", "finding", "failed"
						if tc.suppressed {
							event.Verdict = "" // The worker clears rebuttal verdicts.
						}
						assert.Equal(t, tc.suppressed, event.SuppressHooks)
						env.Broadcaster.Broadcast(event)
						runner.WaitUntilIdle()
						if tc.eligible {
							require.Len(t, fake.CreateReqs, 1)
							if eventType == "review.completed" {
								assert.Contains(t, fake.CreateReqs[0].Body, "finding")
							} else {
								assert.Contains(t, fake.CreateReqs[0].Body, "failed")
							}
						} else {
							assert.Empty(t, fake.CreateReqs)
						}
						if tc.suppressed {
							assert.Zero(t, webhooks.Load())
						} else {
							assert.EqualValues(t, 1, webhooks.Load(), "other hook types still run")
						}
					})
				}
			}
		})
	}
}

func TestRunKataHookLogsNoBinding(t *testing.T) {
	var buf strings.Builder
	fake := &katatest.FakeClient{BindingErr: kata.ErrNoBinding}
	hr := &HookRunner{
		logger:        log.New(&buf, "", 0),
		newKataClient: func(string) kata.Client { return fake },
	}
	hr.wg.Add(1)
	hr.runKataHook(config.HookConfig{Type: "kata"}, Event{Type: "review.failed", JobID: 5, SHA: "x"}, t.TempDir())

	assert.Empty(t, fake.CreateReqs, "must not create without a binding")
	assert.Contains(t, buf.String(), "no .kata.toml binding")
}

// kataOnPathMarker puts a kata executable first on PATH that records every
// run, so a test can prove the daemon never starts it.
func kataOnPathMarker(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the kata executable")
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "kata-was-run")
	script := "#!/bin/sh\necho \"$@\" >> " + marker + "\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "kata"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func kataConnectionConfig(fake *katafake.Server) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{
		Enabled: true, Endpoint: fake.URL(), APIKey: "synthetic-kata-credential",
		DefaultProject: "reviews", Teammate: "roborev",
	}
	return cfg
}

func newKataHookRunner(t *testing.T, cfg *config.Config, logger *log.Logger) *HookRunner {
	t.Helper()
	hr := NewHookRunner(NewStaticConfig(cfg), NewBroadcaster(), logger)
	t.Cleanup(hr.Stop)
	return hr
}

func TestKataHookFilesThroughTheConfiguredConnection(t *testing.T) {
	marker := kataOnPathMarker(t)
	fake := katafake.New(t)
	fake.Principal = "kata-service"
	var buf strings.Builder
	hr := newKataHookRunner(t, kataConnectionConfig(fake), log.New(&buf, "", 0))

	event := Event{Type: "review.failed", JobID: 5, SHA: "abc123", RepoName: "repo", Error: "boom"}
	hr.wg.Add(1)
	hr.runKataHook(config.HookConfig{Type: "kata"}, event, t.TempDir())

	var posts []katafake.Request
	for _, r := range fake.Requests() {
		if r.Method == http.MethodPost {
			posts = append(posts, r)
		}
	}
	require.Len(t, posts, 1, "log: %s", buf.String())
	assert.Equal(t, "/api/v1/projects/7/issues", posts[0].Path, "an unbound repository files into default_project")
	body := posts[0].JSON(t)
	assert.Equal(t, "roborev", body["actor"])
	assert.Contains(t, body["title"], "Review failed for repo")
	assert.Equal(t, "Bearer synthetic-kata-credential", posts[0].Header.Get("Authorization"))
	assert.NotEmpty(t, posts[0].Header.Get("Idempotency-Key"))
	assert.Contains(t, buf.String(), "created issue")
	assert.Contains(t, buf.String(), "as kata-service", "the attribution Kata returned is logged")
	assert.NotContains(t, buf.String(), "synthetic-kata-credential")
	assert.NoFileExists(t, marker, "the kata CLI must not run when the connection is enabled")
}

func TestKataHookProjectPrecedence(t *testing.T) {
	bound := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bound, ".kata.toml"), []byte("version = 1\n[project]\nname = \"other\"\n"), 0o600))
	tests := []struct {
		name, workdir, hookProject, wantPath string
	}{
		{"hook project wins over a binding", bound, "reviews", "/api/v1/projects/7/issues"},
		{"binding wins over the default project", bound, "", "/api/v1/projects/8/issues"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := katafake.New(t)
			// A committed binding resolves through the server's alias
			// registry; this workspace is bound to "other".
			fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/v1/projects/resolve" {
					return false
				}
				canonical := fake.Projects[1]
				fake.Reply(w, gen.ProjectResolveBody{Project: canonical, Alias: gen.ProjectAlias{
					AliasIdentity: "local://" + tt.workdir, AliasKind: "local", ID: 1, ProjectID: canonical.ID, CreatedAt: time.Unix(0, 0).UTC(),
				}})
				return true
			}
			hr := newKataHookRunner(t, kataConnectionConfig(fake), log.New(io.Discard, "", 0))
			hr.wg.Add(1)
			hr.runKataHook(config.HookConfig{Type: "kata", Project: tt.hookProject}, Event{Type: "review.failed", JobID: 5, SHA: "x"}, tt.workdir)
			var posts []string
			for _, r := range fake.Requests() {
				if r.Method == http.MethodPost {
					posts = append(posts, r.Path)
				}
			}
			require.NotEmpty(t, posts)
			assert.Equal(t, tt.wantPath, posts[len(posts)-1], "the issue files into the selected project")
			for _, p := range posts[:len(posts)-1] {
				assert.Equal(t, "/api/v1/projects/resolve", p, "a committed binding resolves through the server's alias registry")
			}
		})
	}
}

func TestKataHookWithABrokenConnectionFailsVisiblyAndNeverShellsOut(t *testing.T) {
	marker := kataOnPathMarker(t)
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.com", APIKeyFile: filepath.Join(t.TempDir(), "missing"), Teammate: "roborev"}
	var buf strings.Builder
	hr := newKataHookRunner(t, cfg, log.New(&buf, "", 0))
	hr.wg.Add(1)
	hr.runKataHook(config.HookConfig{Type: "kata", Project: "p"}, Event{Type: "review.failed", JobID: 5, SHA: "x"}, t.TempDir())
	assert.Contains(t, buf.String(), "kata hook error")
	assert.Contains(t, buf.String(), "api_key_file")
	assert.NoFileExists(t, marker)
}

func TestKataHookKeepsTheCLIWhenTheConnectionIsOff(t *testing.T) {
	hr := newKataHookRunner(t, config.DefaultConfig(), nil)
	_, isCLI := hr.newKataClient(t.TempDir()).(*kata.CLIClient)
	assert.True(t, isCLI)
	hr = newKataHookRunner(t, nil, nil)
	_, isCLI = hr.newKataClient(t.TempDir()).(*kata.CLIClient)
	assert.True(t, isCLI, "a runner without a config source behaves as before")
}
