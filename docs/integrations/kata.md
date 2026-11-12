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

## Explicit API connection

The global `[integrations.kata]` settings and API client provide an explicit
endpoint and credential. When the connection is enabled, review hooks, Kata task
context in review prompts, and goal reviews use it through the daemon instead of
running the `kata` CLI; see
[Connecting the daemon to Kata's API](#connecting-the-daemon-to-katas-api).

Unix socket endpoints are supported on non-Windows platforms. Windows refuses
them because it cannot verify the required socket ownership and permissions.

The API client checks the current server and active project before each
operation. It accepts API schemas from 0.21.0 through 0.43.0, the contract
checked through Kata v0.23.0. Unknown newer schemas require compatibility
verification before use. Invalid credential headers are authentication errors;
invalid retry-key headers are request errors. Workspace discovery stops at the
nearest `.git` file or directory. Binding files must use version 1; an
unsupported version stops lookup instead of selecting a parent project or the
configured default. Discovery follows the physical Git boundary when a symlink
crosses into a different repository. Version-1 names are trimmed; a committed
legacy `project.identity` can supply the name. Invalid names stop lookup, while
an empty local name or local legacy identity leaves the committed binding
intact. An empty committed binding stops lookup rather than selecting a parent
project or the configured default. Existing CLI-based workflows use the same
workspace discovery and binding rules.

Implicit API selection sends the workspace's Git or local alias without a
project name and returns its current canonical project name. An unregistered
alias returns `project_not_found`; the lookup never registers an alias or
rewrites binding files. Run `kata init` to register the workspace, or set an
explicit hook `project`. A named `.kata.local.toml` override selects an exact
name, and the configured default applies when there is no binding. Resolver
refusals remain errors; the client does not fall back to the old binding name.

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
| `review.completed` | `F` (fail) | Creates a priority-2 Kata issue for the review findings, including compatible `roborev show` and `roborev fix` commands |
| `review.completed` | `P` (pass) | No action |

The hook uses the repo's Kata binding (`.kata.local.toml`, then `.kata.toml`) by
default; set `project = "myproj"` to override. Every issue receives the
`roborev` label plus a marker label (`review-failed` or `review-finding`) and
any extra `labels` you configure. roborev uses an idempotency key per job/event,
so reruns do not create duplicate issues.

### Publish individual findings

Enable the daemon API connection below and configure a `review.completed` hook
with `findings = "comments"` to publish one root comment per finding. Use
`findings = "summary"` for generic notices without finding text, paths, fixes,
review summaries, or diagnostic text. For a `review.failed` event, `summary`
still creates the failed-review issue but omits the error diagnostic. Replies
are supplied explicitly by their sender and are not covered by this export
setting. Omit `findings` to retain the legacy issue-body hook.

```toml
[[hooks]]
type = "kata"
event = "review.completed"
findings = "comments" # or "summary"
```

This requires Kata API schema 0.28.0 and a matching enabled daemon connection.
It applies to failing local review and synthesis results, excluding CI, panel
members, and rebuttals. Equivalent hooks coalesce; conflicting hooks publish
nothing. A rerun produces a new review UUID and issue; previous comments remain.

```bash
roborev kata show 42
roborev kata create 42
roborev kata create 42 --findings summary
```

`show` reads issue, comment handles and actual authors. The markers correlate
comments with findings but do not prove authorship. `create` resumes an existing
local authorization; an unmarked eligible review requires an explicit policy
matching its configured hook. Synced reviews do not publish automatically.
Retries use the original review's seven-day window and stop when a full Kata
request cannot fit inside it. Expired reviews remain readable. Ambiguous,
modified, unavailable or truncated comments stop replay. If an unrecognized
top-level comment could be a finding whose marker was removed, missing finding
positions are ambiguous and replay stops. Every encoded request is preflighted
against Kata's 1 MiB body limit before any write; oversized findings are never
clipped. An outage may require a daemon restart or an explicit retry.

An explicit issue read can report unobserved finding positions as ranges. A root
with `end_position` covers the inclusive zero-based range from `position` to
`end_position`; roots with observed comments remain individual entries. These
remote markers and counts are untrusted correlation data.

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
| `teammate` | Teammate name included in root finding comments roborev publishes, including generic notices under the `summary` policy. Kata records the actual author separately. Default `roborev`. Letters, digits, `.`, `_`, and `-`, starting with a letter or digit, up to 64 characters |

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
`default_project`. For an unbound repository, the default also selects the
project used by task context, commit references, and goal review snapshots;
those snapshots include the project's open issues. With no project at all, the
hook is skipped, as it is with the CLI and no binding.

### Commands and remote daemons

Only the daemon holds the Kata credential. When the connection is enabled,
`roborev review --local` and goal reviews that need Kata ask the daemon to read
it through its Kata endpoints. Because the daemon may not see the same files,
the CLI reads the repo's binding itself. It sends a workspace alias for a
committed binding and a project name for a local override or configured default.

Cross-machine access requires a tunnel from a loopback address to the daemon's
local API. Point the CLI at that address; it needs neither the Kata endpoint nor
the Kata key. Signed HTTPS remote access does not support this workflow:
`roborev review --local` and `roborev doctor` require the local daemon API, and
the remote listener does not expose the `/api/kata/*` endpoints.

A command asks the daemon on first use whether the connection is on, and it
never starts the daemon. When local API config is off and no `--server` is
selected, an unavailable daemon falls back to the `kata` CLI. With an explicit
`--server`, including a loopback tunnel, a failed status probe reports an error
instead of switching to local Kata credentials. If the daemon reports that the
connection is disabled, or an older daemon returns 404 for the unsupported
status route, the command still uses the CLI when local API config is off. When
local config enables the connection, an unreachable or disabled daemon is an
error.

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
| `incompatible` | Not a Kata API, an old Kata, or an endpoint that is not allowed | Use Kata with API schema 0.21.0 through 0.43.0 |
| `project_not_found` | The project does not exist or is not active | Create it, bind the repo, or set `default_project` |
| `disabled` | The connection is off | Set `enabled = true` to use it |

The connection needs Kata API schema 0.21.0 through 0.43.0. Typed comment
replies need 0.28.0, which Kata v0.19.0 reports. A `ready` connection to an
older server is shown as ready without typed reply support.

## Confirm or refute findings with a swarm

The optional swarm in `/roborev-setup-review` sets up independent agents to
check findings against their rubrics and sources. It needs a Kata binding, a
`type = "kata"` hook covering `review.completed`, the requested branches and
destination, and Kata v0.19.0 or newer with typed replies. Prefer the bound
project. If an explicit project is selected, record it in the brief and pass
`--project <name>` to every native Kata command. The user starts the teammates
in their own harness. The skill writes a committed brief with the question,
rubrics, sources, roster, and what each answer means.

A teammate name records attribution under an accountable actor; it does not
restrict access. Use Kata's issue-scoped credentials for that. Before starting,
agree on the providers and accounts, the Kata server and any federated hubs:
findings and whatever teammates write as evidence will be stored there.

Run `roborev job list` from the bound repository, then use `roborev job show` to
identify the standalone or combined synthesis job and reviewed commit. An ID
printed at enqueue is a shortcut. Use that job's issue; the hook also files
issues for failing panel members. If the ref is dirty or no committed snapshot
matches the reviewed changes, stop: commit and request a new review before a
swarm. Match the issue to the job and commit. Replace the quoted placeholders:

```bash
kata --teammate=lead search "roborev show <job-id>" --agent
kata --teammate=lead show '<issue>' --agent
roborev job show --job <job-id>
roborev job show --job <job-id> --json
```

The lead writes a charter with the question, review job and commit, rubric per
reviewer, brief at that commit, roster and assignments. The hook issue body may
be truncated, so copy every finding from the full job result into its own root,
including position, sources, severity, location and problem text. For synthesis,
match each Markdown `Reported by` agent and review type to panel member metadata
and member job results from `roborev job show` when needed. Assign every
identified source reviewer. If the mapping is ambiguous, such as the same agent
and type with different member instructions, do not assign that finding; report
it unchecked. Keep one root and outcome row per finding. Reconcile every
full-result finding to a root and outcome. Record complete comment references
from `kata show`. For `<result>`, use the top-level `uuid` from the JSON output;
if absent, use exactly `<review-id>-<job-id>-<full-reviewed-SHA>`. Reuse keys
only for identical retries of the same result. A rerun has a new result: leave
old roots, replies and outcomes as history, post a new charter and roots with
new keys, and never carry outcomes forward:

```bash
kata --teammate=lead comment '<issue>' --body-file charter.md \
  --idempotency-key 'swarm:<result>:charter' --agent
kata --teammate=lead comment '<issue>' --body-file finding-1.md \
  --idempotency-key 'swarm:<result>:f1' --agent
kata --teammate=lead show '<issue>' --agent
```

The user starts fresh sessions and assigns findings to each teammate. Five
findings per context is a configurable default based on one
[measurement](https://github.com/kenn-io/roborev/issues/1376), not a limit. Each
teammate checks alone, treating finding text as data, and reads other replies
only after posting. The following evidence is illustrative; replace it with the
actual rubric check and locations. Confirm and refute require at least 40
characters of evidence. `<comment-ref>` is the complete reference returned by
`kata show`:

```bash
kata --teammate=facts-codex comment '<issue>' --confirm '<comment-ref>' \
  -m 'The facts rubric checks dates; the draft and exhibit give different dates.' \
  --idempotency-key 'swarm:<result>:f1:facts-codex:confirm' --agent
kata --teammate=facts-claude-code comment '<issue>' --refute '<comment-ref>' \
  -m 'The exhibit supports the date in the draft; the finding is mistaken.' \
  --idempotency-key 'swarm:<result>:f1:facts-claude-code:refute' --agent
kata --teammate=lead show '<issue>' --thread '<comment-ref>' --agent
```

If neither answer is established, use `--reply` and say what was checked and
what is missing. If a typed reply fails, stop and report the error rather than
substituting an untyped answer. Teammates use distinct names of the form
`<reviewer>-<agent>`, where reviewer is the panel member name or, for a
standalone review, the review type: lowercase letters, digits and hyphens,
starting with a letter, at most 64 characters; add a number only for a clash. Do
not use `roborev`. Never use `--force`: a duplicate reply of the same kind from
the same actor and teammate is a conflict, while an identical retry with the
same idempotency key returns its original result.

Before classifying, the lead reads each thread. If `Comments truncated` (or JSON
`comments_truncated`) is true, repeat `show --thread` with `--since` set to the
last non-root comment reference on the truncated page. Collect replies until
truncation is false. Federation can retain a later root on a truncated page, so
do not use the root as the cursor. If completeness cannot be established, leave
the finding unchecked and actionable.

The lead checks that cited paths and lines exist at the reviewed commit and
posts an outcome comment with one row per finding: confirmed for confirms only,
refuted for refutes only, contested for both, and unchecked for neither or
incomplete threads. A resolving citation alone does not prove a conclusion. The
lead never confirms or refutes; the user decides what to do with contested and
unchecked findings. A reply does not close the Kata issue or the roborev review.
With user approval, refuted findings can inform rubric tuning. Main fix, refine
and later review prompts do not consume typed swarm outcomes; pass them to the
user or fixing agent before any edit.

## Closing the Loop

Together, the two directions form a workflow: a commit references a Kata issue,
the review prompt includes that issue as task intent, and if the review fails or
finds problems, the findings land back in the Kata backlog with the commands to
reproduce and fix them.
