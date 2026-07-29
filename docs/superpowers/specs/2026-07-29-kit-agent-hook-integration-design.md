# Kit Agent Hook Integration Design

## Goal

Adopt `go.kenn.io/kit/agenthook` from kit v0.14.0 as roborev's common
coding-agent hook layer. Expose every harness profile supplied by kit while
keeping roborev responsible only for review-reminder policy, local session
state, and daemon lifecycle.

The supported harnesses are Claude Code, Codex, GitHub Copilot CLI, Cursor,
Factory Droid, Gemini CLI, Hermes Agent, and Qwen Code.

## Scope

This change replaces roborev's agent-specific hook installation, payload
decoding, and response encoding with kit. It expands the `agent-hook` CLI to all
eight kit profiles and makes the default install discover locally installed
harnesses.

The local `roborev-agent-hook` daemon, threshold configuration, failed-review
queries, commit detection, and session counters remain roborev-owned. The
change does not replace hook integrations with ACP: ACP controls sessions for
which roborev is the client, while these hooks steer sessions already running
inside an independently launched coding-agent harness.

## Success Criteria

- A default install selects every locally installed kit-supported harness and
  preserves unrelated native hook configuration.
- An explicit install can target any one of the eight profiles or all eight.
- Every profile receives a valid native response. Seven profiles receive a
  self-contained default reminder that can resolve roborev findings without a
  separately installed roborev skill; Cursor records events but emits no
  reminder because kit v0.14.0 exposes no Cursor control response at
  `PostToolUse` or `Stop`.
- Existing Codex, Claude Code, and Factory Droid hook entries are replaced on
  the next install instead of duplicated, while unrelated commands that happen
  to contain `agent-hook run` are preserved.
- Hermes delivers reminders with the repository and lineage that triggered
  them even if the session changes directories before its next `Stop`.
- Roborev contains no native harness config mutation, payload normalization,
  tool-name translation, or response encoding that kit already provides.

## Library Boundary

Kit owns:

- the supported profile registry and display metadata;
- default config-path discovery;
- native event and shell-tool mappings;
- command quoting for POSIX, Win32, and PowerShell;
- JSON and YAML config planning, installation, and ownership-safe replacement;
- native payload normalization into typed Claude-shaped events;
- typed handler dispatch; and
- harness-specific response encoding.

Roborev owns:

- selection of installed profiles for automatic installation;
- resolution of the stable roborev executable path;
- roborev's hook command and ownership marker;
- the user-level policy that rejects project-scoped Factory Droid hooks;
- threshold and instruction configuration;
- communication with the local hook daemon;
- commit, turn, and failed-review accounting; and
- the decision to emit, defer, or suppress a reminder based on profile
  capability.

No copied native config or response logic remains in roborev. The existing
Factory Droid path-safety policy remains because it is a roborev security
decision rather than a harness-format implementation.

## CLI Contract

### Install

`roborev agent-hook install` defaults to automatic selection. A profile is
selected when either its known CLI executable is available through
`os/exec.LookPath` or the parent directory of its kit-resolved config path
exists.

The executable candidates are:

| Profile | Executable candidates |
|---------|-----------------------|
| Claude Code | `claude` |
| Codex | `codex` |
| GitHub Copilot CLI | `copilot` |
| Cursor | `agent` |
| Factory Droid | `droid` |
| Gemini CLI | `gemini` |
| Hermes Agent | `hermes` |
| Qwen Code | `qwen` |

Config-directory detection honors environment-specific locations through
kit's `agenthook.ConfigPath` implementation.

The complete install and dump flag contract is:

| Flag | Install | Dump | New semantics and migration |
|------|---------|------|-----------------------------|
| `--agent` | Optional; empty means auto, a profile selects one, `all` selects eight | Required; exactly one profile | Existing explicit `codex`, `claude`, and `droid` values continue to work. |
| `--binary` | Retained for auto, one, or all | Not supported | Kit builds a profile-specific command from the resolved executable. |
| `--command` | One explicit profile only | One explicit profile only | Used verbatim after validation that it contains `agent-hook run` and selects the requested profile with `--agent`; it cannot be combined with `--binary`. |
| `--config` | One explicit profile only | One explicit profile only | Uniform replacement for agent-specific config flags. |
| `--codex-config` | Removed | Not supported | Replace with `--agent codex --config PATH`. |
| `--claude-config` | Removed | Not supported | Replace with `--agent claude --config PATH`. |
| `--scope` | Removed | Removed | Omit `--scope user`; project-scoped Factory Droid installation remains unsupported. |
| `--timeout` | Retained for auto, one, or all | Retained | Applies one duration to all three installed lifecycle hooks; bare integers remain seconds and kit converts to each profile's native unit. |
| `--dry-run` | Retained | Not supported | Plans every selected profile without writing. |

Removed flags fail through Cobra's normal unknown-flag error. Documentation
gives the replacement invocation rather than carrying deprecated aliases.

