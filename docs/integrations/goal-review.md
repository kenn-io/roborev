---
title: Engineering with Superpowers and Kata
description: Help agents carry intent through a tracked, reviewed, and verified implementation
---

Superpowers, Kata, and Roborev work together to help agents carry a feature from
an understood outcome to a verified implementation. Superpowers shapes the spec,
linked implementation plan, and execution process. Kata keeps tasks,
dependencies, decisions, and completion evidence visible. Roborev checks whether
the intent and planned work agree, then reviews the implementation as it
develops.

The spec is authoritative; the plan explains how to implement it. Reviews give
agents feedback they can act on throughout the work. A successful review is one
piece of evidence alongside tests and the feature's observed success criteria.

## Let an agent run the engineering loop

Install the bundled skills with `roborev skills install`. With Superpowers and
Kata available, explicitly invoke the workflow in your implementing agent:

```text
$roborev-superpowers Add request validation to the service.
```

Claude Code uses `/roborev-superpowers`. The implementing agent can be Codex,
Claude, or another supported skill host; goal reviews use a separately
configured Pi or supported Claude reviewer. The workflow preserves project
instructions, existing authorization, and Superpowers' decision points.

For example, suppose a feature must reject invalid requests before storing data:

1. Brainstorm a spec with the expected response, constraints, and tests that
    demonstrate no data was stored. Review that spec before implementation.
1. Write its linked Superpowers plan. Represent its validation, handler, and
    regression-test tasks as Kata children, with the spec/plan references and
    actual prerequisites. Review the plan and open graph together.
1. Execute the agreed plan with Superpowers TDD. Each task carries its failing
    test, passing verification, commit, and current attention state in Kata.
1. Address Roborev code findings against the implementation. If the work reveals
    a missing constraint or changes scope, reconcile the spec, plan, and graph,
    then request a fresh intent review before continuing.
1. Exercise the success criteria, complete required tests and current-head
    reviews, and record the delivery evidence. Close finished Kata children,
    then the parent; leave unfinished handoffs explicit.

The skill resumes existing artifacts and tasks instead of starting the process
again. It also supports the selected Superpowers execution method, including
tracked delegation when authorized. The review and gate commands below are the
integration points used by this loop; they can also be used directly.

## Review a spec or plan

Choose `pi` or `claude-code` as your configured `review_agent`, or pass
`--agent` explicitly. After brainstorming writes the spec, run:

```bash
roborev review --type goal --wait \
  --spec docs/superpowers/specs/feature-design.md
```

After writing-plans creates the linked implementation plan, run:

```bash
roborev review --type goal --wait \
  --plan docs/superpowers/plans/feature.md
```

A current Superpowers plan declares its spec before its first task:

```markdown
# Feature Implementation Plan

**Goal:** Implement the feature's success criteria.
**Architecture:** Extend the existing service.
**Tech Stack:** Go and the existing database.
**Spec:** `docs/superpowers/specs/feature-design.md`

### Task 1: Add validation

**Files:** Create `internal/feature/validate.go`
**Interfaces:** Produce `Validate` for request handlers.

- [ ] Add a failing regression test.
- [ ] Run `go test ./internal/feature -run Validate`. Expected: FAIL.
- [ ] Implement validation.
- [ ] Run the same command. Expected: PASS.
```

