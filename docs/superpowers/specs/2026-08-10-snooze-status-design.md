# Snooze Status Design

## Problem

`roborev snooze` records a reminder suppression window for one repository,
linked worktree, and branch. The daemon uses that state when handling Agent Hook
events, but users can only observe it indirectly from the command that created
the snooze. There is no inventory of active snoozes in `roborev status`, and the
TUI does not indicate when its current checkout-and-branch view is snoozed.

The UI must not describe a whole repository as snoozed. A sibling worktree or a
different branch in the same repository remains unsnoozed.

## User Experience

`roborev status` will include an `Active Snoozes` section when at least one
snooze has not expired. Each row will show the repository name, worktree path,
branch, and local expiry time. The section is omitted when no snoozes are
active. `roborev status --json` will expose the same records in the daemon
object's `active_snoozes` array so scripts can inspect exact scopes without
parsing terminal output.

The TUI will add a prominent `[SNOOZED until <time>]` badge to the queue title
only when all of these conditions hold:

- the TUI was launched from a Git checkout;
- exactly one active repository filter matches that checkout's main repository;
- the active branch filter matches the checkout's branch;
- an active snooze matches the checkout's exact worktree path and branch.

The normal automatic repository and branch filters satisfy these conditions
when the TUI is launched inside the snoozed checkout. Clearing either filter,
selecting a different repository or branch, or launching outside that exact
worktree hides the badge. This avoids implying that a broader or sibling view
is snoozed. The badge remains in the title's non-optional left side so compact
mode cannot hide it.

## Data Model and API

Storage will gain a read-only query that returns every active local Agent Hook
snooze at a supplied time. It will join snooze rows to repository metadata,
exclude deadlines at or before that time, and order results deterministically
by repository name, worktree path, and branch. Expired rows may remain stored;
they are not returned.

`storage.DaemonStatus` will gain an `active_snoozes` field containing
`storage.AgentHookSnooze` records. `AgentHookSnooze` will include the repository
display name in addition to the existing root path, worktree path, branch, and
deadline. The daemon's existing status handler will populate this field from
the storage query. Using the status response keeps the CLI and TUI on the same
polling path and avoids a second endpoint or polling loop.

The generated daemon client and OpenAPI artifact will be regenerated through
the repository's existing generation target after the API schema changes.

## TUI Context Matching

The TUI model currently records the current checkout's main repository and
branch but not the checkout root. Startup discovery will also retain the exact
Git worktree root. A helper will compare the active filters, startup context,
and daemon-provided snoozes and return the matching active snooze, if any.

Repository identity reconciliation can expand an automatic repository filter
to multiple equivalent roots. Because the badge requires exactly one root, it
will remain hidden for such an aggregate filter. This is intentional: the view
no longer identifies one exact repository scope.

Status refreshes will naturally remove the badge after expiry or after
`roborev snooze off`. Display ticks can compare the deadline with the current
time so an already-expired badge disappears promptly even before the next
fallback status poll.

## Error Handling

If the active-snooze storage query fails, the daemon status request will fail
through the existing status error path instead of returning incomplete state
that could falsely present a checkout as unsnoozed. Existing CLI and TUI status
transport handling remains unchanged.

Paths are compared in their normalized stored form. The normal TUI automatic
branch filter is not created for detached HEAD, so that broad default view does
not identify an exact snooze scope and will not show the badge.

## Verification

Focused tests will cover:

- storage listing only active snoozes with repository metadata and stable order;
- daemon status JSON containing active snooze records;
- human and JSON `roborev status` output with and without active snoozes;
- TUI matching of exact repo, worktree, and branch context;
- absence of the TUI badge for broad, different-branch, and sibling-worktree
  filters;
- expiry removing the contextual badge;
- compact TUI mode retaining the contextual badge.

The existing repository-wide Go test suite and non-mutating lint and formatting
checks will validate integration with the generated client and surrounding CLI
and TUI behavior.

## Documentation

The command reference will document the new status section and JSON field. The
Agent Hook guide will explain where active snoozes are visible, and the
changelog will record the new observability behavior.

## Out of Scope

This change does not add TUI controls for creating or clearing snoozes, alter
snooze duration or scope, delete expired records, or annotate every job row.
It does not change review enqueueing or worker queue pause behavior.
