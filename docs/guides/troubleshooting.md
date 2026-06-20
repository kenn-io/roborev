---
title: Troubleshooting
description: Diagnose and fix common roborev issues
---

## Reviews Not Triggering

The most common issue is commits going unreviewed. Walk through these checks in order.

### Check the hook is installed

roborev uses a `post-commit` git hook to enqueue commits for review. Verify it exists:

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

The hook enqueues commits, but the daemon must be running to process the queue. Check with:

```bash
roborev status
```

If the daemon is stopped, start it:

```bash
roborev daemon start
```

`roborev init` starts the daemon automatically, but it won't survive a reboot unless you've set up a launchd/systemd service. If the daemon was running but reviews still aren't appearing, check the daemon log for errors:

```bash
roborev daemon run 2>&1 | head -50
```

### Post-commit hook log

roborev logs every post-commit hook invocation to `~/.roborev/post-commit.log` as JSONL. Each entry records a timestamp, the repository path, the outcome (`ok` or `error`), and a reason when the hook skips or fails. This is useful for diagnosing silent hook failures, especially in linked git worktrees where path resolution issues can prevent the hook from firing.

```bash
# View the last few hook invocations
tail -5 ~/.roborev/post-commit.log | jq .
```

### Mangled hook file

Other tools (Husky, lefthook, pre-commit, overcommit) can overwrite or corrupt the post-commit hook. Symptoms include:

- A stray `fi` with no matching `if`
- Missing `roborev enqueue` line
- The hook file containing only another tool's boilerplate

To diagnose, inspect the hook file and look for the `roborev enqueue` call. If it's missing or the file looks wrong, reinstall:

```bash
roborev install-hook --force
```

If your repo uses a hook manager, you may need to add the `roborev enqueue` call to your hook manager's post-commit configuration instead. See [Review Hooks](/guides/hooks/) for details on hook managers and `core.hooksPath`.

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

This is the "nuke it from orbit" option: it re-registers the repo with the daemon, reinstalls the hook, and restarts the daemon. Use this when you're not sure what's wrong and want a clean slate.

## See Also

- [Quick Start](/quickstart/): Initial setup and first review
- [CLI Commands](/commands/): Full command reference
- [Review Hooks](/guides/hooks/): Hook configuration and custom workflows
