---
title: Kata
description: Pull Kata task context into review prompts, file review findings back as Kata issues, and connect the daemon to Kata's API
---

[Kata](https://katatracker.com/) is a local-first issue tracker for humans and
coding agents. Issue state lives in a local daemon and SQLite database rather
than in the repository; a repo commits only a small `.kata.toml` binding to its
Kata project. The roborev integration is bidirectional: reviews can pull task
context from your Kata project into the review prompt, and review hooks can file
failed reviews and review findings back into Kata as issues.

## Prerequisites

Roborev reaches Kata in one of two ways:

- **The `kata` CLI (default).** The `kata` CLI must be on `PATH`. If it is
    missing, both directions are skipped without failing the review. Kata's own
    routing applies: `KATA_SERVER`, the binding, or the local daemon.
- **Kata's API.** The roborev daemon connects to a Kata server you name in
    `[integrations.kata]`, with its own credential, and needs no `kata` CLI. See
    [Connecting the daemon to Kata's API](#connecting-the-daemon-to-katas-api).

A repo is bound to a Kata project by `.kata.toml` (usually committed) or
`.kata.local.toml` (a local override that is not committed). When both name a
project, `.kata.local.toml` wins. Roborev looks for the binding in the
repository and its parent directories, the way the `kata` CLI does.

## Superpowers specs, plans and the open graph

[Superpowers Intent Reviews](/docs/integrations/goal-review/) review design
intent before implementation, using the spec, its linked plan and the complete
open Kata graph. They include issues filed by roborev, preserve graph relations
and fail when a bound ledger is unavailable. A synchronous API can also evaluate
a proposed issue or edit before a caller writes it to Kata.

The optional code-review context below has separate settings and limits.

## Task Context in Review Prompts

When enabled, roborev includes Kata issue content in local review prompts so the
reviewer understands the task intent behind a change:

```toml
# .roborev.toml or ~/.roborev/config.toml
[kata_context]
mode = "current"   # off (default), current, or open
max_chars = 50000  # cap on Kata context bytes in the prompt
```

| Mode | Behavior |
|------|----------|
| `off` | Do not include Kata context (default) |
| `current` | Include only Kata issues referenced in the reviewed commit messages, such as `Closes: kata#abc4` or `<project>#abc4` |
| `open` | Include all open Kata issues from the bound project, excluding issues filed by roborev itself |

`current` mode frames referenced issues as authoritative task intent. `open`
mode frames the open backlog as background context so the reviewer does not
treat every open issue as part of the current change. Dirty reviews have no
commit messages to inspect, so `current` mode includes no Kata context for them;
use `open` if you want dirty reviews to see the backlog.

Kata context applies to local review prompts only (single commit, branch ranges,
and dirty reviews). Daemon CI pull-request reviews never include Kata context,
since PR head content is untrusted and backlog details could leak into public PR
comments. Fix and task jobs do not receive Kata context either.

If the prompt exceeds `max_prompt_size`, roborev trims other optional context
first and drops Kata context last.

See [Kata Integration](/docs/configuration/#kata-integration) in the
configuration reference for full details.

## Filing Findings as Kata Issues

A `type = "kata"` review hook files review failures and findings as Kata issues:

```toml
[[hooks]]
event = "review.*"
type = "kata"
branches = ["main"]
labels = ["from-review"]
priority = 2
```

| Event | Verdict | Action |
|-------|---------|--------|
| `review.failed` | n/a | Creates a priority-1 Kata issue for the failed review job |
| `review.completed` | `F` (fail) | Creates a priority-2 Kata issue for the review findings, including `roborev show` and `roborev fix` commands |
| `review.completed` | `P` (pass) | No action |

The hook uses the repo's Kata binding (`.kata.local.toml`, then `.kata.toml`) by
default; set `project = "myproj"` to override. Every issue receives the
`roborev` label plus a marker label (`review-failed` or `review-finding`) and
any extra `labels` you configure. roborev uses an idempotency key per job/event,
so reruns do not create duplicate issues.

See [Built-in: Kata Integration](/docs/guides/hooks/#built-in-kata-integration)
in the review hooks guide for hook fields and event patterns.

## Connecting the daemon to Kata's API

By default roborev runs the `kata` CLI from the repository, and the CLI decides
which Kata server to use and who is writing. The API connection replaces that
with one explicit server and one identity that belongs to roborev. Enable it in
the global config on the machine that runs the daemon:

```toml
# ~/.roborev/config.toml
[integrations.kata]
enabled = true
endpoint = "https://kata.example.com"           # https, loopback http, or unix:///path
api_key_file = "~/.config/roborev/kata-token"   # or api_key; masked in config output
default_project = "reviews"                     # used when a repo has no binding
teammate = "roborev"                            # default
```

| Key | Description |
|-----|-------------|
| `enabled` | Use the API connection instead of the `kata` CLI. Default `false` |
| `endpoint` | Required when enabled: `https://host`, `http://localhost:port` (or another loopback address), or `unix:///path/to/socket`. Remote plaintext HTTP, userinfo, query strings, and fragments are rejected |
| `api_key` | Bearer credential, masked in `roborev config list` and `get`. Prefer `api_key_file` |
| `api_key_file` | File holding the credential, read by the daemon each time it connects. A leading `~/` is your home directory. Mutually exclusive with `api_key` |
| `default_project` | Project used when a repo has no binding |
| `teammate` | Teammate name Kata records on comments roborev posts. Default `roborev`. Letters, digits, `.`, `_`, and `-`, starting with a letter or digit, up to 64 characters. Roborev files issues, which carry no teammate, and does not post comments yet, so nothing uses it today |

The section is global-only. A repo's `.roborev.toml` that sets it is rejected,
because a repo must not choose where credentials are sent. A loopback HTTP
endpoint needs a key; a Unix socket does not, and roborev uses one only when the
socket and its directory are private to the daemon's user. Changes apply to the
next Kata call; the daemon needs no restart.

### What changes when it is enabled

- Review hooks, Kata task context in review prompts, and Superpowers goal
    reviews all use the connection. None of them runs the `kata` CLI.
- Roborev ignores `KATA_SERVER`, `KATA_AUTHOR`, `KATA_TEAMMATE`, and the
    `server` settings in `.kata.local.toml`. Only `[integrations.kata]` selects
    the server and the writer.
- The connection never follows redirects and uses Kata v0.19.0's five-second
    non-streaming request budget.
- If the connection is enabled but cannot be built, for example because the
    endpoint is not allowed or the key file cannot be read, every Kata call
    fails with that reason. Roborev never falls back to the `kata` CLI, which
    would send the work to a different Kata than the one you configured.

### Give roborev its own credential

Every write names `roborev` as the actor. A credential that Kata issues for a
named user or service overrides that name, and a static token uses it. Create a
credential just for roborev so its issues are not attributed to a person, and
roborev records the author that Kata returns, not the one it asked for. The hook
log line for a filed issue ends with that author. Roborev never writes as
another actor.

### How the project is chosen

1. The hook's `project`, when it sets one.
1. The repo's binding: `.kata.local.toml`, then `.kata.toml`.
1. `default_project`.

The project must exist on the server and be active. A binding file that exists
but cannot be read stops the lookup instead of falling through to
`default_project`. With no project at all, the hook is skipped, as it is with
the CLI and no binding.

### Commands and remote daemons

Only the daemon holds the Kata credential. When the connection is enabled,
`roborev review --local` and goal reviews that need Kata ask the daemon to read
it through its Kata endpoints. The CLI reads the repo's binding itself and sends
the project name, because the daemon may not see the same files.

Cross-machine access requires a tunnel from a loopback address to the daemon's
local API. Point the CLI at that address; it needs neither the Kata endpoint nor
the Kata key. Signed HTTPS remote access does not support this workflow:
`roborev review --local` and `roborev doctor` require the local daemon API, and
the remote listener does not expose the `/api/kata/*` endpoints.

A command asks the daemon on first use whether the connection is on, and it
never starts the daemon. If the daemon is not running, or has the connection
off, the command runs the `kata` CLI as it always has. The exception is a shell
whose own config enables the connection: there an unreachable or disabled daemon
is an error, not a quiet switch to the CLI.

### Check the connection

`roborev doctor` shows the daemon's view under Integrations: the state, the Kata
server version, and whether the server stores typed replies. It checks the
project that `roborev review --local` would send: the repo's binding as this
machine sees it, else the daemon's `default_project`. A binding file that cannot
be read is reported separately.

| State | Meaning | What to do |
|-------|---------|------------|
| `ready` | The server is compatible and the project exists | Nothing |
| `unreachable` | The server did not answer | Check that Kata is running and the endpoint is right |
| `authentication_required` | Kata rejected the credential, or none could be read | Set `api_key_file` to a credential for roborev |
| `incompatible` | Not a Kata API, an old Kata, or an endpoint that is not allowed | Use Kata with API schema 0.21.0 or later |
| `project_not_found` | The project does not exist or is not active | Create it, bind the repo, or set `default_project` |
| `disabled` | The connection is off | Set `enabled = true` to use it |

The connection needs Kata API schema 0.21.0 or later. Typed comment replies need
0.28.0, which Kata v0.19.0 reports. A `ready` connection to an older server is
shown as ready without typed reply support.

## Closing the Loop

Together, the two directions form a workflow: a commit references a Kata issue,
the review prompt includes that issue as task intent, and if the review fails or
finds problems, the findings land back in the Kata backlog with the commands to
reproduce and fix them.
