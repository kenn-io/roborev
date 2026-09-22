---
title: Troubleshooting
description: Diagnose and fix common roborev issues
---

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

## Reviews Not Triggering

The most common issue is commits going unreviewed. Walk through these checks in
order.

### Check the hook is installed

roborev uses a `post-commit` git hook to enqueue commits for review. Verify it
exists:

```bash
# Resolves core.hooksPath (relative or absolute) against the main
# repo root, falling back to .git/hooks. Works from linked worktrees.
COMMON="$(git rev-parse --path-format=absolute --git-common-dir)"
HP="$(git config core.hooksPath || true)"
if [ -n "$HP" ]; then
  case "$HP" in /*) HOOKS="$HP" ;; *) HOOKS="${COMMON%/.git}/$HP" ;; esac
else
  HOOKS="$COMMON/hooks"
fi
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
reviews still aren't appearing, check the daemon log for errors:

```bash
roborev daemon run 2>&1 | head -50
```

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
