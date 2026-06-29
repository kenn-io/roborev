# CI Discord Webhook Notifications Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a hot-reloaded global `ci.discord_webhook_url` setting that sends best-effort Discord notifications for CI job failures, with per-agent dedupe for quota/cooldown bursts.

**Architecture:** Keep notification ownership inside the existing CI poller `review.failed` handler. Load the failed job from storage to apply `ReviewJob.IsCIReview()` and enrich the Discord embed. Put Discord payload, classification, posting, and dedupe helpers in a focused daemon file, reusing existing review classification constants/helpers and webhook redaction helpers.

**Tech Stack:** Go stdlib (`net/http`, `encoding/json`, `time`), existing `internal/config`, `internal/daemon`, `internal/review`, `internal/storage`, `testify`, docs Markdown.

---

## File Structure

- `internal/config/ci.go`: add `CIConfig.DiscordWebhookURL` with TOML and sensitive tags.
- `internal/config/config_test.go`: add parse coverage for `ci.discord_webhook_url`.
- `internal/config/keyval_test.go`: add sensitive-key coverage for `ci.discord_webhook_url`.
- `internal/daemon/worker.go`: extract `agentTimeoutErrorPrefix` constant and use it in timeout formatting.
- `internal/daemon/ci_discord.go`: new focused helper file for Discord payloads, failure classification, HTTP delivery, CI notification orchestration, and quota/cooldown dedupe.
- `internal/daemon/ci_discord_test.go`: new focused unit tests for payload classification and HTTP delivery.
- `internal/daemon/ci_poller.go`: add dedupe state/test clock fields to `CIPoller` and call the Discord notification path from `handleReviewFailed`.
- `internal/daemon/ci_poller_test.go`: add integration-style poller tests for hot-reloaded URL behavior, CI filtering, posting, and quota/cooldown dedupe.
- `docs/integrations/github.md`: document the new CI option and failure notification behavior.
- `docs/configuration.md`: correct the stale hot-reload note so `[ci]` is not listed as restart-required.

## Design Constraints

- Read `cfgGetter.Config().CI.DiscordWebhookURL` during each `review.failed` handling call, never at poller startup.
- Call the Discord notification path from the existing CI poller event flow. Do not add a second event subscription or long-lived notifier goroutine.
- Route CI panel posting before Discord notification so Discord delays cannot affect the current PR posting action.
- Load the job with `db.GetJobByID` before filtering because `Event` lacks Source, ReviewType, RetryCount, and Panel fields.
- Use `ReviewJob.IsCIReview()` for the CI predicate.
- Use a `review.ReviewResult{Status: string(job.Status), Error: errorText}` adapter when calling `review.IsQuotaFailure`, `review.IsTransientFailure`, and `review.IsGenuineFailure`.
- Extract and reuse `agentTimeoutErrorPrefix` for prefixless worker timeouts. If that message format changes, the timeout classifier and worker should change together.
- Dedupe quota/cooldown notifications globally per canonical agent. This intentionally suppresses agent-X quota messages from all repos while the daemon-wide agent-X cooldown window is active; the first message carries one representative failed job.
- Keep the dedupe map touched only from the existing CI poller event goroutine and direct sequential tests. Add a comment documenting that no mutex is needed under that ownership; add a mutex instead if future code calls it concurrently.
- Trim Discord fields for length, but do not claim path sanitization of raw error text. The error string is free-form agent/provider output and is only length-bounded.

---

### Task 1: Add CI Config Key

**Files:**
- Modify: `internal/config/ci.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/config/keyval_test.go`

- [ ] **Step 1: Write the failing global config parse test**

Add a test near `TestCIConfigNewFields` in `internal/config/config_test.go`:

```go
func TestCIConfigDiscordWebhookURL(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	err := os.WriteFile(configPath, []byte(`
[ci]
enabled = true
discord_webhook_url = "https://discord.com/api/webhooks/123/token"
`), 0o644)
	require.NoError(t, err)

	cfg, err := LoadGlobalFrom(configPath)
	require.NoError(t, err)
	assert.Equal(t,
		"https://discord.com/api/webhooks/123/token",
		cfg.CI.DiscordWebhookURL,
	)
}
```

