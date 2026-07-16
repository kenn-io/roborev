# Quiet-Hours Bypass Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a global, case-insensitive quiet-hours bypass list that exempts selected PR authors only from the additional quiet-hours throttle.

**Architecture:** `QuietHoursConfig` owns the new TOML field and username membership check. The CI poller composes the ordinary throttle and quiet-hours throttle independently, skipping only the quiet-hours layer for matching authors. The Zensical GitHub integration guide documents the interaction.

**Tech Stack:** Go, BurntSushi TOML configuration loading, testify, Zensical Markdown documentation.

## Global Constraints

- `[ci.quiet_hours].bypass_users` is global and case-insensitive.
- The new list bypasses only the additional quiet-hours throttle; `[ci].throttle_interval` still applies independently.
- Omitting the list preserves the current all-user quiet-hours behavior.
- Use only synthetic identities and generic behavior descriptions in tests, docs, commits, and PR text.
- Remove `docs/superpowers/` working documents before opening the PR.

---

### Task 1: Quiet-Hours Bypass Configuration

**Files:**
- Modify: `internal/config/ci.go:310-423`
- Test: `internal/config/quiet_hours_test.go:183-204`

**Interfaces:**
- Consumes: TOML decoding into `config.CIConfig.QuietHours`.
- Produces: `QuietHoursConfig.BypassUsers []string` and `(*QuietHoursConfig).IsBypassed(login string) bool`.

- [ ] **Step 1: Write failing configuration tests**

Add a membership test using only synthetic identities:

```go
func TestQuietHoursBypassUsers(t *testing.T) {
	q := QuietHoursConfig{BypassUsers: []string{"Trusted-User"}}
	assert.True(t, q.IsBypassed("trusted-user"))
	assert.True(t, q.IsBypassed("TRUSTED-USER"))
	assert.False(t, q.IsBypassed("other-user"))
}
```

Extend `TestQuietHoursConfigTOMLParsing` with:

```toml
bypass_users = ["trusted-user"]
```

and assert:

```go
assert.Equal(t, []string{"trusted-user"}, q.BypassUsers)
```

- [ ] **Step 2: Run the configuration tests and verify RED**

Run:

```bash
go test ./internal/config -run 'TestQuietHours(BypassUsers|ConfigTOMLParsing)$'
```

Expected: compilation fails because `QuietHoursConfig.BypassUsers` and `IsBypassed` do not exist.

- [ ] **Step 3: Implement the configuration API**

Add the field:

```go
// BypassUsers lists GitHub usernames that bypass the additional
// quiet-hours throttle. Matching is case-insensitive.
BypassUsers []string `toml:"bypass_users"`
```

Factor case-insensitive membership into a shared helper and use it for both bypass lists:

```go
// IsBypassed reports whether the given GitHub login bypasses the additional
// quiet-hours throttle. Comparison is case-insensitive.
func (q *QuietHoursConfig) IsBypassed(login string) bool {
	return containsUsername(q.BypassUsers, login)
}

func containsUsername(users []string, login string) bool {
	lower := strings.ToLower(login)
	for _, user := range users {
		if strings.ToLower(user) == lower {
			return true
		}
	}
	return false
}

func (c *CIConfig) IsThrottleBypassed(login string) bool {
	return containsUsername(c.ThrottleBypassUsers, login)
}
```

- [ ] **Step 4: Run the configuration tests and verify GREEN**

Run:

```bash
go test ./internal/config -run 'TestQuietHours(BypassUsers|ConfigTOMLParsing)$'
```

Expected: PASS.

- [ ] **Step 5: Commit the configuration contract**

Stage `internal/config/ci.go` and `internal/config/quiet_hours_test.go`, then commit with a rationale-focused message describing the independent quiet-hours policy layer.

### Task 2: CI Poller Behavior and Zensical Documentation

**Files:**
- Modify: `internal/daemon/ci_poller.go:613-644`
- Test: `internal/daemon/ci_poller_quiet_hours_test.go:67-200`
- Modify: `docs/integrations/github.md:467-483`
- Modify: `docs/integrations/github.md:558-568`

**Interfaces:**
- Consumes: `(*config.QuietHoursConfig).IsBypassed(login string) bool` from Task 1.
- Produces: independent base/quiet-hours throttle composition in `(*CIPoller).throttlePR` and documented `[ci.quiet_hours].bypass_users` behavior.

- [ ] **Step 1: Write failing CI poller tests**

