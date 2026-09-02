---
title: Supported Agents
description: AI agents supported by roborev
---

roborev supports multiple AI coding agents and auto-detects which ones are
installed.

## Supported Agents

| Agent | CLI Command | Install |
|-------|-------------|---------|
| Codex | `codex` | `npm install -g @openai/codex` |
| Claude Code | `claude` | `npm install -g @anthropic-ai/claude-code` |
| Gemini | `agy` or `gemini` | `curl -fsSL https://antigravity.google/cli/install.sh \| bash` (preferred) or `npm install -g @google/gemini-cli` |
| Copilot | `copilot` | `npm install -g @github/copilot` |
| Cursor | `agent` | See [cursor.com/cli](https://cursor.com/cli) |
| OpenCode | `opencode` | `npm install -g opencode-ai@latest` ([anomalyco/opencode](https://github.com/anomalyco/opencode)) |
| Droid | `droid` | See [factory.ai](https://factory.ai/) |
| Kilo | `kilo` | `npm install -g @kilocode/cli` |
| Kiro | `kiro-cli` | See [kiro.dev](https://kiro.dev/) |
| Pi | `pi` | `npm install -g @mariozechner/pi-coding-agent` |
| Grok Build | `grok` | `curl -fsSL https://x.ai/cli/install.sh \| bash` ([x.ai/cli](https://x.ai/cli)) |

## Auto-Detection

roborev auto-detects installed agents and falls back in this order:

1. Codex
1. Claude Code
1. Gemini
1. Copilot
1. OpenCode
1. Cursor
1. Kiro
1. Kilo
1. Droid
1. Pi
1. Grok Build

The first available agent is used unless you specify one explicitly.

## Specifying an Agent

### Per-Command

```bash
roborev review --agent claude-code <sha>
roborev run --agent codex "Explain this code"
roborev refine --agent gemini
```

### Per-Repository

```toml
# .roborev.toml
agent = "claude-code"
```

### Global Default

```toml
# ~/.roborev/config.toml
default_agent = "codex"
```

## Model Selection

You can override the default model for any agent using the `--model` / `-m`
flag:

```bash
roborev review --model gpt-4.1 <sha>
roborev refine --model claude-sonnet-4-20250514
```

### Model Format by Agent

| Agent | Model Format | Example |
|-------|--------------|---------|
| Codex | OpenAI model name | `gpt-4.1`, `o3-mini` |
| Claude Code | Anthropic model name | `claude-sonnet-4-20250514`, `claude-opus-4-20250514` |
| Gemini | Google model name | `gemini-2.5-pro`, `gemini-2.5-flash` |
| Copilot | OpenAI model name | `gpt-4.1` |
| Cursor | Model name | `claude-sonnet-4-20250514`, `gpt-4.1` |
| OpenCode | `provider/model` | `anthropic/claude-sonnet-4-20250514`, `openai/gpt-4.1` |
| Droid | Factory model name | (see Factory.ai docs) |
| Kilo | `provider/model` | `anthropic/claude-sonnet-4-20250514`, `openai/gpt-4.1` |
| Kiro | Model name | (see Kiro docs) |
| Pi | Model name | `claude-sonnet-4-20250514`, `gpt-4.1` |
| Grok Build | xAI model ID | `grok-4.5` (see [x.ai/cli](https://x.ai/cli)) |

### Configuration

Set a default model globally or per-repository:

```toml
# ~/.roborev/config.toml
default_model = "claude-sonnet-4-20250514"
```

```toml
# .roborev.toml
model = "gpt-4.1"  # Override for this repo
```

Model resolution priority: CLI flag > per-repo config > global config > agent
default.

## Routing Claude Code to a Proxy

The `claude-code` agent accepts a model spec of the form `<model>@<base_url>`.
When `<base_url>` starts with `http://` or `https://`, roborev points Claude
Code at that endpoint and pins all tier aliases (Opus, Sonnet, Haiku, subagent)
to the given model. This lets you use local runtimes (Ollama, LM Studio) or
gateways (LiteLLM, OpenRouter) that expose an Anthropic-compatible API.

```toml
# .roborev.toml: local Ollama for reviews, real Anthropic for fixes
agent = "claude-code"
review_model = "glm-5.1:cloud@http://127.0.0.1:11434"
fix_model    = "sonnet"
```

Or per invocation:

```bash
roborev review --model 'glm-5.1:cloud@http://127.0.0.1:11434'
```

A bare proxy spec (`@http://...` with no model) is rejected with an error. The
full URL (including any path or query string) is forwarded as-is to
`ANTHROPIC_BASE_URL`, so include the path your gateway expects. For example,
LiteLLM typically wants a trailing `/v1`, while Ollama wants no path.

### Proxy Authentication

Set `ROBOREV_CLAUDE_PROXY_TOKEN` in your environment to forward a bearer token
to the proxy as `ANTHROPIC_AUTH_TOKEN`. If unset, roborev sends a placeholder
token, which is sufficient for gateways that do not validate the header (such as
Ollama).

roborev does not forward `anthropic_api_key` (or `ANTHROPIC_API_KEY`) to proxy
endpoints. Doing so would leak a real Anthropic credential to arbitrary third
parties.

### URL Restrictions

- Proxy URLs must not embed `user:pass@` credentials. Use
    `ROBOREV_CLAUDE_PROXY_TOKEN` instead.
- `http://` is only accepted for loopback hosts (`127.0.0.1`, `::1`,
    `localhost`), so plaintext tokens can't be sent over the wire. Use
    `https://` for remote proxies.

### Environment Behavior

!!! warning

    As of 0.52, the `claude-code` agent always strips the following variables from
    the child process environment: `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`,
    `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_DEFAULT_OPUS_MODEL`,
    `ANTHROPIC_DEFAULT_SONNET_MODEL`, `ANTHROPIC_DEFAULT_HAIKU_MODEL`, and
    `CLAUDE_CODE_SUBAGENT_MODEL`. If you previously routed Claude Code by exporting
    these in your shell, switch to the `<model>@<base_url>` spec, or configure
    `anthropic_api_key` in `~/.roborev/config.toml` for native (non-proxy) mode.
    roborev re-injects the configured key rather than inheriting from the operator's
    shell.

## Gemini: Antigravity vs Legacy CLI

The Gemini agent works with either the Antigravity `agy` CLI or the legacy
`gemini` CLI. Google has deprecated the legacy CLI, so roborev prefers `agy`
when both are installed and falls back to `gemini` otherwise.

```bash
# Preferred: Antigravity CLI
curl -fsSL https://antigravity.google/cli/install.sh | bash

# Legacy CLI (still supported)
npm install -g @google/gemini-cli
```

Antigravity runs in print mode. Roborev probes `agy --version` and checks the
prompt size to choose the transport. Version 1.1.1 and newer receive prompts
through `--prompt` when the text fits within the platform's command-line size
limit. Other prompts use standard input, with a bare `--print` flag on older
versions. Review jobs omit both `--sandbox` and
`--dangerously-skip-permissions`; agentic jobs use
`--dangerously-skip-permissions`. Omitting `--sandbox` is intentional because
agy's print-mode sandbox can deny the read-only workspace probes that reviews
need.

Since
[agy 1.1.3](https://github.com/google-antigravity/antigravity-cli/releases/tag/1.1.3),
print mode soft-denies tool calls that would need a permission confirmation
instead of silently auto-approving them. A review can hit this on the model's
first tool call, usually a file read or terminal command. In that case, `agy`
exits successfully without producing a review. Roborev marks the job failed,
retries it, then uses a configured backup agent if one is available. Before each
non-agentic review, roborev automatically merges the required allow-rules into
`~/.gemini/antigravity-cli/settings.json`:

```json
{
  "permissions": {
    "allow": [
      "read_file(*)",
      "command(pwd)",
      "command(wc)",
      "command(ls)",
      "command(cat)",
      "command(head)",
      "command(tail)",
      "command(stat)",
      "command(file)"
    ]
  }
}
```

Roborev preserves existing settings and allow entries, appending only missing
rules. `read_file(*)` is the rule that matters: agy's native read tools (view
file, search, list directory) do the real work of a review once file reads are
allowed. The `command` rules keep inspect commands from aborting the run.

These rules apply to every `agy` session. Command targets use prefix matching,
as described in the
[agy permissions docs](https://antigravity.google/docs/cli-permissions). Treat
any `command(...)` rule as broad shell authority rather than a read-only grant.
The `read_file(*)` rule also applies globally and auto-approves reads of any
path on the system. Narrow its target to your source roots if necessary. Review
jobs run without agy's OS-level `--sandbox`, so these command permissions are
not confined to agy's scratch directory. Avoid `command(*)` and
`unsandboxed(*)`, which broaden permission scope further. Agentic jobs are
unaffected by this merge because `--dangerously-skip-permissions` auto-approves
all tools.

Roborev does not currently provide an opt-out for the automatic settings merge
when using `agy`. To avoid Antigravity's global settings and unsandboxed review
path, pin the legacy CLI with `gemini_cmd = "gemini"` in your config, or choose
another agent.

Antigravity does not currently accept a `--model` flag, so an explicit `--model`
returns an error whenever the Gemini agent resolves to `agy` — even when the
legacy `gemini` CLI is also installed — rather than silently ignoring the
override.

If you rely on model selection, pin the legacy CLI with `gemini_cmd = "gemini"`
in your config, install only the legacy CLI, or shadow `agy` on your `PATH` with
a wrapper that exec's `gemini`.

## Pi Structured Output

Pi can run normal review jobs and can also serve as the auto design-review
classifier. roborev uses Pi's JSON schema output extension for classifier jobs.
The default extension source is `npm:@nqbao/pi-json-schema@0.1.1`.

Install the default extension in Pi:

```bash
pi install npm:@nqbao/pi-json-schema
```

roborev still passes the configured extension source explicitly when it invokes
classifier jobs. Installing it in Pi makes setup visible in `pi list` and avoids
runtime package-fetch surprises in offline or locked-down environments.

Override the extension source in global config if you vendor or mirror it:

```toml
[agent.pi]
jsonschemaextension = "/opt/roborev/pi-json-schema/index.ts"
```

If the selected Pi model is registered by another extension, pass that extension
explicitly on every Pi launch:

```toml
[agent.pi]
launch_args = ["--extension", "npm:@example/pi-provider"]
```

See [Pi Classifier Options](/configuration/#pi-classifier-options) for argument
ordering and tokenization details.

## Agentic Support

Different agents have different levels of support for agentic mode (file edits
and commands):

| Agent | Agentic Support |
|-------|-----------------|
| Codex | Full (uses `--dangerously-bypass-approvals-and-sandbox`) |
| Claude Code | Full (uses `--dangerously-skip-permissions`) |
| Gemini (Antigravity) | Full (uses `--dangerously-skip-permissions`) |
| Gemini (legacy) | Full (uses `--yolo` and `--allowed-tools`) |
| Copilot | Limited (requires manual approval for actions) |
| Cursor | Full (uses `--yolo` flag) |
| OpenCode | Full (auto-approves in non-interactive mode) |
| Droid | Full (runs autonomously) |
| Kilo | Full (runs autonomously) |
| Kiro | Full (uses `--trust-all-tools`) |
| Pi | Full (agentic runs use Pi's default tools) |
| Grok Build | Full (uses `--always-approve` in agentic mode; review uses layered read-only safety — see below) |

See [Custom Tasks & Agentic Mode](/advanced/custom-tasks/) for details on review
vs agentic modes.

## Pi

Non-agentic Pi reviews use a positive read-only tool allowlist:

```bash
pi -p --mode json --tools read,grep,find,ls
```

Structured reviews also allow the `json_output` tool registered by the
configured JSON schema extension. They cannot run commands or change files.
Agentic jobs and runs with `allow_unsafe_agents = true` omit the allowlist and
use Pi's default tools, including `bash`, `edit`, and `write`.

Roborev appends the managed allowlist after `[agent.pi].launch_args`, so a
duplicate `--tools` option in global configuration does not replace the review
restriction.

## Grok Build

[Grok Build](https://x.ai/cli) (`grok`) is xAI's coding agent
([source](https://github.com/xai-org/grok-build)). roborev drives it in
**headless mode** — the same one-shot CommandAgent pattern as Claude Code and
Codex — not the long-lived ACP stdio server.

```bash
# Install
curl -fsSL https://x.ai/cli/install.sh | bash
grok login   # or set XAI_API_KEY

# Use as a first-class agent
roborev review --agent grok HEAD
roborev config set agent grok
```

Alias `grok-build` resolves to `grok`. Override the binary path with `grok_cmd`
in config when `grok` is not on `PATH`.

### Review vs agentic launch lines

Non-agentic review is **layered**, not absolute "all tools disabled":

1. `--sandbox read-only` — OS-level filesystem/network sandbox
1. `--tools read_file,grep,list_dir` — positive built-in allowlist
1. `--disallowed-tools <mutating + MCP meta>` — closes residual MCP
    `search_tool`/`use_tool` and other mutating defaults that can outlive the
    allowlist alone
1. `--no-subagents` and `--disable-web-search`

```bash
grok --no-auto-update --output-format streaming-json \
  --sandbox read-only --tools read_file,grep,list_dir \
  --disallowed-tools <mutating defaults including search_tool,use_tool,...> \
  --no-subagents --disable-web-search \
  [--model <id>] [--reasoning-effort <level>] [--resume <id>] \
  --prompt-file <path>
```

Agentic mode (`roborev fix` / `allow_unsafe_agents` / job `agentic`):

```bash
grok --no-auto-update --output-format streaming-json --always-approve \
  [--model <id>] [--reasoning-effort <level>] [--resume <id>] \
  --prompt-file <path>
```

Legacy reasoning mapping: `maximum` → `max`, `thorough` → `high`, `fast` →
`low`. Standard leaves Grok's default effort. Exact `low`, `medium`, `high`,
`xhigh`, and `max` values pass through unchanged.

Session resume uses Grok's `--resume` flag when a session ID is available (same
`SessionAgent` path as Claude/Codex).

### Classification (`SchemaAgent`)

Grok implements `SchemaAgent` for design-routing (`classify_agent = "grok"`) via
headless `--json-schema`. Classification uses **structural tool isolation** (not
absolute Claude-style deny-all equivalence):

- non-empty `--tools <seed>` (empty `--tools ""` is **not** used — Grok
    normalizes an empty CSV to "no override", leaving the default toolset)
- `--disallowed-tools` lists the seed plus every known default Grok Build tool
    and MCP meta (`search_tool`/`use_tool`); when both flags are set, the
    denylist wins on overlap
- `--sandbox read-only`, `--no-subagents`, `--disable-web-search`
- `--max-turns 1`, `--no-memory`, `--no-plan`
- never `--always-approve`
- only Grok-validated `structuredOutput` is accepted; free-form `text` and
    `structuredOutputError` fail the classify call

Residual drift remains if Grok adds tools or aliases; re-validate the deny list
against the auditable pins in source (`grokToolsCLIVersion`, `grokToolsRepoRev`,
`grokToolsMonorepoRev`).

### Skills, hooks, and streaming

Install roborev skills into `~/.grok/skills` with `roborev skills install` (full
review/design/fix/refine/respond surface). Optional mid-session fix hooks:
`roborev agent-hook install --agent grok`. Headless `streaming-json` events
(`text`, `thought`, `tool_call`, …) render through the shared TTY formatter.

### Cursor identity note

The official Grok installer also creates an `agent` symlink/copy. roborev
identifies Cursor via the `agent` command but rejects Grok's alias so a
Grok-only machine is not misdetected as Cursor (local availability and generated
GitHub Actions workflows both pin explicit agent names). Identity probes are
**prompt-safe**: they use only documented version flags (`--version` / `-v`) —
never positional prompts that Cursor would treat as an agent session — and treat
inconclusive results (timeout, crash, empty or oversize version output) as
unavailable rather than as Cursor. Probes are bounded (context deadline plus a
short `WaitDelay` so orphan pipes cannot hang availability checks forever).

## ACP (Agent Client Protocol)

ACP lets you integrate any agent that speaks the
[Agent Client Protocol](https://zed.dev/blog/acp), even if roborev doesn't have
a built-in adapter for it. Configure one or more named ACP agents in
`~/.roborev/config.toml`:

```toml
[acp.codex-acp]
command = "codex-acp"

[acp.goose]
command = "goose"
args = ["acp"]
disable_mode_negotiation = true
```

The subtable key supplies the suffix of the canonical agent identity, so these
entries can be selected with `--agent acp.codex-acp` and `--agent acp.goose`.

See the [Agent Client Protocol (ACP) guide](/advanced/acp/) for setup examples,
the full configuration reference, mode negotiation, and troubleshooting.

## See Also

- [Custom Tasks & Agentic Mode](/advanced/custom-tasks/): Review vs agentic mode
- [Configuration](/configuration/): API keys and auth setup