- [ ] **Step 2: Write the failing sensitive-key test**

Update `TestIsSensitiveKey` in `internal/config/keyval_test.go`:

```go
assert.True(IsSensitiveKey("ci.discord_webhook_url"))
```

- [ ] **Step 3: Run tests to verify RED**

Run:

```bash
go test ./internal/config -run 'TestCIConfigDiscordWebhookURL|TestIsSensitiveKey'
```

Expected: FAIL because `CIConfig.DiscordWebhookURL` does not exist or is not sensitive yet.

- [ ] **Step 4: Implement the config field**

Add the field in `internal/config/ci.go` near other CI notification/comment options:

```go
// DiscordWebhookURL posts best-effort Discord notifications for CI job failures.
// Empty disables Discord notifications.
DiscordWebhookURL string `toml:"discord_webhook_url" sensitive:"true"`
```

- [ ] **Step 5: Run tests to verify GREEN**

Run:

```bash
go test ./internal/config -run 'TestCIConfigDiscordWebhookURL|TestIsSensitiveKey'
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/ci.go internal/config/config_test.go internal/config/keyval_test.go
git commit -m "Add CI Discord webhook config"
```

---

### Task 2: Add Discord Payload And Classification Helpers

**Files:**
- Create: `internal/daemon/ci_discord.go`
- Create: `internal/daemon/ci_discord_test.go`
- Modify: `internal/daemon/worker.go`

- [ ] **Step 1: Write failing payload and classification tests**

Create `internal/daemon/ci_discord_test.go`:

```go
package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

func TestDiscordFailureClass(t *testing.T) {
	tests := []struct {
		name string
		err  string
		want string
	}{
		{
			name: "quota cooldown",
			err:  review.QuotaErrorPrefix + "agent codex quota cooldown active",
			want: "quota/cooldown",
		},
		{
			name: "provider outage",
			err:  review.OutageErrorPrefix + "429 too many requests",
			want: "provider/session outage",
		},
		{
			name: "agent timeout",
			err:  agentTimeoutErrorPrefix + " 30m0s",
			want: "timeout",
		},
		{
			name: "generic",
			err:  "agent: model not found",
			want: "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := storage.ReviewJob{
				Status: storage.JobStatusFailed,
				Error:  tt.err,
			}
			assert.Equal(t, tt.want, discordFailureClass(job, ""))
		})
	}
}

func TestBuildDiscordCIJobFailedPayloadIncludesContext(t *testing.T) {
	job := storage.ReviewJob{
		ID:              42,
		RepoName:        "api",
		GitRef:          "base123..abcdef1234567890",
		CIBaseBranch:    "main",
		Agent:           "codex",
		ReviewType:      "security",
		Status:          storage.JobStatusFailed,
		Error:           review.QuotaErrorPrefix + "agent codex quota cooldown active",
		RetryCount:      2,
		PanelRole:       storage.PanelRoleMember,
		PanelName:       "ci",
		PanelMemberName: "security-codex",
	}

	payload := buildDiscordCIJobFailedPayload(Event{}, job)

	if assert.Len(t, payload.Embeds, 1) {
		embed := payload.Embeds[0]
		assert.Equal(t, "roborev CI job failed", embed.Title)
		fields := discordEmbedFieldsByName(embed.Fields)
		assert.Equal(t, "api", fields["Repository"])
		assert.Contains(t, fields["Job"], "42")
		assert.Contains(t, fields["Job"], "member")
		assert.Contains(t, fields["Job"], "security-codex")
		assert.Equal(t, "codex", fields["Agent"])
		assert.Equal(t, "main", fields["Branch"])
		assert.Equal(t, "quota/cooldown", fields["Failure"])
		assert.Equal(t, "2", fields["Retry count"])
		assert.Contains(t, fields["Error"], "quota cooldown active")
		assert.Contains(t, fields["Ref"], "abcdef1")
	}
}
```