Add `TestCIPollerProcessPR_QuietHoursBypassSkipsQuietThrottle`: configure both bypass lists with `trusted-user`, keep the quiet window active, enqueue two different heads, and assert both heads receive panel runs.

Add `TestCIPollerProcessPR_QuietHoursBypassPreservesBaseThrottle`: configure only the quiet-hours bypass list with `trusted-user`, leave the base throttle at `1h`, enqueue two heads immediately, and assert the second head is deferred and the stale panel is superseded under ordinary-throttle semantics.

Use this synthetic setup:

```go
now := time.Now()
h := newQuietHoursHarness(t, baseThrottle, baseBypass, now)
h.Cfg.CI.QuietHours.BypassUsers = []string{"TRUSTED-USER"}
h.Poller.quietHours = quietWindow(t, now, "1h", true)
pr := func(sha string) ghPR {
	return ghPR{
		Number: 98, HeadRefOid: sha, BaseRefName: "main",
		Author: ghPRAuthor{Login: "trusted-user"},
	}
}
```

- [ ] **Step 2: Run the poller tests and verify RED**

Run:

```bash
go test ./internal/daemon -run 'TestCIPollerProcessPR_QuietHoursBypass'
```

Expected: the skip test fails because quiet hours still throttle every author; the base-throttle preservation test demonstrates the unchanged base behavior.

- [ ] **Step 3: Implement independent poller composition**

Change the quiet-hours layer in `throttlePR` to:

```go
if q := p.quietHours; q != nil && q.Active(p.nowFn()) &&
	!cfg.CI.QuietHours.IsBypassed(pr.Author.Login) && q.Interval > throttle {
	throttle = q.Interval
}
```

Update the function comment to state that quiet-hours bypass users skip only the additional quiet-hours interval.

- [ ] **Step 4: Run the poller tests and verify GREEN**

Run:

```bash
go test ./internal/daemon -run 'TestCIPollerProcessPR_QuietHours'
```

Expected: PASS, including existing quiet-hours retention and base-throttle tests.

- [ ] **Step 5: Update the Zensical GitHub integration docs**

In the Quiet Hours example add:

```toml
bypass_users = ["trusted-contributor"] # skips only the quiet-hours throttle
```

Explain that the quiet interval applies to every author except those in `quiet_hours.bypass_users`, that matching is case-insensitive, and that ordinary throttling still applies unless the author is also listed in `[ci].throttle_bypass_users`.

Add the Quiet Hours Options table row:

```markdown
| `bypass_users` | array | `[]` | GitHub usernames that bypass only the additional quiet-hours throttle (case-insensitive) |
```

- [ ] **Step 6: Run focused package tests**

Run:

```bash
go test ./internal/config ./internal/daemon
```

Expected: PASS.

- [ ] **Step 7: Commit poller behavior and public docs**

Stage `internal/daemon/ci_poller.go`, `internal/daemon/ci_poller_quiet_hours_test.go`, and `docs/integrations/github.md`, then commit with a rationale-focused message about selective quiet-hours exceptions preserving ordinary throttle policy.

### Task 3: Remove Working Documents and Verify the Public Branch

**Files:**
- Delete: `docs/superpowers/specs/2026-07-16-quiet-hours-bypass-design.md`
- Delete: `docs/superpowers/plans/2026-07-16-quiet-hours-bypass.md`

**Interfaces:**
- Consumes: completed implementation and documentation from Tasks 1-2.
- Produces: a public PR diff containing only product code, tests, and Zensical documentation.

- [ ] **Step 1: Delete the Superpowers working documents**

Remove both files listed above and confirm `git diff origin/main...HEAD -- docs/superpowers` is empty after the cleanup commit.

- [ ] **Step 2: Run full verification**

Run:

```bash
gofmt -w internal/config/ci.go internal/config/quiet_hours_test.go internal/daemon/ci_poller.go internal/daemon/ci_poller_quiet_hours_test.go
go test ./...
go build ./...
prek run --all-files
```

Expected: every command exits 0 with no failures.

- [ ] **Step 3: Run the public-data scrub**

Scan the complete diff, unpushed commit messages, and drafted PR title/body with the configured private-terms denylist plus absolute-path, hostname, email, and real-identity heuristics. Expected: zero new hits.

- [ ] **Step 4: Commit the working-document removal**

Stage both deletions and commit them without amending or squashing prior commits.

- [ ] **Step 5: Push and open the PR**

Push `feature/quiet-hours-bypass-users` to `origin` and create a rationale-first PR. The title and body must describe only the generic need for selective quiet-hours exceptions, contain no private deployment or incident details, and omit validation/checklist sections.
