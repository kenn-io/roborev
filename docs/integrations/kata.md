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
any extra `labels` you configure. Repeated delivery of the same publication
reuses its issue. A rerun produces a new review identity and a separate
publication when the API connection is enabled.

Kata hooks publish only standalone local reviews and synthesis parents. CI
reviews and panel members are excluded from both completed and failed events. If
every local panel member fails, the synthesis parent sends one failure
notification, including when Kata uses the CLI. Other hook types keep their
existing event behavior.

### Publish individual findings

Enable the API connection below, then opt in on each Kata hook:

```toml
[[hooks]]
event = "review.*"
type = "kata"
findings = "comments"  # or "summary"
```

| `findings` | Content sent to Kata |
|------------|----------------------|
| Unset | The existing issue body, with no individual finding comments |
| `comments` | One root comment per structured finding, with its severity, problem, fix, and location |
| `summary` | Generic issue and finding notices with local inspection commands; no finding text, locations, fixes, or raw failure messages |

Historical prose reviews have no finding selectors and remain issue-only.
Summary publications use a separate issue from publications containing full
text. The policy also filters pending metadata and errors; clearing a pending
field does not remove its contents from Kata's federated metadata history.

The daemon saves each authorized publication with the review's completion. It
retains the exact payload, policy, destination, and finding selectors across
restarts and reruns. A worker retries pending publications without another hook
event. The operation stays pending until the issue, every expected root comment,
and the issue's metadata ledger have matching receipts.

Completion capture also chooses API or CLI delivery. Changing the connection
before hook dispatch does not switch that notification's transport. Destination
lookups run before the worker guards its terminal transition, including orphan
recovery, so an unavailable Kata server does not prevent other jobs from
starting or being rerun.

Inspect saved publications or explicitly publish an older review:

```bash
roborev kata show 42
roborev kata create 42
roborev kata show 42 --json
roborev kata show --job 42 --json # force a numeric argument to be a job ID
```

`create` uses the job's matching Kata hooks and requires an enabled API
connection. It queues durable work and prints its operation ID and current
state. `show` reads issue metadata to recover mappings even after a lost
response. Its JSON output includes each finding's selector, issue reference and
UID, root comment UID, policy, operation ID, and delivery state, including older
publications after a rerun. Both commands accept a job ID or git reference,
trying git references first. Boolean `--job` forces a positive numeric job ID.
`roborev show` displays roborev review data without querying Kata.

Each failed manual run has its own frozen publication. Automatic retries keep
the same run identity; manually requeueing the job starts a new one. Explicit
publication captures the current terminal run without replacing earlier work.
`create` returns only that run's publications. Earlier publications cannot
authorize a rerun that has no matching hook. If the review document changes
during capture, `create` reports a conflict so it can be retried against the
current document.

Starting the daemon resumes saved operations only. It never publishes older or
imported reviews because a hook was enabled later. Use `create` to authorize
that backfill explicitly.

### Destination and unresolved delivery

A publication freezes the effective project selection and full project UID when
Kata can resolve it. If Kata is offline, a previously validated UID may be
reused only for the same endpoint and project selection. On the first-ever
offline publication, no UID is known: the worker resolves the frozen selection
through a read and saves that UID before any write. It cannot detect project
reassignment that occurred before that first observation.

If the configured credential file cannot be read during capture, completion
retains a blocked diagnostic. An unserializable publication, such as agent
stderr containing invalid UTF-8, also retains a blocked diagnostic while
preserving the job's original failure locally. Restore the credential file or
correct the publication input and use `roborev kata create` to authorize
publication. A publication already captured with a valid credential waits for
that same credential; it never adopts a replacement key.

Retries refuse a changed endpoint, credential, or project identity. They use
metadata markers and the frozen request fingerprint to find the original issue,
including closed issues. After Kata's seven-day replay window, recovery uses
complete reads; it does not repost a missing or altered comment. Ambiguous
markers, changed ledgers, unsupported schemas, unavailable writer endpoints,
closed issues with missing finding comments, and rejected writes retain the
operation with a pending diagnostic. Existing comments can still establish
delivery on a closed issue. Roborev never takes a federation claim to bypass a
rejection.