Add this local test helper in the same file:

```go
func discordEmbedFieldsByName(fields []discordEmbedField) map[string]string {
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		out[f.Name] = f.Value
	}
	return out
}
```

- [ ] **Step 2: Run tests to verify RED**

Run:

```bash
go test ./internal/daemon -run 'TestDiscordFailureClass|TestBuildDiscordCIJobFailedPayloadIncludesContext'
```

Expected: FAIL because the Discord helper types/functions and timeout prefix constant do not exist.

- [ ] **Step 3: Extract the timeout prefix constant**

In `internal/daemon/worker.go`, add a package-level constant near worker failure helpers:

```go
const agentTimeoutErrorPrefix = "agent timeout after"
```

Change the timeout formatting in `processJob` from:

```go
timeoutErr := fmt.Sprintf(
	"agent timeout after %s",
	timeoutDuration.Round(time.Second),
)
```

to:

```go
timeoutErr := fmt.Sprintf(
	"%s %s",
	agentTimeoutErrorPrefix,
	timeoutDuration.Round(time.Second),
)
```

- [ ] **Step 4: Implement payload and classification helpers**

Create `internal/daemon/ci_discord.go` with:

```go
package daemon

import (
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	gitpkg "go.kenn.io/roborev/internal/git"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

const (
	discordFailureQuotaCooldown = "quota/cooldown"
	discordFailureOutage        = "provider/session outage"
	discordFailureTimeout       = "timeout"
	discordFailureError         = "error"
	discordFieldLimit           = 900
)

type discordWebhookPayload struct {
	Content string         `json:"content,omitempty"`
	Embeds  []discordEmbed `json:"embeds,omitempty"`
}

type discordEmbed struct {
	Title  string              `json:"title,omitempty"`
	Color  int                 `json:"color,omitempty"`
	Fields []discordEmbedField `json:"fields,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

func buildDiscordCIJobFailedPayload(event Event, job storage.ReviewJob) discordWebhookPayload {
	agentName := firstNonEmpty(event.Agent, job.Agent)
	errorText := firstNonEmpty(job.Error, event.Error)

	fields := []discordEmbedField{
		{Name: "Repository", Value: nonEmpty(firstNonEmpty(event.RepoName, job.RepoName), "unknown"), Inline: true},
		{Name: "Job", Value: discordJobSummary(job), Inline: true},
		{Name: "Agent", Value: nonEmpty(agentName, "unknown"), Inline: true},
		{Name: "Ref", Value: discordRefSummary(job, event), Inline: true},
		{Name: "Branch", Value: nonEmpty(job.HookBranch(), "unknown"), Inline: true},
		{Name: "Failure", Value: discordFailureClass(job, event.Error), Inline: true},
		{Name: "Retry count", Value: strconv.Itoa(job.RetryCount), Inline: true},
		{Name: "Error", Value: trimDiscordField(errorText), Inline: false},
	}

	return discordWebhookPayload{
		Embeds: []discordEmbed{{
			Title:  "roborev CI job failed",
			Color:  0xD73A49,
			Fields: fields,
		}},
	}
}

func discordFailureClass(job storage.ReviewJob, eventError string) string {
	errorText := firstNonEmpty(job.Error, eventError)
	result := reviewpkg.ReviewResult{
		Status: string(job.Status),
		Error:  errorText,
	}
	if reviewpkg.IsQuotaFailure(result) {
		return discordFailureQuotaCooldown
	}
	if reviewpkg.IsTransientFailure(result) {
		return discordFailureOutage
	}
	if strings.Contains(errorText, agentTimeoutErrorPrefix) {
		return discordFailureTimeout
	}
	if reviewpkg.IsGenuineFailure(result) {
		return discordFailureError
	}
	return discordFailureError
}

func discordJobSummary(job storage.ReviewJob) string {
	parts := []string{fmt.Sprintf("#%d", job.ID)}
	for _, p := range []string{job.PanelRole, job.PanelName, job.PanelMemberName, job.ReviewType} {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " / ")
}

