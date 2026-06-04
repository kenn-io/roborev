# Agent Hook

`roborev agent-hook` is an optional Codex and Claude Code harness integration.
It lets an interactive agent session ask roborev whether it should run
`$roborev-fix` without replacing roborev's normal git post-commit hook.

The integration is explicit opt-in:

```bash
roborev agent-hook install
```

By default this installs hook entries for both Codex and Claude Code. Use
`--agent codex` or `--agent claude` to update only one harness. Existing hook
entries are preserved, and repeated installs are idempotent.

For declarative setups, print the JSON to manage yourself:

```bash
roborev agent-hook dump --agent codex
roborev agent-hook dump --agent claude
```

## Runtime Model

Agent harnesses invoke:

```bash
roborev agent-hook run
```

`run` reads a hook payload on stdin, talks to a small local
`roborev-agent-hook` daemon, and emits the hook response JSON expected by the
harness. The hook daemon is separate from the main roborev daemon and stores
only local session counters under:

```text
${ROBOREV_DATA_DIR:-~/.roborev}/agent-hook/
```

The main roborev daemon remains the source of review/job truth. If the local
agent-hook daemon cannot be reached or started, `run` fails open by emitting
`{}` and logging the diagnostic to stderr. Malformed hook input and missing
`session_id` are treated as invalid harness invocations and return a normal CLI
error.

`run` auto-starts the daemon on demand, so manual management is rarely needed.
When it is, the daemon is managed like the main roborev daemon:

```bash
roborev agent-hook daemon start    # start the daemon (no-op if already running)
roborev agent-hook daemon status   # print running daemons as JSON
roborev agent-hook daemon stop     # stop the daemon
roborev agent-hook daemon restart  # replace the daemon with the caller's binary
```

`status` reports each live daemon's PID, version, address, and reachability.
Inspect tracked session counters (including `remind_count`, the number of
`$roborev-fix` reminders emitted) with `roborev agent-hook status`.

## Configuration

Configure thresholds in the global roborev config file
`${ROBOREV_DATA_DIR:-~/.roborev}/config.toml`:

```toml
[agent_hook]
turn_threshold = 5
commit_threshold = 0
failed_review_threshold = 4
instruction = "Invoke the $roborev-fix skill now."
```

`turn_threshold` controls Stop hook prompting; `0` disables it.
`commit_threshold` controls Bash `PostToolUse` prompting after commit-producing
commands; `0` disables it. `failed_review_threshold` controls prompting when
open failed roborev reviews are visible for the current repo/branch; `0`
disables it. Commit baselines are tracked per repo and branch, so switching
branches does not carry a stale commit count.

Command flags override environment variables, which override TOML config:

```text
ROBOREV_AGENT_HOOK_TURN_THRESHOLD
ROBOREV_AGENT_HOOK_COMMIT_THRESHOLD
ROBOREV_AGENT_HOOK_FAILED_REVIEW_THRESHOLD
ROBOREV_AGENT_HOOK_INSTRUCTION
ROBOREV_AGENT_HOOK_ROBOREV_ADDR
ROBOREV_AGENT_HOOK_DAEMON_ADDR
```

`ROBOREV_AGENT_HOOK_ROBOREV_ADDR` and `ROBOREV_AGENT_HOOK_DAEMON_ADDR` are
operational overrides only; they are not persisted in TOML.
