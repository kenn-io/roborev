---
title: Kata
description: Pull Kata task context into review prompts and file review findings back as Kata issues
---

[Kata](https://katatracker.com/) is a local-first issue tracker for humans and
coding agents. Issue state lives in a local daemon and SQLite database rather
than in the repository; a repo commits only a small `.kata.toml` binding to its
Kata project. The roborev integration is bidirectional: reviews can pull task
context from your Kata project into the review prompt, and review hooks can file
failed reviews and review findings back into Kata as issues.

## Prerequisites

- The `kata` CLI must be on `PATH`. If it is missing, both directions are
    skipped without failing the review.
- The repo must be bound to a Kata project with a committed `.kata.toml`.

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

The hook uses the repo's committed `.kata.toml` binding by default; set
`project = "myproj"` to override. Every issue receives the `roborev` label plus
a marker label (`review-failed` or `review-finding`) and any extra `labels` you
configure. roborev uses an idempotency key per job/event, so reruns do not
create duplicate issues.

See [Built-in: Kata Integration](/docs/guides/hooks/#built-in-kata-integration)
in the review hooks guide for hook fields and event patterns.

## Closing the Loop

Together, the two directions form a workflow: a commit references a Kata issue,
the review prompt includes that issue as task intent, and if the review fails or
finds problems, the findings land back in the Kata backlog with the commands to
reproduce and fix them.

## Archiving custom rubrics

Roborev can attach the exact rendered custom rubric to issues referenced by a
local commit or range review. This requires Kata 0.21.0 or later, with `kata` on
the roborev daemon's `PATH`. First configure the Kata vault with
`kata docbank setup`. Then opt in globally in `~/.roborev/config.toml`:

```toml
[kata_rubrics]
mode = "current" # off (default) or current
```

The vault is readable by everyone who can read issues or write metadata on that
Kata daemon. Enable this only if that audience may read your rubrics. A
repository can opt out with `[kata_rubrics] mode = "off"` in `.roborev.toml`; it
cannot enable the feature.

Roborev archives after the review completes, subject to these rules:

- It attaches `roborev-rubric.<type>.<digest>.md` to up to ten referenced issues
    in the bound project. The name uses the first 16 SHA-256 characters; the
    review records the full digest. A moved issue is skipped if it now belongs
    to another project. A move between the last check and the attachment write
    can still change the destination.
- Repeated reviews can reuse an existing reference with the same name. Editing a
    rubric adds a new reference and retains the old ones. When the Kata CLI
    supports `docbank attachments add --reuse-existing`, roborev uses atomic
    reuse without listing attachments. This requires daemon API 0.45.0 or
    later; an older daemon rejects the upload until upgraded. Older CLIs use
    list then add, so concurrent archives can create duplicates. Atomic reuse
    applies to the same resolved Docbank version; changed Docbank state can
    resolve a different version.
- A repository-defined type is eligible only when its template and all includes
    resolve inside the repository, including through symlinks. Untracked and
    ignored files are eligible. Global types are operator-owned and can use
    external sources.
- Rubrics larger than 1 MiB are skipped. Built-in types, supplied prompts or
    settings, dirty reviews, CI reviews and synthesis are excluded.
- Archiving is best effort. Failures are logged without a durable retry; the
    next review referencing the issue tries again.
- Before Kata's federated evidence support ships, a relayed project uploads to
    the serving daemon's vault, which other hubs may not be able to read.
