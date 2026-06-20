---
title: Agent Client Protocol (ACP)
description: Integrate any ACP-compatible agent with roborev
---

ACP is an [open protocol from Zed](https://zed.dev/blog/acp) for editor-to-agent communication over stdin/stdout JSON-RPC. roborev uses ACP to integrate agents that don't have built-in adapters. If an agent speaks ACP (or has a wrapper that does), you can plug it in.

## How It Works

roborev acts as the ACP **client**. The agent process is the ACP **server**.

1. roborev launches the agent command as a subprocess
2. Communication happens via stdin/stdout JSON-RPC
3. roborev negotiates a session mode and model with the agent
4. Prompts are sent, and the agent streams responses back
5. roborev enforces file access boundaries (read-only vs read-write) at the operation level

## Setup with Well-Known Agents

Several agents have ACP wrappers you can install directly.

| Agent | ACP Wrapper | Install |
|-------|-------------|---------|
| Codex | `codex-acp` | `npm install -g @zed-industries/codex-acp` |
| Claude Code | `claude-agent-acp` | `npm install -g @zed-industries/claude-agent-acp` |
| Gemini | `gemini --experimental-acp` | `npm install -g @google/gemini-cli` |

### Configuring a Wrapper

Set `command` to the wrapper binary in your ACP config:

```toml
# ~/.roborev/config.toml
[acp]
name = "codex-acp"
command = "codex-acp"
```

### Environment Variable Override

Override the ACP command for a single invocation without editing config files:

```bash
ROBOREV_ACP_ADAPTER_COMMAND=/opt/agents/my-custom-acp roborev review HEAD
```

### Verifying the Setup

Confirm the configured command is on your PATH, then run a review to test end-to-end communication:

```bash
which codex-acp          # or your configured command
roborev review HEAD
```

## Custom ACP Agents

Any binary that implements the ACP server protocol can be used. Set `command` to its path:

```toml
# ~/.roborev/config.toml
[acp]
name = "my-agent"
command = "/usr/local/bin/my-acp-agent"
args = ["--verbose"]
```

Once configured, the agent can be selected with `--agent my-agent`.

## Configuration Reference

Configure ACP in the `[acp]` section of `~/.roborev/config.toml`:

```toml
[acp]
name = "my-agent"                  # Agent name (required)
command = "/usr/local/bin/my-acp"  # ACP agent command (required)
args = ["--verbose"]               # Additional arguments
model = "my-model"                 # Default model
timeout = 600                      # Timeout in seconds (default: 600)
read_only_mode = "plan"            # Mode for review flows
auto_approve_mode = "auto-approve" # Mode for agentic flows
disable_mode_negotiation = false   # Skip SetSessionMode RPC
```

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `name` | string | (required) | Agent name used in `--agent` flag |
| `command` | string | (required) | Path or command for the ACP agent binary |
| `args` | array | `[]` | Additional CLI arguments passed to the agent |
| `model` | string | | Default model to request from the agent |
| `timeout` | int | `600` | Command timeout in seconds |
| `read_only_mode` | string | `"plan"` | Mode value sent for review (read-only) flows |
| `auto_approve_mode` | string | `"auto-approve"` | Mode value sent for agentic flows |
| `mode` | string | | Default agent mode (overrides `read_only_mode` unless explicitly opting in) |
| `disable_mode_negotiation` | bool | `false` | Skip ACP `SetSessionMode` RPC while keeping authorization behavior |

## Modes

roborev selects the ACP session mode automatically based on the workflow.

### Review Flow

Uses `read_only_mode` (default: `"plan"`). The agent can read files but cannot write or run commands. This is used during `roborev review` and `roborev run` (without `--agentic`).

### Fix/Refine Flow

Uses `auto_approve_mode` (default: `"auto-approve"`). The agent can edit files and run commands. This is used during `roborev refine` and `roborev run --agentic`.

### Disabling Mode Negotiation

Some agents don't support ACP session modes. Set `disable_mode_negotiation = true` to skip the `SetSessionMode` RPC call. roborev still enforces its own authorization boundaries (read-only vs read-write) regardless of this setting.

## Security

ACP agents run as subprocesses with the following guardrails:

- **Path validation**: File operations (reads, writes, edits) are validated against the repository root using symlink-aware path resolution. Terminal operations in read-write mode are not path-bounded and can execute arbitrary commands.
- **Mode enforcement**: Write and terminal operations are blocked at the operation boundary in read-only mode, independent of what the agent requests.
- **Bounded reads**: File reads are capped at 10 MB. Terminal output is capped at 1 MB.

## Troubleshooting

### "no Codex ACP wrapper command was found"

The ACP wrapper package is not installed. Install it:

```bash
npm install -g @zed-industries/codex-acp
```

### "mode X is not available"

The agent doesn't support the requested session mode. Check which modes the agent supports, or set `disable_mode_negotiation = true` in your `[acp]` config.

### "model X is not available"

The agent doesn't support the requested model. Remove the `model` field from your `[acp]` config, or check the agent's documentation for supported model names.

### "write operation not permitted in read-only mode"

This is expected during reviews. The agent attempted a write operation, but roborev blocked it because the task is running in review (read-only) mode. If you need file edits, use `roborev refine` or `roborev run --agentic`.

## See Also

- [Supported Agents](/agents/): Built-in agent adapters and auto-detection
- [Custom Tasks & Agentic Mode](/advanced/custom-tasks/): Review vs agentic modes
- [Configuration](/configuration/): Global and per-repo settings
