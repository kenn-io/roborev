# Resilient CI Review Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make CI review robust to AI-provider outages — never post a terminal "Review Failed" comment; retry with backoff until the review succeeds or, after 3 days of transient failures (or a few genuine ones), post an honest non-blocking note.

**Architecture:** Add a transient/outage failure class to `ClassifyLimit`; a `ci_pr_review_attempts` table is the durable, CAS-guarded source of truth for "this (repo,pr,sha) is being handled"; a finalize-time outcome classifier decides post-vs-defer; a poller retry sweep re-enqueues fresh panel runs on an exponential→hourly backoff (3-day cap). Spec: `docs/superpowers/specs/2026-06-02-resilient-ci-review-design.md`.

**Tech Stack:** Go, SQLite (CGO, local) + PostgreSQL (`jackc/pgx/v5`, sync), `testify`, table-driven tests.

**Conventions:** After Go changes run `go fmt ./...` and `go vet ./...`, stage all changes, commit each task. Tests use `testify` (`assert`/`require`); `assert := assert.New(t)` shorthand when >3 assertions. Never `--no-verify`.

---

## File Structure

| File | Responsibility | Change |
|------|----------------|--------|
| `internal/agent/limit.go` | Error→`LimitKind` classification | Add transient rules + codex usage-limit quota rule |
| `internal/agent/limit_test.go` | Classification tests | Add captured-string table cases |
| `internal/review/result.go` | Result types + error prefixes | Add `OutageErrorPrefix`, `OutageError()` helper |
| `internal/review/failclass.go` *(new)* | Failure-class predicates over `ReviewResult` | `IsTransientFailure`, `CountTransientFailures` |
| `internal/review/synthesis.go` | Comment formatters | New give-up / soft-note formatters; transient label in existing formatters |
| `internal/review/retry.go` *(new)* | Backoff math + constants | `RetrySchedule`, `NextDelay`, `GaveUp` |
| `internal/daemon/worker.go` | Failure handling | Prepend `OutageErrorPrefix` on transient final-fail |
| `internal/storage/review_attempts.go` *(new)* | Attempts table model + CAS methods | Reserve / Defer / ClaimDueRetry / MarkDone / Delete / queries |
| `internal/storage/db.go` | SQLite schema | `CREATE TABLE IF NOT EXISTS ci_pr_review_attempts` |
| `internal/storage/postgres.go` | Postgres schema parity | Same table (no sync cursor) |
| `internal/daemon/ci_poller.go` | Poll loop, finalize, sweeps | Outcome classifier, retry sweep, closed-PR attempts cleanup, reserve-on-enqueue |

Phases are independently testable: **P1 classification**, **P2 backoff+comments**, **P3 storage**, **P4 poller integration** (depends on P1–P3).

---

## Phase 1 — Failure classification

### Task 1: Transient + codex usage-limit classification rules

**Files:**
- Modify: `internal/agent/limit.go:60` (`defaultLimitRules`)
- Test: `internal/agent/limit_test.go`

- [ ] **Step 1: Write the failing test** (append to `limit_test.go`)

```go
func TestClassifyLimitTransientAndUsage(t *testing.T) {
	cases := []struct {
		name, agent, msg string
		want             LimitKind
	}{
		{"codex 429 retry limit", "codex",
			`codex stream reported failure: exceeded retry limit, last status: 429 Too Many Requests, request id: abc`,
			LimitKindTransient},
		{"codex stream disconnect", "codex",
			`codex stream reported failure: Reconnecting... 2/5 (stream disconnected before completion: An error occurred while processing your request ... help.openai.com)`,
			LimitKindTransient},
		{"gemini 429 capacity", "gemini",
			`Attempt 1 failed with status 429. No capacity available for model gemini-3.1-pro-preview on the server`,
			LimitKindTransient},
		{"http 503", "codex", `agent: codex failed: 503 Service Unavailable`, LimitKindTransient},
		{"codex usage limit -> quota", "codex",
			`codex stream reported failure: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Mar 2nd, 2026 1:22 PM.`,
			LimitKindQuota},
		// Genuine/deterministic MUST NOT be transient:
		{"model not supported", "codex",
			`codex stream reported failure: {"detail":"The 'devstral-2' model is not supported when using Codex with a ChatGPT account."}`,
			LimitKindNone},
		{"unknown option", "droid", `agent: droid failed: error: unknown option '-C'`, LimitKindNone},
		{"stdin not a terminal", "codex", `agent: codex failed: Error: stdin is not a terminal`, LimitKindNone},
		{"context window", "codex", `Codex ran out of room in the model's context window.`, LimitKindNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyLimit(tc.agent, tc.msg).Kind)
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestClassifyLimitTransientAndUsage -v`
Expected: FAIL — transient/usage cases return `LimitKindNone` (rules absent).

- [ ] **Step 3: Add rules** to `defaultLimitRules` in `limit.go`. Insert transient rules BEFORE returning, and the codex usage-limit quota rule among the quota rules. Order does not matter (first match wins; the genuine strings match none).

```go
	// Transient/outage — observed provider wording only (no speculative
	// substrings; see the no-speculative note above). Retried with backoff.
	{Agents: []string{"*"}, Substring: "too many requests", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "status: 429", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "status 429", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "stream disconnected before completion", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "stream reported failure: reconnecting", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "500 internal server error", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "502 bad gateway", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "503 service unavailable", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "service unavailable", Kind: LimitKindTransient},
	// Codex ChatGPT-account usage cap — a quota skip, not a hard failure.
	{Agents: []string{"codex"}, Substring: "you've hit your usage limit", Kind: LimitKindQuota},
