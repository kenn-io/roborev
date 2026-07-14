# CI Quiet-Hours Throttle

Date: 2026-07-14
Status: Approved

## Problem

Overnight, goal-type agent sessions push to PRs frequently. Each push triggers
a full CI review panel, burning tokens on reviews that are stale by morning.
The existing `[ci] throttle_interval` cannot express this: it is bypassable
per-user (`throttle_bypass_users`) and applies around the clock.

## Goal

A configurable quiet-hours window (e.g. 23:00–05:00 US/Central) during which a
stronger per-PR throttle applies to **all** users, bypass list included.
Contributors still get reviews overnight — just at most once per
`throttle_interval` (default 1h) per PR. This is not a shutdown: in-flight
panels complete, synthesize, and post normally.

## Design

### Config

New nested table under global `[ci]` only (no per-repo override — quiet hours
reflect the operator's schedule, not repo policy):

```toml
[ci.quiet_hours]
start = "23:00"            # HH:MM, required to enable
end = "05:00"              # HH:MM; start > end wraps past midnight
timezone = "US/Central"    # IANA name; empty = machine local time
throttle_interval = "1h"   # per-PR minimum interval during the window; default 1h
```

- Feature is enabled only when both `start` and `end` are set.
- `start == end` is treated as disabled (a zero-length window, not 24h).
- Empty `timezone` means machine local time (`time.Local`). Implementation
  note: this must be special-cased — `time.LoadLocation("")` returns UTC,
  not local time.
- `throttle_interval = "0"` is valid and makes the feature a no-op (a zero
  quiet interval never exceeds the base throttle under max semantics),
  mirroring how `throttle_interval = "0"` disables the base throttle.
- Invalid `start`/`end`/`timezone`/`throttle_interval` values: log a warning
  and treat the feature as disabled, matching the lenient style of
  `ResolvedThrottleInterval`. A typo must not throttle harder than
  configured.

Struct: `QuietHoursConfig` in `internal/config/ci.go`, embedded in `CIConfig`
as `QuietHours QuietHoursConfig \`toml:"quiet_hours"\``. Parsing and checking
are split so validation runs once per poll cycle, not per PR:

```go
// Resolve parses and validates the config. Returns (nil, nil) when the
// feature is disabled (start/end unset or start == end), and an error on
// invalid values.
func (q *QuietHoursConfig) Resolve() (*QuietHoursWindow, error)

// Active reports whether t falls inside the window.
func (w *QuietHoursWindow) Active(t time.Time) bool
```

`QuietHoursWindow` carries the parsed start/end clock times, the
`*time.Location`, and the resolved `Interval time.Duration` (default 1h).

### Window semantics

- Boundaries: active when `start <= now < end` in the configured timezone.
- Wrap: when `start > end` (e.g. 23:00–05:00), active when
  `now >= start || now < end`.
- Clock time comparison uses the instant converted into the configured
  `time.Location` (`time.LoadLocation`), so DST is handled by the stdlib.

### Poller integration (`internal/daemon/ci_poller.go`)

The window is resolved **once per poll cycle** in `poll()` and the resolved
`*QuietHoursWindow` is passed down to `throttlePR`. An invalid config logs
one warning per poll cycle (not per PR) and disables the feature for that
cycle; valid config is parsed once instead of re-parsing HH:MM strings per
PR.

`throttlePR` applies it as an effective-interval override:

```go
throttle := cfg.CI.ResolvedThrottleInterval()
if cfg.CI.IsThrottleBypassed(pr.Author.Login) {
    throttle = 0
}
if quiet != nil && quiet.Active(p.now()) && quiet.Interval > throttle {
    throttle = quiet.Interval
}
if throttle <= 0 {
    return false, nil
}
// ... existing LatestPanelTimeForPR check, deferred commit status, unchanged
```

- Bypass users bypass only the normal interval; the quiet-hours interval
  applies to everyone.
- A PR with no prior panel run (`lastReview.IsZero()`) is still reviewed
  immediately, even inside the window — first reviews are never blocked.
- The existing "Review deferred — next eligible at HH:MM UTC" pending commit
  status is reused unchanged. Known cosmetic inaccuracy, accepted: with the
  quiet interval, `lastReview.Add(throttle)` may land after the window ends,
  overstating the wait for bypass users who become eligible at the first
  poll after the window (e.g. last review 04:50, window ends 05:00, status
  says 05:50). The status is advisory; capping it at the window end would
  add wrap-around complexity for a cosmetic string.
- **Clock source**: add a `now func() time.Time` field on `CIPoller`
  (defaulting to `time.Now`). `throttlePR` uses it for both the window check
  and the interval comparison (`p.now().Sub(lastReview)` replaces
  `time.Since(lastReview)`), so tests control one clock for both. Precedent
  for injected clocks exists per-feature (`discordNowFn` in
  `ci_discord.go`); a poller-wide field is new.

### Explicitly out of scope

- **Retry sweep** (`retryDueReviewAttempts`): untouched. It completes
  already-committed attempts (provider-outage retries), low volume.
- **In-flight panels**: complete, synthesize, and post normally.
- **Lifecycle sweeps** (closed-PR cleanup, stuck-attempt reconcile, posting
  recovery): untouched.
- **Global rate limiting** across PRs: rejected — per-PR matches the existing
  throttle model and the stated need.
- **Per-repo override**: not needed.

### Metrics impact

None. `ExportCIMetrics` turnaround starts at `ci_pr_panels.created_at` /
`first_attempt_at`, both created at enqueue time inside `CreateCIPanelRun`.
A quiet-hours-throttled PR has no panel or attempt row until it is actually
enqueued, so waiting time is invisible to exported metrics — identical to the
existing contributor throttle.

## Testing

- Table-driven unit tests for the window check in `internal/config`:
  same-day window, midnight wrap, boundary instants (start inclusive, end
  exclusive), timezone conversion, unset config, `start == end`, invalid
  time/timezone/duration strings.
- `throttlePR` tests in `internal/daemon`:
  - quiet hours active + recent review → throttled
  - bypass user + quiet hours active + recent review → still throttled
  - bypass user + quiet hours inactive → not throttled (existing behavior)
  - quiet hours active + no prior review → reviewed immediately
  - quiet interval shorter than normal interval → normal interval wins
    (max semantics)