func discordRefSummary(job storage.ReviewJob, event Event) string {
	ref := firstNonEmpty(event.SHA, job.GitRef)
	if ref == "" {
		return "unknown"
	}
	ref = headOf(ref)
	return gitpkg.ShortSHA(ref)
}

func trimDiscordField(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	if len(s) <= discordFieldLimit {
		return s
	}
	return reviewpkg.TrimPartialRune(s[:discordFieldLimit-3]) + "..."
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func canonicalDiscordAgent(job storage.ReviewJob, event Event) string {
	return agent.CanonicalName(firstNonEmpty(event.Agent, job.Agent))
}
```

Adjust names during implementation if the compiler reveals collisions; keep the file focused on Discord behavior.

- [ ] **Step 5: Run tests to verify GREEN**

Run:

```bash
go test ./internal/daemon -run 'TestDiscordFailureClass|TestBuildDiscordCIJobFailedPayloadIncludesContext'
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/worker.go internal/daemon/ci_discord.go internal/daemon/ci_discord_test.go
git commit -m "Build CI Discord failure payloads"
```

---

### Task 3: Add Discord Webhook HTTP Delivery

**Files:**
- Modify: `internal/daemon/ci_discord.go`
- Modify: `internal/daemon/ci_discord_test.go`

- [ ] **Step 1: Write failing HTTP delivery tests**

Append tests to `internal/daemon/ci_discord_test.go`:

```go
func TestPostDiscordWebhookPostsJSON(t *testing.T) {
	type request struct {
		contentType string
		payload     discordWebhookPayload
	}
	reqCh := make(chan request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload discordWebhookPayload
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		reqCh <- request{contentType: r.Header.Get("Content-Type"), payload: payload}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	payload := discordWebhookPayload{Embeds: []discordEmbed{{Title: "roborev CI job failed"}}}

	var logs []string
	ok := postDiscordWebhook(context.Background(), server.URL, payload, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	assert.True(t, ok)
	assert.Empty(t, logs)
	select {
	case got := <-reqCh:
		assert.Equal(t, "application/json", got.contentType)
		require.Len(t, got.payload.Embeds, 1)
		assert.Equal(t, "roborev CI job failed", got.payload.Embeds[0].Title)
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for Discord webhook request")
	}
}

func TestPostDiscordWebhookRedactsURLInLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()

	webhookURL, err := neturl.Parse(server.URL)
	require.NoError(t, err)
	webhookURL.User = neturl.UserPassword("token", "secret")
	webhookURL.Path = "/api/webhooks/123456/sensitive-token"
	webhookURL.RawQuery = "api_key=12345"
	webhookURL.Fragment = "frag"

	var logs []string
	ok := postDiscordWebhook(context.Background(), webhookURL.String(), discordWebhookPayload{}, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	assert.False(t, ok)
	require.NotEmpty(t, logs)
	logOutput := strings.Join(logs, "\n")
	assert.Contains(t, logOutput, "502 Bad Gateway")
	assert.Contains(t, logOutput, "/...")
	assert.NotContains(t, logOutput, "token")
	assert.NotContains(t, logOutput, "secret")
	assert.NotContains(t, logOutput, "api_key")
	assert.NotContains(t, logOutput, "12345")
	assert.NotContains(t, logOutput, "frag")
	assert.NotContains(t, logOutput, "sensitive-token")
}
```

Add imports as needed: `context`, `encoding/json`, `fmt`, `net/http`, `net/http/httptest`, `strings`, `time`, and `net/url` aliased as `neturl`.

- [ ] **Step 2: Run tests to verify RED**

Run:

```bash
go test ./internal/daemon -run 'TestPostDiscordWebhookPostsJSON|TestPostDiscordWebhookRedactsURLInLogs'
```

Expected: FAIL because `postDiscordWebhook` does not exist.

- [ ] **Step 3: Implement webhook delivery**

Add to `internal/daemon/ci_discord.go`:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type discordLogf func(format string, args ...any)

func postDiscordWebhook(ctx context.Context, webhookURL string, payload discordWebhookPayload, logf discordLogf) bool {
	safeURL := redactWebhookURL(webhookURL)

	body, err := json.Marshal(payload)
	if err != nil {
		logf("Discord webhook error (url=%q): marshal payload: %v", safeURL, err)
		return false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		logf("Discord webhook error (url=%q): build request: %v", safeURL, redactURLError(err))
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logf("Discord webhook error (url=%q): %v", safeURL, redactURLError(err))
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return true
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if len(respBody) > 0 {
		logf("Discord webhook error (url=%q): status %s: %s", safeURL, resp.Status, strings.TrimSpace(string(respBody)))
		return false
	}
	logf("Discord webhook error (url=%q): status %s", safeURL, resp.Status)
	return false
}
```

Do not duplicate `redactWebhookURL` or `redactURLError`; reuse the existing same-package helpers from `internal/daemon/hooks.go`.

- [ ] **Step 4: Run tests to verify GREEN**

Run:

```bash
go test ./internal/daemon -run 'TestPostDiscordWebhookPostsJSON|TestPostDiscordWebhookRedactsURLInLogs'
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/ci_discord.go internal/daemon/ci_discord_test.go
git commit -m "Post CI Discord failure webhooks"
```

---

### Task 4: Wire CI Poller Event Handling And Dedupe

**Files:**
- Modify: `internal/daemon/ci_poller.go`
- Modify: `internal/daemon/ci_discord.go`
- Modify: `internal/daemon/ci_poller_test.go`

- [ ] **Step 1: Write failing poller tests**

Add a mutable config getter near the CI poller tests in `internal/daemon/ci_poller_test.go`:

```go
type mutableConfigGetter struct {
	cfg *config.Config
}

func (g *mutableConfigGetter) Config() *config.Config {
	return g.cfg
}
```

Add tests:

```go
func TestCIPollerDiscordWebhookReadsURLAtEventTime(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	getter := &mutableConfigGetter{cfg: h.Cfg}
	h.Poller.cfgGetter = getter

	reqCh := make(chan discordWebhookPayload, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload discordWebhookPayload
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		reqCh <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	_, _, members := h.seedCIPanelRun(t, "acme/api", 1, "headsha111", "base..headsha111",
		[]jobSpec{{Agent: "codex", ReviewType: "security", Status: "failed", Error: "agent: failed"}})

	h.Poller.handleReviewFailed(ciEvent(members[0].ID, "review.failed"))
	assert.Empty(t, reqCh, "empty URL skips notification")

	h.Cfg.CI.DiscordWebhookURL = server.URL
	h.Poller.handleReviewFailed(ciEvent(members[0].ID, "review.failed"))
	select {
	case payload := <-reqCh:
		require.Len(t, payload.Embeds, 1)
		assert.Equal(t, "roborev CI job failed", payload.Embeds[0].Title)
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for Discord webhook request")
	}

	h.Cfg.CI.DiscordWebhookURL = ""
	h.Poller.handleReviewFailed(ciEvent(members[0].ID, "review.failed"))
	assert.Empty(t, reqCh, "cleared URL skips future notifications")
}

func TestCIPollerDiscordWebhookIgnoresNonCIJobs(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	reqCh := make(chan discordWebhookPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload discordWebhookPayload
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		reqCh <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	h.Cfg.CI.DiscordWebhookURL = server.URL

	job, err := h.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: h.Repo.ID,
		GitRef: "abc123",
		Agent:  "codex",
	})
	require.NoError(t, err)
	h.markJobFailed(t, job.ID, "agent: failed")

	h.Poller.handleReviewFailed(ciEvent(job.ID, "review.failed"))

	assert.Empty(t, reqCh)
}

func TestCIPollerDiscordWebhookDedupesQuotaCooldownPerAgent(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.AgentQuotaCooldown = "5m"
	reqCh := make(chan discordWebhookPayload, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload discordWebhookPayload
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		reqCh <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	h.Cfg.CI.DiscordWebhookURL = server.URL

	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	h.Poller.discordNowFn = func() time.Time { return now }
	quotaErr := review.QuotaErrorPrefix + "agent codex quota cooldown active"
	_, _, firstMembers := h.seedCIPanelRun(t, "acme/api", 2, "headsha222", "base..headsha222",
		[]jobSpec{{Agent: "codex", ReviewType: "security", Status: "failed", Error: quotaErr}})
	_, _, secondMembers := h.seedCIPanelRun(t, "acme/api", 3, "headsha333", "base..headsha333",
		[]jobSpec{{Agent: "codex", ReviewType: "review", Status: "failed", Error: quotaErr}})

	h.Poller.handleReviewFailed(ciEvent(firstMembers[0].ID, "review.failed"))
	h.Poller.handleReviewFailed(ciEvent(secondMembers[0].ID, "review.failed"))

	require.Len(t, reqCh, 1, "same-agent quota cooldown is deduped globally")

	now = now.Add(5*time.Minute + time.Second)
	h.Poller.handleReviewFailed(ciEvent(secondMembers[0].ID, "review.failed"))
	require.Len(t, reqCh, 2, "dedupe expires after configured quota cooldown")
}
```

Use `assert.Empty(t, reqCh)` only if it compiles for channel length in this repo's testify version; otherwise use `assert.Len(t, reqCh, 0)`.

- [ ] **Step 2: Run tests to verify RED**

Run:

```bash
go test ./internal/daemon -run 'TestCIPollerDiscordWebhookReadsURLAtEventTime|TestCIPollerDiscordWebhookIgnoresNonCIJobs|TestCIPollerDiscordWebhookDedupesQuotaCooldownPerAgent'
```

Expected: FAIL because the poller is not wired and dedupe state/test clock do not exist.

- [ ] **Step 3: Add CI poller dedupe state**

Modify `CIPoller` in `internal/daemon/ci_poller.go`:

```go
	// Discord quota dedupe is owned by the single CI event listener goroutine.
	// Add locking before calling it from concurrent goroutines.
	discordQuotaDedupe map[string]time.Time
	discordNowFn       func() time.Time
```

Initialize in `NewCIPoller`:

```go
discordQuotaDedupe: make(map[string]time.Time),
discordNowFn:       time.Now,
```

- [ ] **Step 4: Implement notification orchestration and dedupe**

Add methods in `internal/daemon/ci_discord.go`:

```go
func (p *CIPoller) notifyDiscordCIJobFailed(event Event) {
	if p == nil || p.db == nil || p.cfgGetter == nil {
		return
	}
	cfg := p.cfgGetter.Config()
	webhookURL := strings.TrimSpace(cfg.CI.DiscordWebhookURL)
	if webhookURL == "" {
		return
	}

	job, err := p.db.GetJobByID(event.JobID)
	if err != nil {
		log.Printf("CI Discord webhook: lookup job %d: %v", event.JobID, err)
		return
	}
	if job == nil || !job.IsCIReview() {
		return
	}

	failureClass := discordFailureClass(*job, event.Error)
	if failureClass == discordFailureQuotaCooldown &&
		p.suppressDiscordQuotaCooldownNotification(canonicalDiscordAgent(*job, event), cfg) {
		return
	}

	payload := buildDiscordCIJobFailedPayload(event, *job)
	postDiscordWebhook(context.Background(), webhookURL, payload, log.Printf)
}

func (p *CIPoller) suppressDiscordQuotaCooldownNotification(agentName string, cfg *config.Config) bool {
	if agentName == "" {
		agentName = "unknown"
	}
	if p.discordQuotaDedupe == nil {
		p.discordQuotaDedupe = make(map[string]time.Time)
	}
	nowFn := p.discordNowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	if until, ok := p.discordQuotaDedupe[agentName]; ok && now.Before(until) {
		return true
	}
	p.discordQuotaDedupe[agentName] = now.Add(config.ResolveAgentQuotaCooldown(cfg))
	return false
}
```

Add missing imports: `context`, `log`, `time`, and `go.kenn.io/roborev/internal/config`.

- [ ] **Step 5: Wire `handleReviewFailed`**

Modify `internal/daemon/ci_poller.go`:

```go
func (p *CIPoller) handleReviewFailed(event Event) {
	if event.Type != "review.failed" {
		return
	}
	p.routePanelEvent(event.JobID)
	p.notifyDiscordCIJobFailed(event)
}
```

Keep route-first ordering so current PR posting is not blocked by Discord delivery.

- [ ] **Step 6: Run tests to verify GREEN**

Run:

```bash
go test ./internal/daemon -run 'TestCIPollerDiscordWebhookReadsURLAtEventTime|TestCIPollerDiscordWebhookIgnoresNonCIJobs|TestCIPollerDiscordWebhookDedupesQuotaCooldownPerAgent'
```

Expected: PASS.

- [ ] **Step 7: Run existing panel posting tests**

Run:

```bash
go test ./internal/daemon -run 'Test.*Panel|TestRetrySweep|TestReconcileStuckAttempt|TestClosedPRCleansUpDeferredAttempt'
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/daemon/ci_poller.go internal/daemon/ci_discord.go internal/daemon/ci_poller_test.go
git commit -m "Notify Discord for CI job failures"
```

---

### Task 5: Document CI Discord Notifications

**Files:**
- Modify: `docs/integrations/github.md`
- Modify: `docs/configuration.md`

- [ ] **Step 1: Update GitHub CI options reference**

In `docs/integrations/github.md`, add this row to the CI options table near `include_costs`:

```markdown
| `discord_webhook_url` | string | | Discord webhook URL for best-effort CI job failure notifications |
```

- [ ] **Step 2: Add Discord notification docs**

Add a section before `## Quota Handling`:

````markdown
## Discord Failure Notifications

Set `discord_webhook_url` in the global `[ci]` config to send Discord messages when CI poller jobs fail:

```toml
[ci]
discord_webhook_url = "https://discord.com/api/webhooks/..."
```

The setting is hot-reloaded. Empty disables notifications.

Notifications are best-effort and never affect job state, CI retry state, or PR comment posting. Messages include the repository, CI base branch, job ID, panel/member context when available, agent, review type, ref, retry count, failure class, and trimmed error text. Raw error text is length-bounded but not path-sanitized, so avoid routing these notifications to public channels.

Quota/cooldown failures are deduped globally per canonical agent for the configured `agent_quota_cooldown` window. This means one `codex` quota failure can suppress additional `codex` quota messages from other repos until the cooldown window expires; the first message is the representative failure for that daemon-wide agent cooldown.
````

- [ ] **Step 3: Correct configuration hot-reload docs**

In `docs/configuration.md`, update:

```markdown
**Settings that require daemon restart:** `server_addr`, `max_workers`, the `[ci]` section, and the `[sync]` section.
```

to:

```markdown
**Settings that require daemon restart:** `server_addr`, `max_workers`, and the `[sync]` section.
```

- [ ] **Step 4: Verify docs references**

Run:

```bash
rg -n 'discord_webhook_url|Discord Failure Notifications|Settings that require daemon restart' docs/integrations/github.md docs/configuration.md
```

Expected: the new setting appears in GitHub integration docs, and the restart line no longer lists `[ci]`.

- [ ] **Step 5: Commit**

```bash
git add docs/integrations/github.md docs/configuration.md
git commit -m "Document CI Discord failure notifications"
```

---

### Task 6: Final Verification

**Files:**
- No code changes unless verification reveals a defect.

- [ ] **Step 1: Run focused package tests**

Run:

```bash
go test ./internal/config ./internal/daemon
```

Expected: PASS.

- [ ] **Step 2: Run repository tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Run lint if tests pass**

Run:

```bash
make lint-ci
```

Expected: PASS. If it reports formatting or lint issues, fix them, re-run the focused tests, then re-run `make lint-ci`.

- [ ] **Step 4: Inspect final diff and status**

Run:

```bash
git status --short
git diff --stat HEAD
```

Expected: no unstaged/uncommitted changes after the task commits. If verification required follow-up fixes, commit those fixes before handoff.
