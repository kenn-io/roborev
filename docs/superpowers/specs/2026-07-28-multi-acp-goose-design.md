# Multiple ACP Agents and Goose Integration

## Goal

Allow roborev to configure and select more than one Agent Client Protocol
(ACP) agent, then prove the integration against Goose using its ChatGPT Codex
provider.

## Configuration

Replace the singleton `[acp]` configuration with named subtables. The subtable
key is the canonical roborev agent name and is not repeated inside the value:

```toml
[acp.goose]
command = "goose"
args = ["acp"]

[acp.foo]
command = "foo-acp"
```

The Go configuration types will represent `acp` as a map from agent name to
`ACPAgentConfig`. The old `name` field becomes unnecessary and will be removed.
The shipped singleton format will not gain a permanent compatibility adapter;
this change rolls configuration forward to the named-table format.

Global and repository ACP maps are merged by name. Repository entries replace
the complete matching global entry rather than merging individual fields. This
keeps overrides predictable while retaining unrelated global ACP agents.

Configuration loading will reject empty names, empty commands, and ACP names
that collide with built-in agent names. Quoted TOML keys can be used for names
that require them.

## Agent Resolution

All ACP lookup paths will resolve configuration using the selected agent name.
This includes:

- explicit agent selection;
- available-agent discovery;
- primary and backup workflow agents;
- workflow model selection and ACP model fallback;
- synthesis jobs; and
- repository overrides.

The existing bare `acp` built-in remains the generic, convention-based
`acp-agent` adapter. Named entries such as `goose` are distinct configured
agents.

Model resolution must use the selected ACP agent's configured model. A model
belonging to one named ACP agent must never leak into another agent during
primary, backup, or synthesis selection.

## Goose Installation and Authentication

Install Goose globally through mise's GitHub backend. Respect the user's mise
minimum-release-age policy rather than overriding it; at design time this means
Goose 1.43.0 even though 1.44.0 has just been released.

Inspect `goose --help` and `goose acp --help` after installation. Configure
Goose's `chatgpt-codex` provider through its OAuth flow, using the user's ChatGPT
subscription rather than an OpenAI API key. The Goose configuration remains the
source of truth for its default model, so the initial roborev entry will not set
`model`.

The live roborev configuration will receive:

```toml
[acp.goose]
command = "goose"
args = ["acp"]
```

Credentials and OAuth tokens must not be copied into roborev configuration,
repository files, logs, or commits.

## ACP Version Strategy

Roborev currently uses `github.com/coder/acp-go-sdk` v0.13.5. That is both the
latest release and identical to the SDK's main branch. Goose 1.44.0 uses ACP v1;
the official ACP v2 schema is still alpha.

Therefore this work will not fabricate a dependency upgrade or adopt ACP v2.
Compatibility will be established with a real Goose ACP session. If that test
exposes a concrete v1 protocol gap, the gap will be diagnosed and addressed
explicitly rather than preemptively replacing the SDK.

## Validation and Isolation

Unit tests will cover map decoding, validation, global/repository merging,
agent discovery, named selection, model isolation, backups, and synthesis.
Existing singleton tests and documentation examples will be migrated to named
tables.

The implementation will run targeted package tests followed by repository-wide
tests and non-mutating lint checks.

The Goose acceptance test will use a roborev binary built into scratch space and
a scratch `ROBOREV_DATA_DIR`; it will not replace the installed roborev binary
or use the live review database. Goose may read its intentionally configured
user-level OAuth state. The test will run a foreground, read-only review against
the current repository and record only behavioral success or sanitized errors.

## Documentation

Update the ACP guide, agent overview, configuration documentation, command
reference where necessary, and changelog. Examples will use `[acp.<name>]` and
will explain name-based repository overrides.

The ACP guide will include complete Goose examples for:

- installing the CLI through mise;
- authenticating the `chatgpt-codex` provider with a ChatGPT subscription;
- registering `goose acp` as `[acp.goose]`;
- selecting Goose for one review with `--agent goose`;
- choosing Goose as a workflow default or backup; and
- overriding the global Goose entry for one repository without discarding
  unrelated global ACP agents.

Examples must not include tokens, credential-file contents, machine-specific
paths, or a hard-coded model that can drift from Goose's configured default.
