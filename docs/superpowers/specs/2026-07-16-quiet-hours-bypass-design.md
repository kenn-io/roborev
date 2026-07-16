# Quiet-Hours Bypass Design

## Goal

Allow operators to exempt selected GitHub users from the additional CI
quiet-hours throttle without changing how the ordinary per-PR throttle applies.

## Configuration

Add a case-insensitive `bypass_users` list to `[ci.quiet_hours]`:

```toml
[ci.quiet_hours]
start = "23:00"
end = "05:00"
timezone = "UTC"
throttle_interval = "1h"
bypass_users = ["trusted-contributor"]
```

The list is global because quiet hours are global. An empty or omitted list
preserves the current behavior in which quiet hours apply to every user.

## Throttle Semantics

The base and quiet-hours throttle decisions remain independent:

- `[ci].throttle_bypass_users` controls only the ordinary throttle.
- `[ci.quiet_hours].bypass_users` controls only the additional quiet-hours
  throttle.
- A user must be exempt from both active layers to receive an immediate review.
- Username matching is case-insensitive, consistent with the existing base
  throttle bypass.

The poller will compute the base interval as it does today. It will apply the
quiet-hours interval only when the window is active and the PR author is not in
the new quiet-hours bypass list. Existing first-review, panel-retention,
supersede, and deferred-status behavior stays unchanged.

## Implementation Boundaries

`internal/config/ci.go` owns the new field and case-insensitive membership
check. `internal/daemon/ci_poller.go` consumes that check while combining the
base and quiet-hours intervals. No schema, API, or per-repository configuration
changes are required.

## Tests and Documentation

Configuration tests will verify TOML parsing and case-insensitive matching.
Poller tests will cover a quiet-hours bypass user with and without a base
throttle exemption, proving that bypassing quiet hours does not accidentally
bypass the ordinary throttle. The GitHub integration guide will document the
new option and its independent interaction with the existing bypass list.

All examples and fixtures will use synthetic identities and contain no
deployment-specific context.