`--agent <name>` selects one profile deterministically. `--agent all` selects
all eight profiles without checking installation. If automatic selection finds
none, the command fails with an actionable error that suggests an explicit
agent name or `--agent all`.

`--config` overrides the config path only when one explicit profile is
selected. `--command` is likewise restricted to one explicit profile because a
raw command must identify the native profile passed to `agent-hook run`.
`--binary` remains valid for automatic, explicit, and all-profile installs;
kit builds the correctly quoted per-profile commands from the resolved binary.

Automatic and all-profile installs attempt every selected profile. Successful
profiles remain installed if another profile fails, and the command returns a
joined error describing every failure. `--dry-run` uses kit's planning API and
performs no writes.

Each generated command identifies its profile:

```text
roborev agent-hook run --agent <profile>
```

The stable ownership marker is `roborev`. Agent-hook config commands containing
that application namespace are roborev-owned, including entries created by
older roborev releases, so the first kit-backed installation replaces them in
place. A command owned by another application that happens to contain
`agent-hook run` is not removed. Explicit `--command` overrides must contain the
same marker; the marker is independent of a particular absolute binary path.

### Dump

`roborev agent-hook dump` requires one explicit profile. It uses kit's planning
API and writes the complete native config to stdout: JSON for JSON-backed
profiles and YAML for Hermes. Diagnostics and binary-resolution notices stay
on stderr so stdout remains suitable for declarative configuration systems.

The uniform `--config` flag supplies an existing config to merge into. The
command supports the same profile names, hook set, marker, command resolution,
and Factory Droid path policy as `install`.

### Run

`roborev agent-hook run` accepts every kit profile through `--agent`. Commands
written by the installer always include the flag. The command resolves the
profile through kit and passes stdin and stdout to `agenthook.Handle` with a
roborev handler. `--agent` is required; users upgrading an existing stable
binary installation run `roborev agent-hook install` once to replace old
commands that did not identify their profile. No permanent legacy parser or
profile guess is retained.

## Runtime Architecture

The runtime data flow is:

```text
native hook payload
    -> kit normalization and typed dispatch
    -> roborev lifecycle handler
    -> local roborev-agent-hook daemon
    -> typed roborev reminder output
    -> kit native response encoding
```

The roborev handler embeds `agenthook.NoopHandler` and implements
`PreToolUse`, `PostToolUse`, and `Stop`. It converts kit's normalized common
fields and raw tool input into roborev's internal daemon request. Shell tool
names are already normalized to kit's `ToolBash`, so roborev no longer needs
native tool-name aliases such as Factory Droid's `Execute`.

For `PreToolUse`, the handler records the baseline and returns an empty typed
output. For reminder-capable profiles, `PostToolUse` returns a
`PostToolUseOutput` with additional context when the daemon triggers a reminder,
and `Stop` returns a `StopOutput` with a blocking decision and the reminder
reason. Kit translates those types into each harness's supported native
response. Cursor sends the same request data and thresholds as every other
profile, so daemon accounting and trigger evaluation remain uniform, but the
handler always returns zero typed outputs because Cursor cannot encode control
output at either reminder boundary.

The default instruction is self-contained across all profiles:

```text
Resolve open roborev findings now. Use the roborev-fix skill if available;
otherwise run `roborev fix --open --list`, inspect each job with
`roborev show --job <id> --json`, fix and verify all findings, record each fix
with `roborev comment --commenter agent-hook --job <id> "<summary>"`, then run
`roborev close <id>` before continuing.
```

Existing configuration and environment overrides continue to replace this
instruction. Shipped skills remain the richer path for Claude Code, Codex, and
Factory Droid, but they are no longer a prerequisite for a useful reminder.

## Delivery-Aware Triggering

Hermes can observe `PostToolUse` but cannot inject control output at that
boundary. Its control-capable boundary for roborev reminders is `Stop`.

Roborev therefore marks post-tool requests with whether that profile can
deliver a post-tool reminder. When delivery is unavailable and a threshold is
reached, the state daemon persists a `PendingReminder` containing the rendered
reason, trigger kind, tracked repository, worktree, branch, HEAD, lineage key,
relevant counts, and creation time. The reason identifies the absolute
triggering worktree and tells the agent to change to it before running the
fallback roborev commands. The daemon reserves that trigger by advancing its
per-scope bookkeeping and resetting only the affected scope's counters, while
later activity begins a fresh counting window. The delivered-reminder count and
delivery timestamps do not advance until a control-capable boundary emits the
reminder.

Pending reminders are keyed by repository lineage and trigger kind within the
session. A repeated threshold crossing for an already-pending key coalesces
into that entry: it preserves the original creation time, refreshes repository
metadata and reason, and accumulates the relevant count. The next `Stop`
selects a pending reminder independently of the stop payload's CWD, emits its
stored reason, removes only that entry, and records delivery. Multiple pending
entries are ordered by trigger priority (failed reviews before commits), then
creation time, then key. Before delivery, a failed-review entry is revalidated
against its stored repository lineage; an entry whose reviews are no longer
actionable is discarded without incrementing delivery counters. Lower-priority
and other-repository entries remain pending for later stop boundaries; a normal
stop-threshold reminder is likewise left unconsumed when a pending reminder
wins.

