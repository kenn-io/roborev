---
title: Supported Agents
description: AI agents supported by roborev
---

roborev supports multiple AI coding agents and auto-detects which ones are installed.

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

## Auto-Detection

roborev auto-detects installed agents and falls back in this order:

1. Codex
2. Claude Code
3. Gemini
4. Copilot
5. OpenCode
6. Cursor
7. Kiro
8. Kilo
9. Droid
10. Pi

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

You can override the default model for any agent using the `--model` / `-m` flag:

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

Model resolution priority: CLI flag > per-repo config > global config > agent default.

## Routing Claude Code to a Proxy

The `claude-code` agent accepts a model spec of the form `<model>@<base_url>`. When `<base_url>` starts with `http://` or `https://`, roborev points Claude Code at that endpoint and pins all tier aliases (Opus, Sonnet, Haiku, subagent) to the given model. This lets you use local runtimes (Ollama, LM Studio) or gateways (LiteLLM, OpenRouter) that expose an Anthropic-compatible API.

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

A bare proxy spec (`@http://...` with no model) is rejected with an error. The full URL (including any path or query string) is forwarded as-is to `ANTHROPIC_BASE_URL`, so include the path your gateway expects. For example, LiteLLM typically wants a trailing `/v1`, while Ollama wants no path.

### Proxy Authentication

Set `ROBOREV_CLAUDE_PROXY_TOKEN` in your environment to forward a bearer token to the proxy as `ANTHROPIC_AUTH_TOKEN`. If unset, roborev sends a placeholder token, which is sufficient for gateways that do not validate the header (such as Ollama).

roborev does not forward `anthropic_api_key` (or `ANTHROPIC_API_KEY`) to proxy endpoints. Doing so would leak a real Anthropic credential to arbitrary third parties.

### URL Restrictions

- Proxy URLs must not embed `user:pass@` credentials. Use `ROBOREV_CLAUDE_PROXY_TOKEN` instead.
- `http://` is only accepted for loopback hosts (`127.0.0.1`, `::1`, `localhost`), so plaintext tokens can't be sent over the wire. Use `https://` for remote proxies.

### Environment Behavior

!!! warning
    As of 0.52, the `claude-code` agent always strips the following variables from the child process environment: `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`, `ANTHROPIC_DEFAULT_HAIKU_MODEL`, and `CLAUDE_CODE_SUBAGENT_MODEL`. If you previously routed Claude Code by exporting these in your shell, switch to the `<model>@<base_url>` spec, or configure `anthropic_api_key` in `~/.roborev/config.toml` for native (non-proxy) mode. roborev re-injects the configured key rather than inheriting from the operator's shell.

## Gemini: Antigravity vs Legacy CLI

The Gemini agent works with either the Antigravity `agy` CLI or the legacy `gemini` CLI. Google has deprecated the legacy CLI, so roborev prefers `agy` when both are installed and falls back to `gemini` otherwise.

```bash
# Preferred: Antigravity CLI
curl -fsSL https://antigravity.google/cli/install.sh | bash

# Legacy CLI (still supported)
npm install -g @google/gemini-cli
```

Antigravity runs in print mode — the prompt is passed via `--prompt` on `agy` >= 1.1.1 and piped over stdin with a bare `--print` on older versions (roborev probes `agy --version` to pick the contract). Review jobs get `--sandbox`; agentic jobs get `--dangerously-skip-permissions`.

Antigravity does not currently accept a `--model` flag, so an explicit `--model` returns an error whenever the Gemini agent resolves to `agy` — even when the legacy `gemini` CLI is also installed — rather than silently ignoring the override.

If you rely on model selection, pin the legacy CLI with `gemini_cmd = "gemini"` in your config, install only the legacy CLI, or shadow `agy` on your `PATH` with a wrapper that exec's `gemini`.

## Pi Structured Output

Pi can run normal review jobs and can also serve as the auto design-review classifier. roborev uses Pi's JSON schema output extension for classifier jobs. The default extension source is `npm:@nqbao/pi-json-schema@0.1.1`.

Install the default extension in Pi:

```bash
pi install npm:@nqbao/pi-json-schema
```

roborev still passes the configured extension source explicitly when it invokes classifier jobs. Installing it in Pi makes setup visible in `pi list` and avoids runtime package-fetch surprises in offline or locked-down environments.

Override the extension source in global config if you vendor or mirror it:

```toml
[agent.pi]
jsonschemaextension = "/opt/roborev/pi-json-schema/index.ts"
```

See [Pi Classifier Options](/configuration/#pi-classifier-options).

## Agentic Support

Different agents have different levels of support for agentic mode (file edits and commands):

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
| Pi | Full (tools execute without confirmation) |

See [Custom Tasks & Agentic Mode](/advanced/custom-tasks/) for details on review vs agentic modes.

## ACP (Agent Client Protocol)

ACP lets you integrate any agent that speaks the [Agent Client Protocol](https://zed.dev/blog/acp), even if roborev doesn't have a built-in adapter for it. Configure an ACP agent in the `[acp]` section of `~/.roborev/config.toml`:

```toml
[acp]
name = "codex-acp"
command = "codex-acp"
```

Once configured, the ACP agent can be selected with `--agent <name>`.

See the [Agent Client Protocol (ACP) guide](/advanced/acp/) for setup examples, the full configuration reference, mode negotiation, and troubleshooting.

## See Also

- [Custom Tasks & Agentic Mode](/advanced/custom-tasks/): Review vs agentic mode
- [Configuration](/configuration/): API keys and auth setup