Use `--local` to run without the daemon. Agent, model, provider, reasoning,
repo, quiet, and wait options follow ordinary review settings. Goal reviews
require Pi or Claude's tool-disabled structured output path. Pi needs the
[JSON Schema extension](/docs/configuration/#pi-classifier-options) used by its
classifier adapter. Claude needs a CLI with `--bare` support and a configured
Anthropic API key or proxy model. Its goal path suppresses hooks, ambient
configuration, MCP servers, and session persistence; OAuth/keychain credentials
are unavailable in bare mode. Unsupported agents return an error. Verification
commands in documents are never run.

Goal review takes no commit arguments. Commit selectors, dirty/branch reviews,
panels, and minimum-severity filtering are unsupported. `--spec` and `--plan`
are available only with `--type goal`. Queued results appear in the existing
show, list, and wait commands. Retry and rerun retain the original documents and
graph, even if the files have changed or disappeared. After editing intent,
enqueue a new review to capture the new evidence. Goal reviews cannot be used as
parents or candidates in the code-fix workflow; address their findings in the
spec, plan, or Kata authoring workflow.

## Select the artifacts

With no explicit paths, roborev requires exactly one file matching
`docs/superpowers/specs/*-design.md`. It discovers a plan only when that plan's
`Spec:` reference resolves to the selected spec. Zero linked plans means a valid
spec-stage review; multiple matches require `--plan`.

Selecting `--plan` reads its spec reference. Roborev loads that reference
without `--spec` only when it points to a top-level
`docs/superpowers/specs/*-design.md` file. Custom locations such as
`docs/plans/` require an explicit `--spec` that matches the plan reference.
Older plans without a spec reference require both `--spec` and `--plan`.
References are read outside code fences and before the first task heading.
Backtick paths, plain paths, and Markdown links are accepted; URLs and multiple
spec declarations are rejected. Unreadable or malformed candidate plans also
fail discovery, so a half-written linked plan is not silently skipped. Select
both paths explicitly to bypass unrelated historical drafts that cannot be read
or parsed.

Paths resolve from the requested checkout root, including plan references.
Absolute paths inside that checkout are accepted, but symbolic links in any
artifact path are rejected. Files may be ignored or uncommitted when an operator
selects them explicitly; plan text alone cannot select arbitrary checkout files.
Historical documents are never selected by date, modification time, Git HEAD, or
execution scratch. Roborev does not read `GOAL.md` or `.superpowers/sdd/` as
artifact selectors.

Either CLI/API path replaces the configured pair as a unit. Blank explicit
paths, ambiguous discovery, unreadable files, and invalid UTF-8 are errors. Long
specs and plans are supported. The shared prompt builder uses `max_prompt_size`
as the inline budget. Larger complete prompts use file handoff through the agent
adapter; Roborev does not trim documents or the graph.

## Automatic review

Configure `.roborev.toml` in the registered repository's main checkout:

```toml
review_agent = "pi"

[goal_review]
enabled = true
spec_file = "docs/superpowers/specs/feature-design.md"
# Omit plan_file to discover the linked plan when writing-plans creates it.
watch = ["goal", "kata_graph"]
```

| Key | Default | Meaning |
|-----|---------|---------|
| `goal_review.enabled` | `false` | Enable automatic review |
| `goal_review.spec_file` | Omitted | Explicit spec path; otherwise discover |
| `goal_review.plan_file` | Omitted | Explicit plan path; otherwise discover by spec linkage |
| `goal_review.watch` | `["goal", "kata_graph"]` | Watch artifact intent, the open graph, or both; `[]` disables polling |

The watcher uses the main checkout's configuration and Kata binding for all its
worktrees. Artifact paths resolve inside each worktree. Manual reviews and gate
requests use the requested checkout's configuration and binding instead. Queued
jobs use the same configuration source for prompt preparation, timeouts, budget
routing, reruns, and backup selection. The TUI rerun picker follows this policy
too. If a requested worktree disappears before execution, its frozen review runs
in the main checkout using the main checkout's configuration.

Every 30 seconds, the daemon checks registered repository configuration.
Disabled repositories skip Git worktree discovery and Kata capture. Each enabled
checkout captures its artifacts and open Kata graph, then compares the selected
watched content with its last review. Enqueueing reuses that capture.

Each checkout has separate pending state. The watcher reviews the initial
snapshot, coalesces edits while a review is pending, and retries capture or
enqueue errors. Completed reviews prevent unchanged snapshots from being
reviewed again after a daemon restart. Candidate gate jobs do not affect this
comparison. A linked plan appearing changes the review stage. Checkbox status
changes alone do not enqueue another review; task or command edits do.

With automatic discovery, no spec means the watcher waits for one to appear.
Ambiguous discovery and missing explicitly configured files produce visible
errors. The watcher reports an unchanged error once, then reports it again only
if the error changes or the checkout recovers and later fails.

These are optional integration points after brainstorming and writing-plans, for
both inline and subagent-driven execution. No installed Superpowers skills or
hooks are modified, and no artifact-written callback is assumed. A review is
evidence about the captured intent, rather than proof of human approval, code
correctness, or tests having run.

## What is checked

Reviews check a coherent outcome, preserved constraints and non-goals, testable
success criteria, concrete verification commands and expected outcomes, grounded
Files/Interfaces, accounted-for deferrals, proportionate scope, graph
contradictions, and plan/spec coverage and drift. Flexible spec sections,
relative Create paths, long documents, older plan formats, and an empty Review
Focus section are supported.

The full open Kata graph includes issue bodies, labels, parent, dependency, and
related links, including issues filed by roborev. A feature spec does not govern
every other feature: drift findings require an explicit relationship to these
artifacts. Closed or external link targets remain references. Absent Kata
binding means an empty graph; a bound but unavailable ledger causes an error.

Each finding includes severity, a problem, a concrete fix, and an optional
location in the captured evidence. An invalid line number loses its line anchor;
an unknown file or Kata ID loses its location. The finding remains visible.
Malformed results or missing problem/fix text still fail validation. Mechanical
plan findings cannot be discarded by the semantic reviewer.

Queued goal reviews retain structured findings and severity counts, stream agent
output, and record reported token usage and cost. Missing cost telemetry remains
unknown. Goal jobs participate in daily budget routing, restricted to supported
goal reviewers. The existing soft-budget and retained-attempt accounting rules
apply.

Goal jobs emit `goal_review.*` events. Custom command hooks can subscribe to
these events explicitly. Existing `review.*` hooks and built-in Kata/Beads hooks
do not handle goal events, avoiding feedback into the reviewed task graph.

## Candidate gate for downstream integrations

The gate mode comes from the trusted global `~/.roborev/config.toml`; checkout
configuration cannot weaken its own gate. The default is `block`:

```toml
[goal_review.kata_gate]
default = "block" # block, warn, or off
```

If `.roborev.toml` sets `goal_review.kata_gate`, loading that repository config
fails with a global-only configuration error. The gate returns a blocking error
response rather than accepting the checkout's policy.

`POST /api/goal-review` evaluates the current graph or a proposed Kata edit
synchronously. It captures the proposed graph without writing Kata, enqueues a
goal review with source `goal_gate`, and waits for the result:

```json
{
  "repo_path": "/workspace/project",
  "spec_file": "docs/superpowers/specs/feature-design.md",
  "candidate": {
    "title": "Add request validation",
    "body": "Implement the spec's invalid-input behavior.",
    "labels": ["implementation"]
  }
}
```

A candidate with no `short_id` simulates creation and uses finding location
`proposed`. An edit must name an open short ID. Title and body are required.
Omitted or null labels/links preserve existing values; explicit empty arrays
clear them. Links use `type` (`parent`, `blocks`, or `related`) and `to_ref`
(local or qualified ID); `incoming` is available only for `blocks`. Replacing
links replaces all incident edges. Targets must be captured open issues; closed
or external references are not writable candidate targets.

The version 1 response contains `status` (`pass`, `findings`, `error`, or
`off`), `mode`, `blocked`, `snapshot_id`, `findings`, and an optional `error`.
In block mode, any finding or evaluation error blocks. Warn mode reports
findings/errors without blocking. Off performs no capture or agent call.
Disabling the watcher does not disable the candidate gate.

Completed evaluations, including capture/reviewer errors, return HTTP 200.
Malformed inputs return 400; shutdown returns 503. Complete candidate task
bodies reach the review. Gate jobs share the ordinary worker pool, retries,
logs, budget routing, and usage accounting. They appear in review history. The
configured job timeout bounds the gate request, including time waiting in the
queue. Disconnect or daemon shutdown cancels queued or running gate jobs. The Go
client's `HTTPClient.GoalReview(ctx, request)` preserves its caller context
rather than imposing the ordinary ten-second HTTP timeout.

A pass applies only to its snapshot ID, which includes both artifacts and the
candidate graph. Downstream Kata hooks must serialize review-and-commit or
verify artifact and ledger freshness. Acknowledgement and override policy belong
downstream; this endpoint does not implement Kata's create/edit hook.
