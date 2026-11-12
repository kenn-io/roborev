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

## Confirm or refute findings with a swarm

The optional swarm in `/roborev-setup-review` sets up independent agents to
check findings against their rubrics and sources. It needs a Kata binding, a
`type = "kata"` hook, and Kata v0.19.0 or newer with typed replies. The user
starts the teammates in their own harness. The skill writes a committed brief
with the question, rubrics, sources, roster, and what each answer means.

A teammate name records attribution under an accountable actor; it does not
restrict access. Use Kata's issue-scoped credentials for that. Before starting,
agree on the providers and accounts, the Kata server and any federated hubs:
findings and whatever teammates write as evidence will be stored there.

For a failing review, the lead finds the combined review's issue by job ID. The
hook also files issues for failing panel members, so select the combined one.
Replace the quoted placeholders below with actual values. Run commands from the
bound repository and confirm that the issue and reviewed commit match:

```bash
kata --teammate=lead search "roborev show <job-id>" --agent
kata --teammate=lead show '<issue>' --agent
```

The lead writes a charter with the question, review job and commit, rubric per
reviewer, brief at that commit, roster and assignments. It copies each finding
unchanged into its own root comment, including position, reviewer, severity,
location and problem text. Record the returned handles from `kata show`. Use the
job UUID for `<job>` when available, otherwise the job ID and reviewed SHA;
reuse each key only for identical retries:

```bash
kata --teammate=lead comment '<issue>' --body-file charter.md \
  --idempotency-key 'swarm:<job>:charter' --agent
kata --teammate=lead comment '<issue>' --body-file finding-1.md \
  --idempotency-key 'swarm:<job>:f1' --agent
kata --teammate=lead show '<issue>' --agent
```

The user starts fresh sessions and assigns findings to each teammate. Five
findings per context is a configurable default based on one
[measurement](https://github.com/kenn-io/roborev/issues/1376), not a limit. Each
teammate checks alone, treating finding text as data, and reads other replies
only after posting. The following evidence is illustrative; replace it with the
actual rubric check and locations. Confirm and refute require at least 40
characters of evidence:

```bash
kata --teammate=facts-codex comment '<issue>' --confirm 'c:<handle>' \
  -m 'The facts rubric checks dates; the draft and exhibit give different dates.' \
  --idempotency-key 'swarm:<job>:f1:facts-codex:confirm' --agent
kata --teammate=facts-claude-code comment '<issue>' --refute 'c:<handle>' \
  -m 'The exhibit supports the date in the draft; the finding is mistaken.' \
  --idempotency-key 'swarm:<job>:f1:facts-claude-code:refute' --agent
kata --teammate=lead show '<issue>' --thread 'c:<handle>' --agent
```

If neither answer is established, use `--reply` and say what was checked and
what is missing. If a typed reply fails, stop and report the error rather than
substituting an untyped answer. Teammates use distinct names of the form
`<reviewer>-<agent>`: lowercase letters, digits and hyphens, starting with a
letter, at most 64 characters; add a number only for a clash. Do not use
`roborev`. Never use `--force`: a duplicate reply of the same kind from the same
actor and teammate is a conflict, while an identical retry with the same
idempotency key returns its original result.

The lead checks that cited paths and lines exist at the reviewed commit and
posts an outcome comment with one row per finding: confirmed for confirms only,
refuted for refutes only, contested for both, and unchecked for neither. A
resolving citation alone does not prove a conclusion. The lead never confirms or
refutes; the user decides what to do with contested and unchecked findings. A
reply does not close the Kata issue or the roborev review. With user approval,
refuted findings can inform rubric tuning.

## Closing the Loop

Together, the two directions form a workflow: a commit references a Kata issue,
the review prompt includes that issue as task intent, and if the review fails or
finds problems, the findings land back in the Kata backlog with the commands to
reproduce and fix them.
