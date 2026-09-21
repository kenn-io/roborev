---
last_edited: 2026-09-17
title: MCP Server
description: Expose roborev review data to AI agents over the Model Context Protocol using stdio or streamable HTTP
---

roborev ships an optional
[Model Context Protocol](https://modelcontextprotocol.io) server so coding
agents can read review results without shelling out to the CLI. The server
searches review history, lists repositories, branches, and jobs, and returns
review output, comments, and streamed job output. It can also comment on and
close existing reviews, snooze Agent Hooks, and complete hook fix sessions. It
cannot start or cancel reviews.

Two transports are available and both expose the same tools.

## Install for coding agents

`roborev mcp install` configures agents whose user configuration directories
exist. Skills, Agent Hooks, and MCP installation support Claude Code, Codex,
Factory Droid, Grok Build, Copilot, Cursor, Gemini, Hermes, and Qwen.

```bash
roborev mcp install
roborev mcp install --agent codex
roborev mcp install --agent all --dry-run
roborev mcp install --agent gemini --transport http --url http://127.0.0.1:7373/mcp
```

Stdio is the default. Use `--binary` to pin an installed executable or shim, and
the global `--server` flag to target a specific daemon. HTTP installation
requires `--transport http --url <daemon MCP URL>` and uses the existing daemon
endpoint. Enable `[mcp]` in that daemon's config as described below.
Installation does not start or restart the daemon.

Use `--config` with one `--agent` to select a custom MCP configuration file. The
installer replaces the `roborev` entry and preserves other settings and servers.
It reserializes the configuration, so formatting and comments may change. Files
are replaced atomically, preserving existing permissions and symlink targets.
Avoid editing the same configuration concurrently during installation.
`--dry-run` prints the merged configuration without writing it.

| Agent | User MCP configuration |
| --- | --- |
| Claude Code | `~/.claude.json` |
| Codex | `~/.codex/config.toml` |
| Factory Droid | `~/.factory/mcp.json` |
| Grok Build | `~/.grok/config.toml` |
| Copilot | `~/.copilot/mcp-config.json` |
| Cursor | `~/.cursor/mcp.json` |
| Gemini | `~/.gemini/settings.json` |
| Hermes | `~/.hermes/config.yaml` |
| Qwen | `~/.qwen/settings.json` |

`CODEX_HOME`, `CLAUDE_CONFIG_DIR`, `GROK_HOME`, `COPILOT_HOME`, `QWEN_HOME`, and
`HERMES_HOME` overrides are honored. Gemini uses `$GEMINI_CLI_HOME/.gemini/`
when `GEMINI_CLI_HOME` is set. With `CLAUDE_CONFIG_DIR`, Claude's file is
`.claude.json` inside that directory.

## Opt into MCP skills and hooks

```bash
roborev skills install --mcp
roborev agent-hook install --mcp
```

Hook installation with `--mcp` also installs the MCP configuration and bundled
MCP skills for each selected agent. For an existing daemon HTTP endpoint:

```bash
roborev agent-hook install --agent codex --mcp --mcp-transport http --mcp-url http://127.0.0.1:7373/mcp
```

The HTTP URL also selects the hook's daemon and must use plain HTTP on a
loopback address supported by the daemon client. `--roborev-server` can
explicitly select a daemon for a stdio installation. Hook dry runs show the
final MCP configuration and planned skill directory. Claude MCP configuration is
resolved independently of a hook `--config` path, using its normal home or
`CLAUDE_CONFIG_DIR` location. `agent-hook run --mcp` emits MCP instructions, and
`agent-hook dump --mcp` prints hooks that pass that flag.

Skill updates preserve the installed mode unless `--mcp` or `--mcp=false` is
explicitly supplied. Use `roborev skills install --mcp=false` and reinstall
hooks without `--mcp` to return to CLI mode. Existing MCP registrations remain.
Review creation and the refine CLI still use the CLI; no MCP tool starts a
review. Missing MCP tools produce a connection error rather than silently
switching the skill back to CLI reads.

## Stdio

`roborev mcp serve` speaks MCP over stdin and stdout and reads from the daemon
through its HTTP API. Without `--server`, the discovered daemon is started when
needed. Diagnostics go to stderr so stdout carries only protocol messages.

Configure it as a command-based server in your MCP client:

```json
{
  "mcpServers": {
    "roborev": {
      "command": "roborev",
      "args": ["mcp", "serve"]
    }
  }
}
```

For Claude Code:

```bash
claude mcp add roborev -- roborev mcp serve
```

The `--server` global flag is honored, so a client can target a specific daemon
address with `roborev --server 127.0.0.1:7373 mcp serve`. An explicitly selected
daemon is only probed: it must already be running and match the CLI version, and
nothing is started for it.

## Streamable HTTP

The daemon can serve the same tools at `/mcp` on its API listener. Enable it in
the global configuration and restart the daemon:

```toml
[mcp]
enabled = true
```

With the default `server_addr` the endpoint is `http://127.0.0.1:7373/mcp`. The
daemon only listens on loopback and applies no additional authentication. When
the daemon listens on a Unix domain socket, most MCP clients cannot reach it
over HTTP; use the stdio transport instead.

```json
{
  "mcpServers": {
    "roborev": {
      "type": "http",
      "url": "http://127.0.0.1:7373/mcp"
    }
  }
}
```

The HTTP transport runs in the daemon process and reads the database directly.
The stdio transport is a separate process that talks to the daemon over HTTP, so
it never opens the database itself.

## Discovering the HTTP listener

`roborev mcp status` lists daemons that currently serve the HTTP endpoint
without starting anything. Each entry reports the MCP URL and the daemon API
base URL it reads from, so a host that already selected a daemon can match the
listener to it. The daemon also advertises the same URL as `mcp_url` in its
`/api/ping` response.

```bash
roborev mcp status          # MCP http://127.0.0.1:7373/mcp (pid 4242, daemon http://127.0.0.1:7373)
roborev mcp status --json   # [{"pid":4242,"transport":"http","url":"http://127.0.0.1:7373/mcp","backend_url":"http://127.0.0.1:7373"}]
```

The JSON form is an array, empty when no daemon has `[mcp]` enabled.

## Tools

| Tool | Purpose |
|------|---------|
| `roborev_add_comment` | Add a comment with `job_id`, `commenter`, and `comment` |
| `roborev_close_review` | Close an existing review by `job_id` |
| `roborev_snooze` | Snooze or resume reminders using `repo_path`, `worktree_path`, `branch`, `enabled`, and an RFC3339 `snoozed_until` when enabled |
| `roborev_complete_fix` | Complete the exact `fix_session_id` UUID supplied by Agent Hook |
| `roborev_status` | Daemon version, queue counts, and worker usage |
| `roborev_list_repos` | Tracked repositories with job counts; returns the `root_path` used by other tools |
| `roborev_list_branches` | Branches with job counts for one repository |
| `roborev_list_jobs` | Review jobs (metadata only) filtered by repository, branch, status, job type, or closed state, with cursor paging |
| `roborev_get_review` | Full review output, verdict, and finding counts for a job id or commit SHA |
| `roborev_list_comments` | Developer responses attached to a review |
| `roborev_get_job_output` | The last lines of the agent's streamed output for a job, at most 2,000, with the size of the daemon's retained snapshot |
| `roborev_search_reviews` | Search completed review history by text or meaning, globally or with repository, branch, time, verdict, and state filters |

Job listings omit prompts and diffs. Review results omit the prompt and return
`verdict` as `pass`, `fail`, or empty when no verdict exists yet.

`roborev_search_reviews` accepts `query`, `mode`, `repo`, `branch`, `since`,
`verdict`, `state`, and `limit`. Search is global when `repo` is omitted. Use
`auto` for normal discovery, `lexical` for exact paths, identifiers, SHA
prefixes, or quoted errors, and `semantic` only when wording-independent
retrieval is specifically needed. Panel reviews are grouped into one result; the
returned `job_id` identifies the member whose content matched.

Search responses include `degraded`, `partial`, and `bounded` state plus mirror
and vector coverage. Fetch a matched review with `roborev_get_review`, then use
`roborev_list_comments` only when its responses are needed. See
[Review History Search](/docs/search/) for exact mode, freshness, privacy, and
coverage semantics.

Errors are returned as tool errors with a stable `code` of `not_found`,
`invalid_argument`, `unavailable`, or `internal`.

## Guidance resource

The server publishes a `roborev://mcp/guidance` Markdown resource describing the
recommended call order: status, search, exact review detail, then responses as
needed.

## Review links

Job listings and review results include `web_url` when the daemon has a browser
listener. Use that URL when linking to a review. It includes the daemon's public
browser origin and configured base path. The same field appears in
`roborev list --json` and `roborev show --json`.

Review IDs are local to a daemon. Do not construct a link using another daemon's
origin. When the browser listener is disabled, `web_url` is omitted.
