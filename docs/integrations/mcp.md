---
title: MCP Server
description: Expose roborev review data to AI agents over the Model Context Protocol using stdio or streamable HTTP
---

roborev ships an optional
[Model Context Protocol](https://modelcontextprotocol.io) server so coding
agents can read review results without shelling out to the CLI. The server is
read-only: it lists repositories, branches, and jobs, and returns review output,
comments, and streamed job output. It never enqueues, cancels, closes, or
comments on reviews.

Two transports are available and both expose the same tools.

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
| `roborev_status` | Daemon version, queue counts, and worker usage |
| `roborev_list_repos` | Tracked repositories with job counts; returns the `root_path` used by other tools |
| `roborev_list_branches` | Branches with job counts for one repository |
| `roborev_list_jobs` | Review jobs (metadata only) filtered by repository, branch, status, job type, or closed state, with cursor paging |
| `roborev_get_review` | Full review output, verdict, and finding counts for a job id or commit SHA |
| `roborev_list_comments` | Developer responses attached to a review |
| `roborev_get_job_output` | The last lines of the agent's streamed output for a job, at most 2,000, with the size of the daemon's retained snapshot |

Job listings omit prompts and diffs. Review results omit the prompt and return
`verdict` as `pass`, `fail`, or empty when no verdict exists yet.

Errors are returned as tool errors with a stable `code` of `not_found`,
`invalid_argument`, `unavailable`, or `internal`.

## Guidance resource

The server publishes a `roborev://mcp/guidance` Markdown resource describing the
recommended call order: status, repositories, jobs, then review detail.