Profiles with post-tool context support keep the existing immediate reminder
behavior. Trigger priority at any single boundary is failed reviews, then
commits, then the turn threshold, so only one reminder is emitted at a time.

Cursor sends normalized `PreToolUse`, `PostToolUse`, and `Stop` events to the
daemon with the same configured thresholds as every other profile. The daemon
therefore applies the same baseline, counters, deduplication, and trigger
accounting. The Cursor handler discards any triggered daemon response and emits
an empty native response because kit v0.14.0 has no Cursor control output at
either reminder boundary.

`PendingReminders` is an optional, `omitempty` field in the existing session
state. State written by older releases loads with no pending entries and needs
no migration or dual-read path. Once the upgraded daemon writes state, older
binaries are not expected to preserve the new pending queue; daemon and client
versions continue to roll forward together.

`roborev agent-hook status` exposes pending reminders as part of the existing
session state JSON. `roborev agent-hook reset SESSION_ID` and `reset --all`
clear them through the existing session-reset behavior. There is no separate
pending-reminder cleanup API or expiry clock: resolved failed-review entries are
discarded by delivery-time revalidation, and commit entries remain scoped to
and clearable with their owning session.

## Error Handling

Malformed input, an unsupported profile name, missing required normalized
fields, or an impossible response encoding is a normal CLI error. These cases
mean the harness invocation is invalid and cannot be safely interpreted.

Communication failure with the local hook daemon remains fail-open. The
handler writes a diagnostic to stderr, returns the zero typed output, and lets
kit encode the correct empty native response for the selected harness. A daemon
failure must never block or deny an agent action.

Installation and dump errors include the profile display name and config path
where available. Automatic detection treats an unreadable or unresolved config
path as not detected when no executable is present; an explicitly selected
profile surfaces the path error during planning or installation.

## Testing

Tests cover only roborev-owned behavior and its integration seam with kit:

- automatic selection by executable and by config directory;
- explicit single-profile and `all` selection;
- validation of uniform `--config`, raw `--command`, and no-agent cases;
- the hook specification, marker, and per-profile run arguments passed to kit;
- one real kit-backed install/run path proving the integration boundary;
- Cursor forwards the same configured thresholds as other profiles and emits
  empty responses even when the daemon reports a trigger;
- the Hermes handler marks post-tool requests for deferred delivery and
  propagates normalized event fields into daemon requests;
- fail-open handler behavior when the local daemon is unavailable; and
- deferred post-tool triggering followed by delivery at `Stop` after a CWD
  change and after a `git -C` command targets another repository;
- priority and retention when multiple repository lineages have pending
  reminders; and
- coalescing repeated same-lineage triggers while preserving creation order,
  and discarding resolved failed-review reminders before delivery;
- loading existing session state without a pending-reminder field.

Tests do not replicate kit's matrix of native config formats, event mappings,
command quoting, normalization, or response encodings. Existing tests whose
only subject is the deleted in-tree installer or response encoder are removed;
the compiler, diff, and remaining behavioral tests establish that the old
implementation is gone.

## Documentation

The README and Zensical documentation will:

- name all eight supported harnesses;
- describe automatic installed-agent detection;
- document explicit `--agent <name>` and `--agent all` behavior;
- replace agent-specific config flags with `--config`;
- include a command-by-command migration table for removed flags and the
  one-time reinstall required by old hook commands;
- describe native JSON or YAML dump output;
- retain stable-binary guidance; and
- explain Hermes's scoped, queued delivery at later stop boundaries; and
- explain that Cursor integration is tracking-only because its post-tool and
  stop hooks have no control response; and
- document the self-contained fallback workflow when no roborev skill exists.

Quickstart checks continue to focus on the coding agents for which roborev
ships fix/refine skills. General hook installation and status output cover all
kit profiles. Expanding roborev's skill packages is unnecessary for this change
because every reminder-capable profile receives the actionable fallback
instruction.

## Implementation Sequence

1. Upgrade `go.kenn.io/kit` to v0.14.0 and validate the public `agenthook` API
   seam without changing user-visible behavior.
2. Add the typed roborev handler and switch runtime normalization and response
   encoding to kit.
3. Add scoped pending reminders and their state tests before enabling Hermes
   post-tool deferral.
4. Replace install and dump config mutation with kit, add installed-profile
   detection, and enforce the complete CLI flag contract in the same atomic
   change as required profile dispatch.
5. Remove superseded native installer, detector, input-mapping, and response
   code after the kit-backed paths pass.
6. Update README and Zensical documentation, run package and repository quality
   gates, and commit the implementation in reviewable stages.

## Non-Goals

- Adding new agent profiles outside kit v0.14.0.
- Moving roborev's session state or review policy into kit.
- Replacing independently launched harness hooks with ACP or MCP.
- Adding project-scoped Factory Droid hook installation.
- Duplicating kit behavior to preserve old internal helper APIs.