```

Update the `TODO`/no-speculative comment block above `defaultLimitRules` to note that transient substrings were added from captured outage strings and remain observation-driven.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -run TestClassifyLimitTransientAndUsage -v`
Expected: PASS. Also run `go test ./internal/agent/` (full package) — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./internal/agent/
git add internal/agent/limit.go internal/agent/limit_test.go
git commit -m "classify provider outages as transient; codex usage-limit as quota"
```

### Task 2: `OutageErrorPrefix` + apply on transient final-fail

**Files:**
- Modify: `internal/review/result.go` (add prefix + helper next to `QuotaErrorPrefix`)
- Modify: `internal/daemon/worker.go:851` (`failOrRetryInner`) and the quota-prefix site
- Test: `internal/daemon/worker_*_test.go` (new test in the worker test package)

- [ ] **Step 1: Write the failing test.** Find the existing failover/quota test context (`newWorkerTestContext`, `exhaustRetries`) in `internal/daemon`. Add a test asserting that a job whose agent error is transient and has no backup is stored with the `outage:` prefix.

```go
func TestTransientFinalFailureGetsOutagePrefix(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJobWithAgent(t, "test")
	tc.exhaustRetries(t, job.ID) // drive retry_count to max
	// No backup configured -> failOrRetryAgent must FailJob with the outage prefix.
	tc.wp.failOrRetryAgent(tc.workerID, job, "codex",
		"agent: codex failed: exit status 1 (parse error: codex stream reported failure: exceeded retry limit, last status: 429 Too Many Requests)")
	got, err := tc.db.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusFailed, got.Status)
	assert.True(t, strings.HasPrefix(got.Error, review.OutageErrorPrefix),
		"want %q prefix, got %q", review.OutageErrorPrefix, got.Error)
}
```

(Confirm helper names against `internal/daemon/worker_test.go`; adjust `createAndClaimJobWithAgent`/`exhaustRetries`/field names to the actual helpers if they differ.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestTransientFinalFailureGetsOutagePrefix -v`
Expected: FAIL — error stored without prefix.

- [ ] **Step 3: Add the prefix + helper** in `internal/review/result.go` (mirror `QuotaErrorPrefix`):

```go
// OutageErrorPrefix is prepended to error messages when a review failed due to
// a transient provider outage (429 / stream-disconnect / 5xx), so the batch
// layer can treat it as retryable rather than a genuine failure.
const OutageErrorPrefix = "outage: "

// OutageError prepends OutageErrorPrefix unless already present.
func OutageError(msg string) string {
	if strings.HasPrefix(msg, OutageErrorPrefix) {
		return msg
	}
	return OutageErrorPrefix + msg
}
```

- [ ] **Step 4: Apply in the worker.** In `failOrRetryInner` (`worker.go:851`), capture the classification. The `agentError` branch already computes `cls := wp.classify(...)`. Lift `cls` so the terminal `FailJob` calls can use it: when `cls.Kind == agent.LimitKindTransient` (or the message classifies transient) and the job is about to be marked failed (the two `wp.db.FailJob(job.ID, workerID, errorMsg)` sites at ~`:894` and ~`:929`), store `review.OutageError(errorMsg)` instead of the raw `errorMsg`. Keep `broadcastFailed`/`logJobFailed` using the unprefixed preview for logs. Mirror exactly how the quota path constructs `quotaMsg := review.QuotaErrorPrefix + errorMsg` (grep `QuotaErrorPrefix` in `worker.go`).

  Minimal approach: add a helper `finalErrorMsg(agentName, errorMsg string, agentError bool) string` that returns `review.OutageError(errorMsg)` when `agentError && wp.classify(agent.CanonicalName(agentName), errorMsg).Kind == agent.LimitKindTransient`, else `errorMsg`; use it at both `FailJob` sites.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestTransientFinalFailureGetsOutagePrefix -v` then `go test ./internal/daemon/ ./internal/review/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