Replies quoting a finding marker and comments from other teammates do not
interfere with finding delivery. A recorded root comment must still match its
original content and attribution.

Publications that exceed Kata's 1 MiB request limit stay pending locally. The
worker sizes each possible receipt transition, including the guard's previous
value and the removal of that finding's pending body, before creating an issue.
Shared metadata uses a hash of returned attribution; local receipts retain the
complete author and teammate. The worker preserves the complete findings instead
of clipping their text.

Publishing and replying are separate operations. This integration publishes root
finding comments; it does not infer acceptance, refutation, or a fix from a
commit, passing review, newer review, or closed issue. It does not close reviews
or issues.

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
| `publish_reassessments` | Publish focused finding reassessments as typed replies. Default `false`; requires a completed finding comment mapping |
| `teammate` | Teammate name Kata records on comments roborev posts. Default `roborev`. Letters, digits, `.`, `_`, and `-`, starting with a letter or digit, up to 64 characters. Root finding comments use this name; issues carry no teammate |

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

## Responding to a published finding

Copy the complete finding selector from `roborev show`, then submit a response
through the daemon:

```bash
roborev kata comment --refute <selector> \
  -m 'The caller checks this value before execution reaches the selected path.'
```

Use `--reply <selector>` for progress, `--confirm <selector>` for reproduced
evidence, `--refute <selector>` for contradicting evidence, or
`--supersede <selector> --replacement <selector>` to name a replacement finding.
Confirm and refute require at least 40 Unicode characters after whitespace is
normalized. Short evidence is rejected with its kind unchanged.

The CLI prints an idempotency key before contacting the daemon. Save it: retry
an interrupted request with the same payload and `--idempotency-key <key>`.
Caller-supplied keys are opaque and case-sensitive. If no key is supplied, the
CLI generates a UUID. Reusing a key with changed content is rejected. Inspect
pending operations with `roborev kata show <job_id|sha>`. A queued operation
remains pending until the remote receipt is recorded durably.

Responses use the finding's original comment, destination, and content policy.
They require a completed comment mapping from the API publication hook. Missing
or ambiguous mappings are reported explicitly. A duplicate reply remains pending
and reports the existing comment; roborev never forces it through or silently
changes its target. To continue an existing reply, give its comment reference as
the flag value:

```bash
roborev kata comment --reply c:abcdef -m 'Follow-up evidence.'
```

The daemon checks the comment's lineage against the original finding mapping
before enqueueing it.

A response changes neither the review verdict nor its closed state. Older
daemons and Kata servers without typed reply support require an update; there is
no fallback to a plain comment.

### Publishing focused reassessments

Focused `comment --rebuttal` jobs record their assessment in the daemon, even
when the waiting CLI disconnects. Completed local panel syntheses can also be
reassessed when their original read-only agent session is available. When a
synthesis used its backup agent, reassessment resumes that agent with its backup
model. The daemon retains the original finding, review version, and canonical
document hash. If the document changes before enqueue, the request is rejected.
If it changes during reassessment, even with the same review identity, the
assessment stays with the original snapshot and cannot annotate or close the
replacement review. Equivalent JSON encodings preserve the document hash. With
`--rebuttal --close`, the daemon checks the persisted reassessment context and
closes only that unchanged parent document in the same transaction. A daemon
without the reassessment-close endpoint rejects the close request.

Reassessment publication is off by default. To enable it globally:

```toml
[integrations.kata]
publish_reassessments = true
```

Confirmed and refuted assessments publish as `confirm` and `refute`. A
superseded assessment publishes as `supersede` when the request names a
`--replacement <selector>`; otherwise it publishes as `reply`. These writes use
`<teammate>-<agent>`, which must satisfy Kata's teammate handle rules as a
combined name. Insufficient evidence leaves publication pending with a
diagnostic. Summary policy withholds the finding and explanation text from both
the response payload and delivery errors.

Automatic typed publication requires `--finding-id`. Legacy `--finding` excerpts
retain their local reassessment behavior.
