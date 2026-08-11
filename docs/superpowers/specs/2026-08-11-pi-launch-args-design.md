# Pi Launch Arguments Design

## Context

Pi can discover models from extensions installed in its normal configuration.
Roborev's isolated classifier invocation disables extension discovery and then
loads only the structured-output extension explicitly. A model supplied by a
different extension can therefore appear in `pi --list-models` while remaining
unavailable to the classifier.

Roborev currently supports a single Pi-specific structured-output extension
setting. Adding another setting tied to provider extensions would solve only
one instance of the broader need to pass user-selected Pi CLI arguments.

## Goal

Allow users to supply arbitrary, tokenized launch arguments for every Pi
invocation without requiring an executable wrapper. The feature must preserve
roborev's control over workflow, safety, model, reasoning, session, and output
arguments.

## Configuration

Add `launch_args` to the global `[agent.pi]` configuration:

```toml
[agent.pi]
launch_args = [
  "--extension",
  "npm:@example/pi-provider",
]
```

Each array entry is one argument passed directly to the Pi process. Roborev
does not perform shell parsing, interpolation, or word splitting. Shell
operators and a flag combined with its value in one string are therefore not
supported.

The setting applies to every built-in Pi workflow, including ordinary reviews,
agentic runs, resumed sessions, and structured-output classification. It is
global-only, matching the existing `[agent.pi]` and `[agent.codex]` settings.

## Argument Ordering

User launch arguments are copied into the argument list first. Roborev then
appends its managed arguments for the selected workflow. This follows the
precedence used for Codex config passthrough: when a CLI treats the last
occurrence of an option as authoritative, roborev's model, reasoning, session,
output, and safety choices win.

Repeatable additive options such as Pi's `--extension` remain effective even
when roborev subsequently adds `--no-extensions`; Pi defines that combination
to disable discovery while retaining explicitly supplied extensions.

Normal Pi runs may discover an extension that is also present in
`launch_args`. Pi 0.84.1 deduplicates the same installed resource when it is
also supplied explicitly, so the resulting model catalog contains one copy and
startup succeeds. Roborev tests should verify the produced argv rather than
assert this external Pi behavior.

## Implementation Shape

- Add `LaunchArgs []string` to `config.PiConfig`.
- Add an owned copy of the arguments to `agent.PiAgent` and preserve it through
  agent cloning and configuration overrides.
- Prepend the configured arguments in both the normal Pi argument builder and
  the classifier argument builder.
- Keep `pi_cmd` as an executable path or name; `launch_args` does not change
  command resolution.
- Keep `jsonschemaextension` as the dedicated structured-output extension
  setting.

No shared launch-argument abstraction will be added for other built-in agents
in this change. ACP agents already have their own `args` setting, while Codex
uses typed `-c` config passthrough. A cross-agent abstraction would broaden the
scope and require separate precedence rules for each CLI.

## Validation and Documentation

Tests will cover:

- loading `launch_args` from TOML;
- preserving the configured slice through Pi agent resolution and cloning;
- prepending arguments to normal, resumed-session, and classifier invocations;
- keeping roborev-managed arguments after user arguments; and
- leaving existing Pi behavior unchanged when `launch_args` is empty.

The agent and configuration documentation will describe the setting, tokenized
argument semantics, all-invocations scope, ordering, and the explicit-extension
use case. The changelog will record the new configuration option.

## Success Criteria

A Pi model registered by an explicitly supplied provider extension can be used
for structured-output classification while the classifier retains
`--no-extensions`. The same configured arguments appear on ordinary Pi
invocations, and existing configurations continue to produce the same command
line when `launch_args` is unset.
