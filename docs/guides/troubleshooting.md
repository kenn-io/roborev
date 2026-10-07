---
title: Troubleshooting
description: Diagnose and fix common roborev issues
---

## Start with roborev doctor

Run `roborev doctor` in the repository that has the problem. It checks config,
the daemon, agents, recent failures, git hooks, and review guidelines, and
prints a fix for each problem it finds. It only reads state, so it is safe to
run at any time. Use `roborev doctor --json` to hand the report to an agent.

A common cause of failed reviews is a daemon that cannot find an agent your
shell can run. This happens when the daemon started from a login item, a service
manager, or a shell with a different `PATH`. Doctor lists those agents under
`agents.daemon_path`; run `roborev daemon restart` from a shell where the agent
works.

See [Diagnosing Setup](/docs/commands/#diagnosing-setup) for the full list of
checks.

## Missing or unstructured reviews after upgrade

Version 0.68.0 archived historical reviews that it could not convert to JSON,
which removed them from normal views. Upgrade to 0.68.1 or later to restore
those reviews automatically. This is a forward migration; do not downgrade or
edit database rows to recover the history.

Existing valid JSON remains authoritative. Recognized Markdown becomes
structured findings. Other text returns as an **unstructured historical
review**, with its original Markdown, recorded verdict, and open/closed state.
These records remain readable in the CLI, TUI, and web app, searchable and
exportable, and usable by `roborev fix`. Unavailable finding counts do not mean
there were no findings.

To convert an unstructured record, give an agent the
[canonical agent migration guide in PR #1219](https://github.com/kenn-io/roborev/pull/1219#agent-migration-guide).
It covers SQLite backup and rehearsal, exporting unresolved records, preserving
findings and synthesis attribution, validated import or the live migration
endpoint, and read-back verification. The archive ID, active review ID, and job
ID are different numeric IDs; the guide identifies which each command needs.

Conversion is optional. Leave a record unstructured when the original text
cannot support a complete finding document. Roborev does not launch an agent or
invent missing findings during upgrade. See
[Review storage and legacy migration](/docs/guides/reviewing-code/#review-storage-and-legacy-migration)
for the storage behavior and deterministic conversion formats.

## High memory use during historical review migration

Version 0.68.2 runs this migration automatically, including when upgrading
directly from 0.67.0 or 0.68.0 without installing intermediate versions. If
migration already completed in 0.68.1, there are no reviews left to migrate.
Historical review archival, restoration, and verdict backfill process at most
100 reviews per batch. Archival and verdict updates commit each batch;
restoration saves each recovered review. Restarting after an interruption
preserves completed work and continues with the remaining records. Startup
restoration skips active reviews before loading their archived text and panel
sources. The explicit legacy export still returns the complete conversion input.

Startup logs identify each migration stage and report batch progress. The SQLite
job-ID migration also copies 100 jobs per statement, but keeps the table
replacement in one transaction. An interrupted table replacement rolls back and
restarts on the next launch. SQLite rebuilds each index as a single operation;
logs identify which index or trigger is being recreated.

## Reviews Not Triggering

The most common issue is commits going unreviewed. Walk through these checks in
order.

### Check the hook is installed

roborev uses a `post-commit` git hook to enqueue commits for review. Verify it
exists:

```bash
# Prints the hooks directory Git runs from this worktree,
# respecting core.hooksPath.
HOOKS="$(git rev-parse --path-format=absolute --git-path hooks)"
cat "$HOOKS/post-commit"
```

A healthy hook contains a `roborev enqueue` call. You should see something like:

```bash
#!/bin/sh
# roborev post-commit hook - auto-reviews every commit
roborev enqueue --quiet 2>/dev/null
```

If the file is missing, run `roborev install-hook` to create it.

### Check the daemon is running

The hook enqueues commits, but the daemon must be running to process the queue.
Check with:

```bash
roborev status
```

If the daemon is stopped, start it:

```bash
roborev daemon start
```

`roborev init` starts the daemon automatically, but it won't survive a reboot
unless you've set up a launchd/systemd service. If the daemon was running but
reviews still aren't appearing, read the saved daemon diagnostics:

```bash
tail -50 "${ROBOREV_DATA_DIR:-$HOME/.roborev}/daemon.log"
```

Both foreground and detached daemons write standard timestamped diagnostics to
`daemon.log` in the data directory and continue writing to stderr. The active
file and its one previous rotation, `daemon.log.1`, each hold at most 10 MiB. A
record larger than that limit is marked `[truncated]` in the file; stderr
retains the complete record. The detached launcher also captures stdout and
stderr under the data directory's `logs/` directory for startup diagnostics.

Only one daemon can own these log files in a data directory, even when `--db`
selects different databases. A second startup exits before changing the running
daemon's logs. Use a separate `ROBOREV_DATA_DIR` for each daemon when running
multiple instances.

### CI repository discovery and polling failures

GitHub reads retry transient HTTP errors, interrupted response bodies, network
interruptions, and request timeouts up to four total attempts. Retries use
exponential backoff with jitter, starting at one second with an eight-second
maximum interval. Server `Retry-After` delays take precedence. A two-minute
elapsed budget limits scheduling the next attempt; it does not interrupt an
attempt already running. Each attempt keeps the HTTP client's timeout, and
caller cancellation stops attempts and waits. A server-required delay beyond the
remaining budget surfaces the failure without retrying early. Writes such as
comments and commit statuses are not retried.

Retry messages appear in `daemon.log`, including transient failures that
recover. If all attempts fail, CI health and `errors.log` retain the failed
operation and a safe category, such as `list pull requests: timeout` or
`list organization repositories: HTTP 503`. Provider response text and
credentials are excluded from these summaries. Other failure details remain in
the daemon log. The next successful poll clears the current failure and
preserves error history; discovery continues to use previously known
repositories when available.

Check the operation and category before changing configuration. For HTTP 401 or
403, check the CI credential and its repository permissions. For rate limits,
allow the server's reset interval to pass. For timeouts, interrupted reads, or
HTTP 5xx responses, check GitHub availability and the daemon's network access. A
persistent failure stays unhealthy until that operation succeeds.

### Post-commit hook log

roborev logs every post-commit hook invocation to `~/.roborev/post-commit.log`
as JSONL. Each entry records a timestamp, the repository path, the outcome (`ok`
or `error`), and a reason when the hook skips or fails. This is useful for
diagnosing silent hook failures, especially in linked git worktrees where path
resolution issues can prevent the hook from firing.

```bash
# View the last few hook invocations
tail -5 ~/.roborev/post-commit.log | jq .
```

### Mangled hook file

Other tools (Husky, lefthook, pre-commit, overcommit) can overwrite or corrupt
the post-commit hook. Symptoms include:

- A stray `fi` with no matching `if`
- Missing `roborev enqueue` line
- The hook file containing only another tool's boilerplate

To diagnose, inspect the hook file and look for the `roborev enqueue` call. If
it's missing or the file looks wrong, reinstall:

```bash
roborev install-hook --force
```

If your repo uses a hook manager, you may need to add the `roborev enqueue` call
to your hook manager's post-commit configuration instead. See
[Review Hooks](/docs/guides/hooks/) for details on hook managers and
`core.hooksPath`.

### The nuclear option

If the above steps don't resolve the issue, reset everything:

**Replace just the hook** with a known-good version:

```bash
roborev install-hook --force
```

This overwrites the existing post-commit hook entirely.

**Full reset:** re-initialize the repo, daemon, and hook from scratch:

```bash
roborev init --force
```

This is the "nuke it from orbit" option: it re-registers the repo with the
daemon, reinstalls the hook, and restarts the daemon. Use this when you're not
sure what's wrong and want a clean slate.

## Automation works in Claude Code CLI but not Claude Desktop

The agent hook (mid-session fix nudges) relies on harness hooks (`PreToolUse` /
`PostToolUse` / `Stop`) that the Claude Code CLI and Codex expose. Claude
Desktop does not expose these hooks, so the agent-hook layer does not run there.
Post-commit reviews still work in any environment - check `roborev status` and
`roborev show HEAD` to confirm reviews are running.

## See Also

- [Quick Start](/docs/quickstart/): Initial setup and first review
- [CLI Commands](/docs/commands/): Full command reference
- [Review Hooks](/docs/guides/hooks/): Hook configuration and custom workflows
