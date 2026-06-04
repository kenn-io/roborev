# Agent Hook Integration Design

## Status

Draft for the `agent-hook-integration` branch. Commit `92125941` already
contains a command-local prototype that passes `go test ./...`, `go vet ./...`,
and `go build ./...`. The remaining work is mostly relocation, parity coverage,
native config, docs, and latency/contract hardening.

## Context

`roborev-hook` gives Codex and Claude Code sessions a review reflex: agent harness hooks call a small command on `Stop` and `PostToolUse`, the command records session state, checks roborev for open failed reviews, and returns an instruction to run `$roborev-fix` when configured thresholds are met.

This should become an optional roborev feature rather than a separate product. The integration should live in the roborev binary, but it should not run inside the main roborev daemon process. Agent hooks are a noisy edge: they parse external payloads, run frequently, inspect shell/git state, and should never be able to crash or wedge the core review daemon.

## Goals

- Ship `roborev agent-hook` as an optional harness integration for Codex and Claude Code.
- Keep the hook state service separate from the main roborev daemon process.
- Move reusable implementation out of `cmd/roborev` into `internal/agenthook`.
- Reuse roborev's existing `go.kenn.io/kit` dependency for daemon lifecycle
  and git subprocess helpers where it fits; avoid unrelated kit migration work.
- Fail open from agent hook execution when local runtime dependencies are unavailable: if the local hook daemon cannot be reached or started, emit `{}` and log a diagnostic.
- Soft-fail roborev daemon lookup: counters may update, but no fix prompt should be returned unless roborev reports actionable failed reviews.
- Keep tests isolated from production `~/.roborev`, `~/.config/git`, live daemon runtimes, and user hooks.

## Non-Goals

- Do not merge hook state into the main roborev daemon.
- Do not replace roborev's existing git post-commit hook system.
- Do not ship a second `roborev-hook` binary from the roborev repo.
- Do not mutate user Codex or Claude configs unless the user runs `roborev agent-hook install`.

## User Interface

The user-facing command group is:

```text
roborev agent-hook install       # install Codex and/or Claude hook entries
roborev agent-hook dump          # print declarative hook config JSON
roborev agent-hook run           # command invoked by agent harness hooks
roborev agent-hook daemon        # local hook state daemon, auto-started by run
roborev agent-hook status        # inspect local hook session state
roborev agent-hook reset         # reset one/all local hook sessions
```

`install` should default to both Codex and Claude, preserve existing hooks, and be idempotent. `dump` should support declarative setups such as Nix/home-manager by printing the same config shape without writing files.

The installed command should be the current roborev executable plus `agent-hook run`.

## Configuration

Config should be native roborev TOML, not a sibling JSON file next to the binary. `roborev-hook` was a spike with no production users or compatibility contract, so remove the prototype's inherited `agent-hook.json` loader outright.

Proposed global config:

```toml
[agent_hook]
turn_threshold = 5
commit_threshold = 0
failed_review_threshold = 4
instruction = "Invoke the $roborev-fix skill now."
```

Resolution order:

```text
command flags > environment variables > global roborev config > defaults
```

Environment variables should use the roborev-native prefix:

```text
ROBOREV_AGENT_HOOK_TURN_THRESHOLD
ROBOREV_AGENT_HOOK_COMMIT_THRESHOLD
ROBOREV_AGENT_HOOK_FAILED_REVIEW_THRESHOLD
ROBOREV_AGENT_HOOK_INSTRUCTION
ROBOREV_AGENT_HOOK_ROBOREV_ADDR
ROBOREV_AGENT_HOOK_DAEMON_ADDR
```

Do not support legacy `ROBOREV_HOOK_*` aliases. The standalone `roborev-hook` spike has no production users, so adding aliases now creates unused compatibility surface.

`roborev_server_addr` and the agent-hook daemon address should stay as flags/env-only operational overrides. Do not add them to TOML unless a real user workflow needs persistent endpoint pinning.

## Architecture

Implementation should be split into a reusable internal package and thin Cobra glue.

```text
cmd/roborev/agent_hook_cmd.go
  Cobra commands, CLI I/O, flag binding.

internal/procutil/env.go
  Shared process-launch helpers currently in cmd/roborev/env.go, including
  git environment filtering and ephemeral binary auto-start guards.

internal/agenthook/config.go
  Option resolution from flags/env/global config/defaults.

internal/agenthook/install.go
  Codex and Claude hook config generation, install, dump, idempotent merging.

internal/agenthook/client.go
  Short-lived hook process client, local daemon discovery, fail-open helper.

internal/agenthook/daemon.go
  Local state daemon HTTP server and runtime record management.

internal/agenthook/state.go
  Session counters, trigger logic, persisted state.

internal/agenthook/git.go
  Git signal detection: repo root, branch, HEAD, commit-producing command checks.

internal/agenthook/roborev.go
  Main roborev daemon query for open failed reviews.

internal/agenthook/types.go
  Hook payloads, responses, session state, public options.
```

The main roborev daemon remains the source of review/job truth. The agent-hook daemon owns only ephemeral session counters and last-seen state.

