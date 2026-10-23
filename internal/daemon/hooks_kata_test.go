package daemon

import (
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/kata/katatest"
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
	require.NoError(t, os.WriteFile(filepath.Join(bound, ".kata.toml"), []byte("[project]\nname = \"other\"\n"), 0o600))
	tests := []struct {
		name, workdir, hookProject, wantPath string
	}{
		{"hook project wins over a binding", bound, "reviews", "/api/v1/projects/7/issues"},
		{"binding wins over the default project", bound, "", "/api/v1/projects/8/issues"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := katafake.New(t)
			hr := newKataHookRunner(t, kataConnectionConfig(fake), log.New(io.Discard, "", 0))
			hr.wg.Add(1)
			hr.runKataHook(config.HookConfig{Type: "kata", Project: tt.hookProject}, Event{Type: "review.failed", JobID: 5, SHA: "x"}, tt.workdir)
			var paths []string
			for _, r := range fake.Requests() {
				if r.Method == http.MethodPost {
					paths = append(paths, r.Path)
				}
			}
			assert.Equal(t, []string{tt.wantPath}, paths)
		})
	}
}

func TestKataHookWithABrokenConnectionFailsVisiblyAndNeverShellsOut(t *testing.T) {
	marker := kataOnPathMarker(t)
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "ftp://kata.example.com", Teammate: "roborev"}
	var buf strings.Builder
	hr := newKataHookRunner(t, cfg, log.New(&buf, "", 0))
	hr.wg.Add(1)
	hr.runKataHook(config.HookConfig{Type: "kata", Project: "p"}, Event{Type: "review.failed", JobID: 5, SHA: "x"}, t.TempDir())
	assert.Contains(t, buf.String(), "kata hook error")
	assert.Contains(t, buf.String(), "endpoint")
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
