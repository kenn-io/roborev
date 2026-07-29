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
- the decision to emit or defer a reminder.

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

`--agent <name>` selects one profile deterministically. `--agent all` selects
all eight profiles without checking installation. If automatic selection finds
none, the command fails with an actionable error that suggests an explicit
agent name or `--agent all`.

`--config` overrides the config path only when one explicit profile is
selected. The agent-specific `--codex-config` and `--claude-config` flags are
removed. `--command` is likewise restricted to one explicit profile because a
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

The stable ownership marker is `agent-hook run`. It matches entries created by
older roborev releases, so the first kit-backed installation replaces those
entries in place rather than leaving duplicate commands. The marker excludes
the executable path and therefore survives version-manager and binary-path
changes.

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
roborev handler.

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
output. For `PostToolUse`, it returns a `PostToolUseOutput` with additional
context when the daemon triggers a reminder. For `Stop`, it returns a
`StopOutput` with a blocking decision and the reminder reason. Kit translates
those types into each harness's supported native response.

## Delivery-Aware Triggering

Hermes can observe `PostToolUse` but cannot inject control output at that
boundary. Its control-capable boundary for roborev reminders is `Stop`.

Roborev therefore marks post-tool requests with whether that profile can
deliver a post-tool reminder. When delivery is unavailable, the state daemon
records commit and review state but does not consume the trigger, increment the
reminder count, or reset counters. The next `Stop` evaluates pending commit and
failed-review thresholds in addition to the normal stop threshold, emits the
pending reminder, and resets counters only after choosing that deliverable
response.

Profiles with post-tool context support keep the existing immediate reminder
behavior. Trigger priority at `Stop` is failed reviews, then commits, then the
turn threshold, so the most actionable reason is reported without emitting
multiple reminders at one boundary.

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
- fail-open handler behavior when the local daemon is unavailable; and
- deferred post-tool triggering followed by delivery and reset at `Stop`.

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
- describe native JSON or YAML dump output;
- retain stable-binary guidance; and
- explain that Hermes delivers post-tool triggers at the next stop boundary.

Quickstart checks continue to focus on the coding agents for which roborev
ships fix/refine skills. General hook installation and status output cover all
kit profiles; expanding roborev's skill packages is outside this change.

## Non-Goals

- Adding new agent profiles outside kit v0.14.0.
- Moving roborev's session state or review policy into kit.
- Replacing independently launched harness hooks with ACP or MCP.
- Adding project-scoped Factory Droid hook installation.
- Duplicating kit behavior to preserve old internal helper APIs.