The command-local prototype currently calls `filterGitEnv` and `shouldRefuseAutoStartDaemon` from `cmd/roborev/env.go`. Moving client/runtime code into `internal/agenthook` requires extracting those helpers into a small shared internal package first; otherwise the refactor will create an import cycle or fail to compile.

## Process Model

1. Codex/Claude invokes `roborev agent-hook run` with hook JSON on stdin.
2. `run` decodes payload and tries to reach the local agent-hook daemon.
3. If the local daemon is absent, `run` auto-starts `roborev agent-hook daemon`.
4. `run` posts the hook event to the local daemon.
5. The local daemon updates per-session counters, inspects git state, and queries the main roborev daemon for open failed reviews.
6. The local daemon returns either no action or a trigger response.
7. `run` maps the trigger response into the agent harness output format.

Failure policy:

- Local agent-hook daemon unavailable: `run` emits `{}` and logs to stderr.
- Main roborev daemon unavailable: failed-review count is unknown; do not prompt.
- Git unavailable/not a roborev repo: skip and emit `{}`.
- Malformed hook stdin or missing `session_id`: return a normal CLI error. This is an invalid harness invocation, not a transient runtime dependency failure.
- Installer/dump invalid config: return a normal CLI error.

Latency policy:

- `agent-hook run` is in the agent hot path, so it needs an explicit end-to-end timeout budget.
- First-call daemon startup may be slower than steady state, but it must be bounded and documented.
- Avoid expensive work on frequent `PostToolUse` events where possible. If daemon startup proves too costly there, prefer only auto-starting on `Stop` and returning `{}` for `PostToolUse` until the hook daemon is already running.

## State

Persist local hook state under the roborev data dir:

```text
${ROBOREV_DATA_DIR:-~/.roborev}/agent-hook/state.json
${ROBOREV_DATA_DIR:-~/.roborev}/agent-hook/runtime/daemon.<pid>.json
${ROBOREV_DATA_DIR:-~/.roborev}/agent-hook/daemon.log
```

The state file tracks session counters and repo heads. It should be written atomically.

Runtime files must identify the service as `roborev-agent-hook`, not `roborev`, so discovery cannot confuse the hook daemon with the main daemon.

## Trigger Behavior

The port should preserve `roborev-hook` behavior:

- `Stop` hooks increment turn counters.
- `PostToolUse` hooks only matter for Bash commands.
- Commit-producing commands include `git commit`, `git cherry-pick`, and `git revert`.
- Turn and commit thresholds prompt only when roborev reports at least one open failed review.
- Failed-review threshold can prompt independently when the open failed review count increases by at least the configured threshold.
- Failed-review SQL filters are scoped to the current branch and include older jobs with empty branch, matching `roborev fix` discovery.
- The hook currently treats API jobs with `Verdict == "F"` as actionable. This is deliberately conservative and can under-count compared with `roborev fix`, which evaluates each review through `jobVerdict(job, review)` and acts on everything that is not passing. Either keep this conservative proxy and test/document the relationship, or add a server-side actionable-count endpoint shared by both hook and fix.
- Prompt counters reset after any trigger.
- `Stop` recursion (`stop_hook_active`) skips without incrementing.

## Test Strategy

Tests must cover:

- CLI command registration and help.
- Hook output mapping for Stop and PostToolUse.
- Per-agent output contract verification for Codex and Claude. The installed hook config is not enough; tests must pin the output shape each harness accepts.
- Fail-open behavior when local hook daemon is unavailable.
- Hard-error behavior for malformed hook stdin and missing `session_id`.
- Installer/dump parity for Codex and Claude.
- Idempotent hook install and existing timeout update.
- Symlinked config write-through behavior.
- Config resolution from flags/env/global config/defaults.
- State persistence and reset/status routes.
- Stop threshold trigger behavior.
- Commit counting and command detection.
- Failed-review threshold behavior.
- Conservative failed-review counting behavior relative to `roborev fix`, including jobs with empty verdict/output.
- Branch-scoped failed-review query params.
- Latency budget behavior for local daemon startup/probe/query timeouts.
- Production isolation: tests set `HOME`, `USERPROFILE`, `XDG_CONFIG_HOME`, and `ROBOREV_DATA_DIR` to temp dirs and never use live roborev runtime files unless explicitly mocked.

## Ship Criteria

- `go test ./...` passes.
- `go vet ./...` passes.
- `go build ./...` passes.
- `roborev agent-hook dump --agent codex` and `--agent claude` produce expected hook config.
- `roborev agent-hook install --dry-run` reports changes without writes.
- Hook execution fails open when the local hook daemon cannot start, while malformed harness payloads return clear CLI errors.
- The hot path has an explicit timeout budget and no unbounded daemon/query wait.
- Agent-hook lifecycle and git subprocess behavior uses roborev's existing kit
  helpers instead of local copies.
- `cmd/roborev` contains only thin `agent-hook` Cobra glue; reusable logic lives in `internal/agenthook`.
- Docs explain that agent-hook is optional and separate from the main roborev daemon.

## Resolved Decisions

- Do not support legacy `ROBOREV_HOOK_*` env aliases in roborev.
- Do not keep the adjacent JSON config fallback in roborev.
- Keep `roborev agent-hook install` as an explicit optional setup step for the first release. `roborev init` may mention it later, but should not install agent harness hooks by default.