go fmt ./... && go vet ./internal/daemon/ ./internal/review/
git add internal/review/result.go internal/daemon/worker.go internal/daemon/*_test.go
git commit -m "tag transient final failures with outage: prefix"
```

### Task 3: `IsTransientFailure` / `CountTransientFailures`

**Files:**
- Create: `internal/review/failclass.go`
- Test: `internal/review/failclass_test.go`

- [ ] **Step 1: Write the failing test**

```go
package review

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsTransientFailure(t *testing.T) {
	assert := assert.New(t)
	transient := ReviewResult{Status: ResultFailed, Error: OutageErrorPrefix + "429 too many requests"}
	quota := ReviewResult{Status: ResultFailed, Error: QuotaErrorPrefix + "quota exceeded"}
	genuine := ReviewResult{Status: ResultFailed, Error: "model not supported"}
	assert.True(IsTransientFailure(transient))
	assert.False(IsTransientFailure(quota))
	assert.False(IsTransientFailure(genuine))
	assert.Equal(1, CountTransientFailures([]ReviewResult{transient, quota, genuine}))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/review/ -run TestIsTransientFailure -v`
Expected: FAIL — undefined `IsTransientFailure`.

- [ ] **Step 3: Implement** `internal/review/failclass.go`:

```go
package review

import "strings"

// IsTransientFailure reports whether a review failed due to a transient
// provider outage (tagged with OutageErrorPrefix), as opposed to quota,
// timeout, or a genuine/deterministic failure.
func IsTransientFailure(r ReviewResult) bool {
	return r.Status == ResultFailed && strings.HasPrefix(r.Error, OutageErrorPrefix)
}

// CountTransientFailures returns the number of transient-outage failures.
func CountTransientFailures(reviews []ReviewResult) int {
	n := 0
	for _, r := range reviews {
		if IsTransientFailure(r) {
			n++
		}
	}
	return n
}

// IsGenuineFailure reports a failed review that is neither quota, timeout, nor
// transient — i.e. a deterministic failure that retrying will not fix soon.
func IsGenuineFailure(r ReviewResult) bool {
	return r.Status == ResultFailed &&
		!IsQuotaFailure(r) && !IsTransientFailure(r) && !IsTimeoutCancellation(r)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/review/ -run TestIsTransientFailure -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./internal/review/
git add internal/review/failclass.go internal/review/failclass_test.go
git commit -m "add transient/genuine failure predicates"
```

---

## Phase 2 — Backoff schedule and comment templates

### Task 4: Backoff helper

**Files:**
- Create: `internal/review/retry.go`
- Test: `internal/review/retry_test.go`

- [ ] **Step 1: Write the failing test**

```go
package review

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRetrySchedule(t *testing.T) {
	assert := assert.New(t)
	s := DefaultRetrySchedule // base 2m, cap 1h, transient cap 72h, genuineMax 3
	assert.Equal(2*time.Minute, s.NextDelay(1))
	assert.Equal(4*time.Minute, s.NextDelay(2))
	assert.Equal(8*time.Minute, s.NextDelay(3))
	assert.Equal(time.Hour, s.NextDelay(20))   // capped
	assert.Equal(time.Hour, s.NextDelay(1000)) // capped, no overflow
	assert.False(s.TransientExhausted(71*time.Hour))
	assert.True(s.TransientExhausted(73*time.Hour))
	assert.False(s.GenuineExhausted(2))
	assert.True(s.GenuineExhausted(3))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/review/ -run TestRetrySchedule -v`
Expected: FAIL — undefined `DefaultRetrySchedule`.

- [ ] **Step 3: Implement** `internal/review/retry.go`:

```go
package review

import "time"

// RetrySchedule defines CI review retry backoff and give-up bounds.
type RetrySchedule struct {
	Base          time.Duration // first delay
	Cap           time.Duration // max single delay
	TransientWall time.Duration // give up transient retries after this since first attempt
	GenuineMax    int           // max consecutive genuine attempts before soft note
}

// DefaultRetrySchedule: 2m, 4m, 8m ... capped at 1h then hourly; transient
// give-up at 3 days; genuine give-up after 3 consecutive genuine attempts.
var DefaultRetrySchedule = RetrySchedule{
	Base:          2 * time.Minute,
	Cap:           time.Hour,
	TransientWall: 72 * time.Hour,
	GenuineMax:    3,
}

// NextDelay returns the backoff before the next attempt given the 1-based count
// of attempts already made. Exponential (Base*2^(n-1)) capped at Cap.
func (s RetrySchedule) NextDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := s.Base
	for i := 1; i < attempt && d < s.Cap; i++ {
		d *= 2
	}
	if d > s.Cap {
		d = s.Cap
	}
	return d
}

// TransientExhausted reports whether transient retries have exceeded the wall
// clock since the first attempt.
func (s RetrySchedule) TransientExhausted(sinceFirst time.Duration) bool {
	return sinceFirst > s.TransientWall
}

// GenuineExhausted reports whether the consecutive-genuine streak hit the cap.
func (s RetrySchedule) GenuineExhausted(consecutiveGenuine int) bool {
	return consecutiveGenuine >= s.GenuineMax
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/review/ -run TestRetrySchedule -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./internal/review/
git add internal/review/retry.go internal/review/retry_test.go
git commit -m "add CI review retry backoff schedule"
```

### Task 5: Give-up / soft-note comment formatters + transient label

**Files:**
- Modify: `internal/review/synthesis.go` (new formatters; transient label in `FormatAllFailedComment`, `FormatRawBatchComment`, `BuildSynthesisPrompt`)
- Test: `internal/review/synthesis_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestGiveUpAndSoftNoteComments(t *testing.T) {
	assert := assert.New(t)
	g := FormatTransientGiveUpComment("abc1234def", "429 too many requests")
	assert.Contains(g, "## roborev: Review Unavailable (`abc1234`)")
	assert.Contains(g, "3 days")
	assert.Contains(g, "429 too many requests")

	s := FormatGenuineSoftNoteComment("abc1234def", "model not supported")
	assert.Contains(s, "## roborev: Review Unavailable (`abc1234`)")
	assert.Contains(s, "next commit")
	assert.Contains(s, "model not supported")
}

func TestTransientMemberRendersSkipped(t *testing.T) {
	r := ReviewResult{Agent: "codex", ReviewType: "default",
		Status: ResultFailed, Error: OutageErrorPrefix + "429"}
	out := FormatRawBatchComment([]ReviewResult{r}, "abc1234def")
	assert.Contains(t, out, "provider unavailable")
	assert.NotContains(t, out, "Review failed. Check CI logs")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/review/ -run 'TestGiveUpAndSoftNoteComments|TestTransientMemberRendersSkipped' -v`
Expected: FAIL — undefined formatters; transient not labelled.

- [ ] **Step 3: Implement** in `synthesis.go`:

```go
// FormatTransientGiveUpComment is posted after the 3-day transient retry cap.
func FormatTransientGiveUpComment(headSHA, lastErrExcerpt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## roborev: Review Unavailable (`%s`)\n\n", gitrepo.ShortSHA(headSHA))
	b.WriteString("roborev tried to review this PR for 3 days but the AI provider " +
		"was repeatedly unavailable, so no review was produced.\n\n")
	if strings.TrimSpace(lastErrExcerpt) != "" {
		fmt.Fprintf(&b, "Last error: `%s`\n", oneLineExcerpt(lastErrExcerpt))
	}
	return b.String()
}

// FormatGenuineSoftNoteComment is posted after bounded genuine failures.
func FormatGenuineSoftNoteComment(headSHA, lastErrExcerpt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## roborev: Review Unavailable (`%s`)\n\n", gitrepo.ShortSHA(headSHA))
	b.WriteString("The review agent repeatedly failed to run (likely an agent or " +
		"configuration error). roborev will try again on the next commit.\n\n")
	if strings.TrimSpace(lastErrExcerpt) != "" {
		fmt.Fprintf(&b, "Last error: `%s`\n", oneLineExcerpt(lastErrExcerpt))
	}
	return b.String()
}

// oneLineExcerpt collapses whitespace and truncates for a single-line excerpt.
func oneLineExcerpt(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	s = strings.TrimSpace(s)
	const max = 200
	if len(s) > max {
		s = TrimPartialRune(s[:max]) + "..."
	}
	return s
}
```

  In `FormatRawBatchComment` and `FormatAllFailedComment`, add a transient branch alongside the quota/timeout branches: when `IsTransientFailure(r)`, label the member "skipped (provider unavailable)" (raw) / `skipped (provider unavailable)` (all-failed list) instead of "failed", and in `FormatRawBatchComment` write "Review skipped — provider temporarily unavailable.\n\n" instead of the "Review failed. Check CI logs" body. In `BuildSynthesisPrompt`, add `else if IsTransientFailure(r)` → `[SKIPPED]` + "(review skipped — provider unavailable)" mirroring the quota branch at `synthesis.go:80`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/review/ -run 'TestGiveUpAndSoftNoteComments|TestTransientMemberRendersSkipped' -v` then `go test ./internal/review/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./internal/review/
git add internal/review/synthesis.go internal/review/synthesis_test.go
git commit -m "add give-up/soft-note comments; label transient members as skipped"
```

---

## Phase 3 — `ci_pr_review_attempts` storage

### Task 6: Table DDL + migrations (SQLite + Postgres)

**Files:**
- Modify: `internal/storage/db.go` (schema string, near `ci_pr_panels` at `:104`)
- Modify: `internal/storage/postgres.go` (schema parity; do NOT add to sync cursors)
- Test: `internal/storage/review_attempts_test.go` (table-exists smoke test)

- [ ] **Step 1: Write the failing test**

```go
func TestReviewAttemptsTableExists(t *testing.T) {
	db := openTestDB(t)
	_, err := db.Exec(`INSERT INTO ci_pr_review_attempts
		(github_repo, pr_number, head_sha, attempt, first_attempt_at, next_attempt_at,
		 last_error_class, consecutive_genuine_attempts, last_error_excerpt,
		 last_panel_run_uuid, state, updated_at)
		VALUES ('o/r', 1, 'sha', 1, datetime('now'), NULL, '', 0, '', '', 'pending', datetime('now'))`)
	require.NoError(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/storage/ -run TestReviewAttemptsTableExists -v`
Expected: FAIL — "no such table: ci_pr_review_attempts".

- [ ] **Step 3: Add DDL** to the SQLite schema string in `db.go` (after the `ci_pr_panels` block):

```sql
CREATE TABLE IF NOT EXISTS ci_pr_review_attempts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    github_repo TEXT NOT NULL,
    pr_number INTEGER NOT NULL,
    head_sha TEXT NOT NULL,
    attempt INTEGER NOT NULL DEFAULT 1,
    first_attempt_at TEXT NOT NULL,
    next_attempt_at TEXT,
    last_error_class TEXT NOT NULL DEFAULT '',
    consecutive_genuine_attempts INTEGER NOT NULL DEFAULT 0,
    last_error_excerpt TEXT NOT NULL DEFAULT '',
    last_panel_run_uuid TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'pending',
    updated_at TEXT NOT NULL,
    UNIQUE(github_repo, pr_number, head_sha)
);
```

  Add the Postgres equivalent to `postgres.go` (the schema-creation block): same columns, `id BIGSERIAL PRIMARY KEY`, `*_at TIMESTAMPTZ`, `UNIQUE(github_repo, pr_number, head_sha)`. Do NOT register it in any sync cursor (`sync.go`/`syncworker.go`) — confirm by grepping that other CI panel tables are likewise not synced.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/storage/ -run TestReviewAttemptsTableExists -v`
Expected: PASS. Postgres parity: `go test -tags postgres ./internal/storage/ -run TestReviewAttemptsTableExists` when `TEST_POSTGRES_URL` is set.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./internal/storage/
git add internal/storage/db.go internal/storage/postgres.go internal/storage/review_attempts_test.go
git commit -m "add ci_pr_review_attempts table (sqlite + postgres)"
```

### Task 7: Attempts model + CAS methods

**Files:**
- Create: `internal/storage/review_attempts.go`
- Test: `internal/storage/review_attempts_test.go`

Methods (all on `*DB`):
- `ReserveReviewAttempt(repo string, pr int, sha string, now time.Time) (created bool, err error)` — `INSERT ... ON CONFLICT(github_repo,pr_number,head_sha) DO NOTHING`; `created` = rows affected == 1.
- `GetReviewAttempt(repo string, pr int, sha string) (*ReviewAttempt, error)` — returns `nil, nil` on no row.
- `DeferReviewAttempt(repo string, pr int, sha, errClass, excerpt, lastRunUUID string, nextAttemptAt time.Time, bumpGenuine bool) error` — sets `state='deferred'`, `next_attempt_at`, error fields; `consecutive_genuine_attempts = CASE WHEN bumpGenuine THEN consecutive_genuine_attempts+1 ELSE 0 END`.
- `ClaimDueReviewAttempt(repo string, pr int, sha string, now time.Time) (claimed bool, attempt int, firstAttemptAt time.Time, err error)` — CAS: `UPDATE ... SET state='pending', attempt=attempt+1, next_attempt_at=NULL WHERE state='deferred' AND next_attempt_at<=now`; on 1 row, re-`SELECT` the row to return `attempt`/`first_attempt_at`.
- `MarkReviewAttemptDone(repo string, pr int, sha string) error` — `state='done'`.
- `DeleteReviewAttempt(repo string, pr int, sha string) error`.
- `DeleteReviewAttemptsForPR(repo string, pr int) (int64, error)` — closed-PR cleanup.
- `GetNonTerminalAttemptPRs(repo string) ([]PanelPRRef, error)` — `SELECT DISTINCT github_repo, pr_number WHERE state IN ('pending','deferred')`.
- `GetDueReviewAttempts(repo string, now time.Time) ([]ReviewAttempt, error)` — `state='deferred' AND next_attempt_at<=now`.

- [ ] **Step 1: Write the failing test**

```go
func TestReviewAttemptLifecycle(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	now := time.Now()

	created, err := db.ReserveReviewAttempt("o/r", 7, "sha1", now)
	require.NoError(t, err)
	assert.True(created)
	// Second reserve for same key is a no-op (dedup).
	created2, err := db.ReserveReviewAttempt("o/r", 7, "sha1", now)
	require.NoError(t, err)
	assert.False(created2)

	// Defer (transient) resets genuine streak.
	require.NoError(t, db.DeferReviewAttempt("o/r", 7, "sha1", "transient", "429", "uuid1",
		now.Add(-time.Minute), false))
	a, err := db.GetReviewAttempt("o/r", 7, "sha1")
	require.NoError(t, err)
	assert.Equal("deferred", a.State)
	assert.Equal(0, a.ConsecutiveGenuineAttempts)

	// Claim the due row (CAS) — exactly one claim succeeds.
	claimed, attempt, _, err := db.ClaimDueReviewAttempt("o/r", 7, "sha1", now)
	require.NoError(t, err)
	assert.True(claimed)
	assert.Equal(2, attempt)
	claimedAgain, _, _, err := db.ClaimDueReviewAttempt("o/r", 7, "sha1", now)
	require.NoError(t, err)
	assert.False(claimedAgain) // now 'pending', not 'deferred'

	// Genuine defer bumps the streak.
	require.NoError(t, db.DeferReviewAttempt("o/r", 7, "sha1", "genuine", "bad model", "uuid2",
		now.Add(-time.Minute), true))
	a, _ = db.GetReviewAttempt("o/r", 7, "sha1")
	assert.Equal(1, a.ConsecutiveGenuineAttempts)

	// Closed-PR cleanup deletes it.
	n, err := db.DeleteReviewAttemptsForPR("o/r", 7)
	require.NoError(t, err)
	assert.Equal(int64(1), n)
	a, err = db.GetReviewAttempt("o/r", 7, "sha1")
	require.NoError(t, err)
	assert.Nil(a)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/storage/ -run TestReviewAttemptLifecycle -v`
Expected: FAIL — undefined methods.

- [ ] **Step 3: Implement** `internal/storage/review_attempts.go`: the `ReviewAttempt` struct (fields matching columns; timestamps via the existing `parseSQLiteTime` helper used by `scanCIPanel`), the methods above using `db.Exec`/`db.QueryRow`, `RowsAffected()` for CAS results, and `?` placeholders. Use `ON CONFLICT(github_repo, pr_number, head_sha) DO NOTHING` for reserve. Follow `ci_panels.go` scanning conventions (`sql.Null*` for nullable `next_attempt_at`, `parseSQLiteTime`). For `ClaimDueReviewAttempt`, compare timestamps with SQLite `datetime(next_attempt_at) <= datetime(?)` passing `now.Format(time.RFC3339)` — mirror the `GetTimedOutPanels` datetime handling at `ci_panels.go:274` to avoid the 'T'-vs-space pitfall noted there.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/storage/ -run TestReviewAttemptLifecycle -v` then `go test ./internal/storage/`
Expected: PASS.

- [ ] **Step 5: Add a CAS-contention test** (two goroutines claim the same due row; exactly one wins):

```go
func TestClaimDueReviewAttemptIsExclusive(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()
	require.NoError(t, db.ReserveReviewAttemptThenDefer(t, "o/r", 9, "s", now)) // helper: reserve + defer due
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _, _, _ := db.ClaimDueReviewAttempt("o/r", 9, "s", now); ok {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), atomic.LoadInt32(&wins))
}
```

  (Inline the reserve+defer instead of a helper if simpler. SQLite single-writer serializes the UPDATEs, so exactly one matches `state='deferred'`.)

- [ ] **Step 6: Run + commit**

```bash
go test ./internal/storage/ -run 'TestReviewAttempt|TestClaimDue' -v
go fmt ./... && go vet ./internal/storage/
git add internal/storage/review_attempts.go internal/storage/review_attempts_test.go
git commit -m "add ci_pr_review_attempts CAS methods"
```

---

## Phase 4 — Poller integration

> These tasks modify `internal/daemon/ci_poller.go`. Read `postPanelRun` (`:1590`), `panelCommitStatus` (`:1700`), `processPR`/enqueue (`:300`–`:401`), `alreadyReviewedPR` (`:451`), `cleanupClosedPRPanels` (`:1916`), and `CreateCIPanelRun` (`internal/storage/ci_panels.go:109`) before editing. Member/synth `EnqueueOpts` are built in the existing enqueue path — factor that into a reusable `func (p *CIPoller) buildPanelOpts(...)` so reserve and retry share it.

### Task 8: Finalize outcome classifier + non-blocking status

**Files:**
- Modify: `internal/daemon/ci_poller.go` (`postPanelRun`, `panelCommitStatus`)
- Create: `internal/daemon/panel_outcome.go` (pure classifier, easy to unit test)
- Test: `internal/daemon/panel_outcome_test.go`

- [ ] **Step 1: Write the failing test** for a pure classifier over member results + attempt state:

```go
func TestClassifyPanelOutcome(t *testing.T) {
	assert := assert.New(t)
	mk := func(status storage.JobStatus, errPrefix string) reviewpkg.ReviewResult {
		return reviewpkg.ReviewResult{Status: reviewpkg.ResultStatus(status), Error: errPrefix}
	}
	ok := reviewpkg.ReviewResult{Status: reviewpkg.ResultDone, Output: "Findings"}
	transient := mk("failed", reviewpkg.OutageErrorPrefix+"429")
	genuine := mk("failed", "bad model")
	quota := mk("failed", reviewpkg.QuotaErrorPrefix+"quota")

	assert.Equal(OutcomePost, classifyPanelOutcome([]reviewpkg.ReviewResult{ok, transient}, 0).Kind)
	assert.Equal(OutcomeDeferTransient, classifyPanelOutcome([]reviewpkg.ReviewResult{transient}, 0).Kind)
	assert.Equal(OutcomeDeferGenuine, classifyPanelOutcome([]reviewpkg.ReviewResult{genuine}, 1).Kind)
	assert.Equal(OutcomeGenuineGiveUp, classifyPanelOutcome([]reviewpkg.ReviewResult{genuine}, 3).Kind)
	assert.Equal(OutcomeAllSkip, classifyPanelOutcome([]reviewpkg.ReviewResult{quota}, 0).Kind)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestClassifyPanelOutcome -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement** `internal/daemon/panel_outcome.go` — an enum `OutcomeKind` (`OutcomePost`, `OutcomeDeferTransient`, `OutcomeDeferGenuine`, `OutcomeGenuineGiveUp`, `OutcomeAllSkip`) and `classifyPanelOutcome(results []reviewpkg.ReviewResult, consecutiveGenuine int) PanelOutcome` implementing the spec precedence:
  1. any `ResultDone` with output → `OutcomePost`.
  2. else any `IsTransientFailure` → `OutcomeDeferTransient`.
  3. else any `IsGenuineFailure` → `GenuineExhausted(consecutiveGenuine+1)` ? `OutcomeGenuineGiveUp` : `OutcomeDeferGenuine`.
  4. else (all quota/timeout/skip) → `OutcomeAllSkip`.

  Return a struct carrying the chosen kind and a representative `LastErrorExcerpt` (first transient/genuine error). Use `reviewpkg.DefaultRetrySchedule.GenuineExhausted`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestClassifyPanelOutcome -v`
Expected: PASS.

- [ ] **Step 5: Wire into `postPanelRun`.** After loading `members` (`:1611`), compute `results := toReviewResults(members)`, fetch the attempt row (`GetReviewAttempt`), and `out := classifyPanelOutcome(results, attempt.ConsecutiveGenuineAttempts)`. Branch:
  - `OutcomePost` / `OutcomeAllSkip` / `OutcomeGenuineGiveUp` / transient-3-day → post a comment (existing body for Post/AllSkip; `FormatGenuineSoftNoteComment` / `FormatTransientGiveUpComment` for give-ups), set status, `MarkReviewAttemptDone`, `MarkPanelPosted`.
  - `OutcomeDeferTransient`: if `DefaultRetrySchedule.TransientExhausted(now.Sub(attempt.FirstAttemptAt))` → transient give-up (post + done). Else `DeferReviewAttempt(... bumpGenuine=false ...)` + `MarkPanelRetired` + status `pending` ("Review pending — provider unavailable, retrying"); no comment.
  - `OutcomeDeferGenuine`: `DeferReviewAttempt(... bumpGenuine=true ...)` + `MarkPanelRetired` + status `pending`; no comment.

  Update `panelCommitStatus` (`:1700`) so `reviewpkg.CountTransientFailures` is subtracted from `realFailures` like quota/timeout, and the `completed == 0` all-failed branch is unreachable for transient (handled by defer before status is set). Keep the existing success/skip branches.

- [ ] **Step 6: Add a focused test** that `postPanelRun` defers (no comment posted, panel retired, attempt deferred) when all members are transient. Use the poller test seams: `setCommitStatusFn` and the PR-comment post hook (grep `callPostPRComment`/`postPRCommentFn` in `ci_poller_test.go`). Assert no comment posted, status == `pending`, `GetReviewAttempt` state == `deferred`.

Run: `go test ./internal/daemon/ -run 'TestClassifyPanelOutcome|TestPostPanelRunDefersTransient' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
go fmt ./... && go vet ./internal/daemon/
git add internal/daemon/panel_outcome.go internal/daemon/panel_outcome_test.go internal/daemon/ci_poller.go
git commit -m "defer transient/genuine panel failures instead of posting Review Failed"
```

### Task 9: Reserve-on-enqueue + retry sweep

**Files:**
- Modify: `internal/daemon/ci_poller.go` (`processPR` enqueue, `alreadyReviewedPR`, poll loop `:292`, new `retryDueReviewAttempts`)
- Test: `internal/daemon/ci_poller_test.go`

- [ ] **Step 1: Write the failing test** — an end-to-end-ish poller test (using existing poller test scaffolding) where the first panel run finishes all-transient → deferred (no comment), then advancing time and running the retry sweep enqueues a new panel run; when that run succeeds, a real combined comment is posted. Model it on the closest existing `ci_poller_test.go` panel test; assert: 0 comments after defer, 1 comment after the successful retry, attempt state transitions `pending→deferred→pending→done`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestRetrySweepReenqueuesAfterTransient -v`
Expected: FAIL — `retryDueReviewAttempts` undefined / no re-enqueue.

- [ ] **Step 3: Implement.**
  - **Reserve on initial enqueue:** in the enqueue path (`processPR`, around `CreateCIPanelRun` at `:401`), call `ReserveReviewAttempt(repo, pr, headSHA, now)` in the same logical step; only proceed to `CreateCIPanelRun` when `created` is true (or when claiming a due retry — see below). Ideally extend `CreateCIPanelRun` to insert the attempts row in its existing transaction, OR wrap both in one `db` transaction; if keeping them separate, reserve first and treat reserve-loser as "already handled".
  - **`alreadyReviewedPR`:** change to return true when `GetReviewAttempt` returns a non-nil row in ANY state (replacing/augmenting the `GetActiveCIPanelByPRSHA` check). Keep the panel check as a fallback for in-flight runs predating this row.
  - **`retryDueReviewAttempts(ctx, ghRepo, cfg)`:** `GetDueReviewAttempts(ghRepo, now)`; for each, `callIsPROpen` — if closed, `DeleteReviewAttempt` and continue; else `ClaimDueReviewAttempt` (CAS) and on success build opts via `buildPanelOpts(...)` and `CreateCIPanelRun(...)` for the same repo/pr/sha. Wire the call into the poll loop next to `cleanupClosedPRPanels` (`:292`).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestRetrySweepReenqueuesAfterTransient -v` then `go test ./internal/daemon/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./internal/daemon/
git add internal/daemon/ci_poller.go internal/daemon/ci_poller_test.go
git commit -m "reserve review attempts on enqueue; add retry sweep with backoff"
```

### Task 10: Closed-PR attempts cleanup + crash reconcile

**Files:**
- Modify: `internal/daemon/ci_poller.go` (`cleanupClosedPRPanels` → also clean attempts; new reconcile)
- Test: `internal/daemon/ci_poller_test.go`

- [ ] **Step 1: Write the failing test** — a deferred attempt whose panel is already retired (so it is NOT in `GetPendingPanelPRs`) gets cleaned up when its PR is closed:

```go
func TestClosedPRCleansUpDeferredAttempt(t *testing.T) {
	// reserve + defer an attempt for PR 5 with its panel retired (no active panel)
	// run cleanupClosedPRPanels with openPRs excluding 5 and callIsPROpen=false
	// assert GetReviewAttempt(...,5,...) == nil (deleted), enabling fresh reopen
}
```

  Fill in using the poller test scaffolding (`newCIPollerTest` or equivalent in `ci_poller_test.go`); set `isPROpenFn` to false for PR 5.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestClosedPRCleansUpDeferredAttempt -v`
Expected: FAIL — attempt survives (cleanup is panel-driven only).

- [ ] **Step 3: Implement.**
  - In `cleanupClosedPRPanels`, after handling panel PRs, also iterate `GetNonTerminalAttemptPRs(ghRepo)`; for each PR absent from `openPRs` AND confirmed closed by `callIsPROpen`, call `DeleteReviewAttemptsForPR(ghRepo, pr)`. Union with the panel-PR set so each PR is open-checked once.
  - **Reconcile:** add `reconcileStuckAttempts(ghRepo)` — find `pending` attempts whose latest panel run (`last_panel_run_uuid` → `GetSynthesisJob`) is missing or terminal-without-post, and re-defer them with `next_attempt_at = now + NextDelay(attempt)`. Call it in the poll loop. Add a test that a `pending` attempt with no live panel gets re-deferred.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run 'TestClosedPRCleansUpDeferredAttempt|TestReconcileStuckAttempt' -v` then `go test ./internal/daemon/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./internal/daemon/
git add internal/daemon/ci_poller.go internal/daemon/ci_poller_test.go
git commit -m "clean up deferred attempts on PR close; reconcile stuck attempts"
```

---

## Final verification

- [ ] `go build ./...` — Expected: clean.
- [ ] `go vet ./...` — Expected: clean.
- [ ] `go test ./...` — Expected: all pass.
- [ ] `go test -tags integration ./...` — Expected: all pass.
- [ ] `make lint` (golangci-lint) — Expected: clean.
- [ ] Manual trace against the spec's behavioral contract: outage → no "Review Failed", deferred + retried; 3-day → give-up note; genuine → soft note after 3; closed/reopened at same SHA → fresh review.

## Spec self-review notes
- Spec coverage: classification (T1–T3), backoff (T4), comments (T5), table+migrations (T6), CAS methods (T7), finalize classifier+status (T8), reserve+retry sweep (T9), closed-PR cleanup+reconcile (T10) — all spec sections mapped.
- The pure `classifyPanelOutcome` (T8) and `RetrySchedule` (T4) isolate the hard logic for fast unit tests; poller wiring tasks depend on them.
- Postgres: table added for parity, not synced (T6) — matches spec.
