---
title: Reviewing Code
description: Review branches, uncommitted changes, and commit ranges
---

## Feature Branches

Use `--branch` to review all commits since your branch diverged from main:

```bash
roborev review --branch              # Review branch vs auto-detected main/master
roborev review --branch=feature-xyz  # Review a specific branch by name
roborev review --branch --base dev   # Review branch vs specific base
```

Reviews are enqueued and run in the background. Open `roborev tui` to browse
results as they complete. This is the recommended workflow for pre-merge reviews
of entire feature branches.

### Reviewing a Different Branch

By default `--branch` reviews the current branch. You can specify a branch name
to review a different branch without switching to it:

```bash
roborev review --branch=feature-xyz
roborev review --branch=feature-xyz --base develop
```

Note: use `--branch=name` (with `=`), not `--branch name`. The space-separated
form treats `name` as a positional commit argument.

### How It Works

1. roborev detects the merge-base between the target branch and the base branch
1. The merge-base to branch tip range is queued as one review target
1. The AI agent reviews the branch range with per-commit context and prior
    reviews from the same base available in an optional XML document
1. Results are stored and can be viewed in the TUI

Prior range reviews live in a temporary file referenced by a structured marker
in the prompt. The agent may read it to check earlier feedback against the
current code. The file contains review outputs and developer responses, not
previous prompts. `review_context_count` limits the number of earlier endpoints
included, with one review per endpoint. Repeated reviews of the current endpoint
are excluded from this document. The file is removed when the review finishes.

Before measuring or sending a prompt, roborev replaces invalid UTF-8 byte
sequences with `�`. This lets reviews run when a diff includes legacy-encoded
files. Valid Unicode and repository files remain unchanged.

### Pre-Merge Review

Before creating a pull request, review your entire branch:

```bash
git checkout feature-branch
roborev review --branch          # Enqueue branch review (runs in background)
roborev tui                      # Browse results as they arrive
roborev compact                  # Consolidate findings across commits
roborev fix                      # Address consolidated findings
```

The TUI gives you a persistent queue of open reviews that must be explicitly
addressed and closed, creating an accountability loop that prevents findings
from getting lost.

### Panel Reviews

For high-value branch reviews, run a named panel so multiple reviewers check the
same range and roborev synthesizes one parent review:

```bash
roborev review --branch --panel branch_final
```

If your config sets `[review] default_panel`, manual daemon reviews use it
automatically. Use `--panel none` to force a normal single-agent review:

```bash
roborev review --branch --panel none
```

See [Subagent Review Panels](/docs/advanced/subagent-review-panels/) for panel
configuration.

Panel subagents can be marked `allow_failure = true` when a reviewer is useful
but flaky. Those members still contribute findings when they succeed, but a
transient failure or cancellation does not fail the synthesized parent if
required reviewers produced usable output. Mark a subagent `non_voting = true`
to trial it: it runs and stores a review labeled non-voting, but never
influences synthesis or the verdict.

### CI Integration

```bash
if ! roborev review --branch --wait --quiet; then
    echo "Reviews found issues"
    exit 1
fi
```

### Branch Review Options

| Flag | Description |
|------|-------------|
| `--branch [name]` | Review all commits on the branch since base. Optionally specify a branch name with `--branch=name`. |
| `--base <branch>` | Compare against a specific base branch (default: auto-detect main/master) |
| `--wait` | Block until the review or panel parent completes (for CI and scripting; see [When to use `--wait`](#when-to-use-wait)) |
| `--quiet` | Suppress output |
| `--agent <name>` | Use specific agent |
| `--reasoning <level>` | Set reasoning depth |
| `--min-severity <level>` | Lowest severity that fails the review (`low`/`medium`/`high`/`critical`); lower findings are still reported |
| `--panel <name or none>` | Run a named review panel, or use `none` to force single-agent review |

## Uncommitted Changes

Use `--dirty` to review working tree changes before committing:

```bash
roborev review --dirty           # Queue review of uncommitted changes
```

Results appear in `roborev tui` once the review completes.

### What Gets Reviewed

The `--dirty` flag includes:

- Staged changes
- Unstaged changes to tracked files
- Untracked files

### Dirty Review Options

| Flag | Description |
|------|-------------|
| `--wait` | Block until review completes (for CI and scripting; see [When to use `--wait`](#when-to-use-wait)) |
| `--quiet` | Suppress output |
| `--agent <name>` | Use specific agent |
| `--reasoning <level>` | Set reasoning depth |
| `--min-severity <level>` | Lowest severity that fails the review (`low`/`medium`/`high`/`critical`); lower findings are still reported |
| `--panel <name or none>` | Run a named review panel, or use `none` to force single-agent review |

## Review Types

Use `--type` to change what the reviewer focuses on. Omitting `--type` gives you
the standard code review.

```bash
roborev review                       # Default code review
roborev review --type security       # Security-focused review
roborev review --type design         # Design-focused review
roborev review --type lookahead      # Time-series look-ahead bias review
roborev review --branch --type security  # Security review of branch
```

| Type | Focus |
|------|-------|
| *(default)* | Bugs, security, testing gaps, regressions, code quality. This is what you get when you omit `--type`. |
| `security` | Injection, auth, credential exposure, path traversal, unsafe patterns |
| `design` | Completeness, feasibility, task scoping, missing considerations |
| `lookahead` | Time-series look-ahead bias (a.k.a. peekahead / future-data leakage): window/lag direction, train/test ordering, fit-on-full-series, as-of join alignment, point-in-time correctness, label/target leakage |

Review types work with all review modes (`--branch`, `--dirty`, `--since`,
single commits, ranges).

The `security` and `design` types can have their own agent and model
configuration via `{type}_agent` and `{type}_model` in `.roborev.toml` or global
config. See
[Workflow-Specific Agent and Model](/docs/configuration/#workflow-specific-agent-and-model).
`lookahead` has no dedicated fields, so pin it through the generic per-type
block:

```toml
[analyze.lookahead]
agent = "claude-code"
model = "sonnet"
```

When unset, it falls back to your repo or global default agent and model.

You can also define schema-constrained review types backed by local Go
templates, then use them anywhere `--type` or `review_type` is accepted. See
[Custom Review Types](/docs/advanced/custom-review-types/).

## Task Context from Kata

If your repo is bound to a [Kata](https://github.com/kenn-io/kata) project,
reviews can include Kata task context in the prompt:

```toml
[kata_context]
mode = "current"   # off (default), current, or open
max_chars = 50000
```

`current` mode includes only Kata issues referenced in the reviewed commit
messages, such as `Closes: kata#abc4`. `open` mode includes the open Kata
backlog as background context. Dirty reviews have no commit messages, so
`current` mode contributes no Kata context there.

Kata context is optional and applies to local reviews only; CI pull-request
reviews never include it. If the `kata` CLI is missing or the repo is not bound
to Kata, the review proceeds without it. See
[Kata Integration](/docs/configuration/#kata-integration) for setup and
[Built-in: Kata Integration](/docs/guides/hooks/#built-in-kata-integration) for
filing review findings back into Kata.

## Severity Filtering

Use `--min-severity` to decide which findings fail a review:

```bash
roborev review --min-severity high          # Only high and critical findings fail the review
roborev review --branch --min-severity medium  # Low findings are informational in branch reviews
```

The reviewer never sees the threshold and reports every finding with its
severity. Roborev applies the threshold afterwards: the review fails when any
finding is at or above it, and passes when every finding is below it. Findings
below the threshold stay in the review output so you can still read them. A
review that passes under the threshold is a pass like any other: it is eligible
for auto-close, and `roborev fix` and the fix skills skip it because there is
nothing to fix. `fix_min_severity` applies only to the findings of failing
reviews.

Every review agent must return the review JSON model. Agents with native JSON
support use it; other agents receive the model in their prompt. Custom review
types keep their existing native-schema agent requirement. Roborev validates the
returned JSON and derives the result from finding severities. An agent that
cannot review the change reports `unable_to_review` or an error.

Set a default per repo in `.roborev.toml` or globally in
`~/.roborev/config.toml`:

```toml
review_min_severity = "medium"
```

The cascade order is: CLI flag > per-repo config > global config. The CLI flag
overrides the config value. See
[Configuration](/docs/configuration/#per-repository-options).

## Review Guidelines

Use `review_guidelines` to give reviewers durable context about your project or
your whole machine:

```toml
# ~/.roborev/config.toml
review_guidelines = """
Prefer concrete, actionable findings.
Do not flag localhost-only data paths as remote trust boundaries.
"""

# .roborev.toml
review_guidelines = """
This repo has no production database yet.
Public APIs must keep backward-compatible JSON fields.
"""
```

Global guidelines apply to every repo. Repo guidelines are appended after global
guidelines by default, so a repo can add local rules without losing shared
preferences. Set `review_guidelines_supersede_global = true` in `.roborev.toml`
when a repo should replace the global text entirely.

See [Review Guidelines](/docs/configuration/#review-guidelines) for common
patterns and precedence details.

## Specific Commit Ranges

Use `--since` to review commits since a specific point:

```bash
roborev review --since HEAD~5       # Review last 5 commits
roborev review --since abc123       # Review commits since abc123 (exclusive)
roborev review --since v1.0.0       # Review commits since a tag
```

The range is exclusive of the starting commit (like git's `..` range syntax).
Unlike `--branch`, this works on any branch including main.

## Large Diffs

Roborev assembles the complete prompt before comparing it with
`max_prompt_size`. When it exceeds that inline budget, Roborev writes the full
prompt to an ignored file under the repository's configured `snapshot_dir` and
tells the agent to read it. This preserves the instructions, diff, findings, and
discussion.

Ordinary reviews, dirty reviews, and panel synthesis use the same file handoff.
Synthesis is the step that combines panel members' reviews into one result.
Files remain available while the agent runs and are cleaned up afterward on a
best-effort basis. Large dirty diffs do not require smaller commits just to fit
the inline budget. Model context limits, configured policies, and provider
comment limits still apply.

## Session Reuse

!!! warning "Experimental"

    Session reuse is experimental. The behavior and configuration may change in
    future releases. Enable it on a per-repo basis first to evaluate before rolling
    out globally.

When reviewing a branch with multiple commits, each review normally starts a
fresh agent session. This means the agent re-reads the repository context,
re-analyzes unchanged files, and rebuilds its understanding from scratch for
every commit. Session reuse changes this: when enabled, the daemon looks for a
completed review on the same branch that used the same agent and review type,
and resumes that session instead of starting a new one.

The result is that the agent retains its prior conversation context (file
contents it already read, architectural understanding it built up, earlier
findings it made) and only needs to analyze what changed since the last review.
This can substantially reduce token usage and review latency on active branches
with frequent commits.

### Enabling Session Reuse

Set `reuse_review_session = true` in your config:

```toml
# Per repo (.roborev.toml)
reuse_review_session = true

# Or globally (~/.roborev/config.toml)
reuse_review_session = true
```

### How It Works

When a new review is enqueued, the daemon searches for a prior completed review
on the same branch that matches the repo, agent, and review type. Candidates are
checked newest-first and must pass two safety checks:

1. **Ancestor check**: The candidate's reviewed commit must be an ancestor of
    the current target. This prevents reusing sessions from rebased or
    force-pushed branches where the history no longer applies.
1. **Distance limit**: The candidate must be within 50 commits of the current
    target. Sessions from much earlier in the branch history are too stale to
    provide useful context.

If a valid candidate is found, its session ID is passed to the agent, which
resumes the prior conversation. If no candidate qualifies, the review starts
fresh as usual.

### Supported Agents

Session reuse requires agent-side support for resuming conversations. The
following agents support it:

| Agent | Resume mechanism |
|-------|-----------------|
| Codex | `codex exec resume --json <session>` |
| Claude Code | `claude --resume <session>` |
| OpenCode | `opencode run --session <session>` |
| Kilo | `kilo run --session <session>` |
| Pi | `pi --session <path>` |

Agents that do not support session reuse (Gemini, Copilot, Cursor, Kiro, Droid)
ignore the setting and always start fresh sessions.

### Tuning with Lookback

By default, the daemon considers all prior sessions on the branch as candidates.
On long-lived branches with many reviews, you can limit the search to the most
recent N candidates:

```toml
reuse_review_session_lookback = 5   # Only consider 5 most recent sessions
```

This is rarely needed. The default (unlimited) works well because the ancestor
and distance checks already filter out stale candidates.

## Waiting for a Review Without Enqueuing

When a post-commit hook already triggers `roborev review`, you don't need
`review --wait` (which enqueues a duplicate job). Use `roborev wait` to block
until the existing job completes:

```bash
roborev wait                     # Wait for most recent job for HEAD
roborev wait abc123              # Wait for job matching a specific commit
roborev wait --sha HEAD~1        # Explicit git ref
roborev wait --job 42            # Wait for a specific job ID
roborev wait --quiet             # Suppress output (exit code only)
```

Exit codes: 0 for PASS, 1 for FAIL or no job found.

### Argument Resolution

A positional argument is resolved as a git ref first (so numeric SHAs like
`123456` are handled correctly), then as a numeric job ID. Use `--sha` or
`--job` to disambiguate when needed.

### Agent Review-Fix Loops

`roborev wait` is designed for coding agents running review-fix refinement
loops. The agent commits, the hook enqueues the review, and the agent calls
`wait` to block until the verdict is ready. This avoids wasting tokens on
repeated `roborev list` or `roborev show` invocations.

```bash
# Typical agent loop
git commit -m "Fix auth validation"   # Hook triggers review
roborev wait --quiet                  # Block until verdict
# Exit code 0 = pass, 1 = fail
```

## When to use `--wait`

By default, `roborev review` enqueues reviews and returns immediately. The
`--wait` flag blocks until the review completes and prints the result to stdout.

This is a convenience for:

- **CI pipelines**: gate merges on review outcomes using the exit code
- **Orchestrators and one-shot agents**: scripts or automated systems that need
    a synchronous result before proceeding
- **`roborev refine`**: the iterative fix loop uses `--wait` internally to
    re-review after each fix

`--wait` is **not recommended inside interactive agent sessions** (Claude Code,
Codex, etc.). Reviews requested with `--wait` still appear in the TUI, but in
practice the result scrolls past in the conversation and is easy to lose track
of. The async workflow creates a persistent accountability loop: reviews stay
open in the TUI queue until explicitly addressed and closed, so nothing falls
through the cracks.

### Exit Codes

When `--wait` is used, the exit code reflects the review verdict:

- Code 0 for passing reviews
- Code 1 for failing reviews

```bash
# CI example
if ! roborev review --branch --wait --quiet; then
    echo "Reviews found issues"
    exit 1
fi
```

## Addressing Findings

After reviewing, use `roborev fix` to let an agent address any failed reviews:

```bash
roborev fix                      # Fix open reviews on this branch
```

Browse open reviews first with `roborev tui`, then fix them. See
[Responding to Reviews](/docs/guides/responding-to-reviews/) for the full set of
options.

## Review storage and legacy migration

New reviews, compact reviews, and synthesis reviews store their complete JSON
review document. Markdown is generated for display and export. Findings below
the configured severity threshold stay in the document, and synthesis documents
retain their source references and reviewer labels.

On database upgrade, roborev handles each older review in one of three ways:

- A review with valid JSON keeps that JSON as the authoritative review.
- A Markdown-only review that roborev wrote in a format it can read back exactly
    is converted to a JSON document. See
    [Automatic conversion](#automatic-conversion).
- Any other review moves to `legacy_reviews` with a `migration_error` that says
    why it was not converted. It is excluded from normal review reads and from
    `roborev export reviews`.

In every case the original record stays archived in `legacy_reviews`. Sync
ignores Markdown-only review updates from older clients. It does not create new
legacy records from them, and they cannot replace a converted review.

Roborev does not guess. It never invents a severity, a fix, or the sources of a
combined panel review, and it never launches an agent to convert historical
records. Automatic conversion keeps this rule: it only copies fields that the
review text states.

### Automatic conversion

Roborev reads back these formats, which it defined itself:

| Format | What it looks like |
|--------|--------------------|
| Rendered document | The Markdown roborev generates from a JSON review: `## Summary`, an optional `**Agent assessment:**` line, and `## Findings` with numbered severity headings |
| Findings list | The output older review prompts asked for: `## Review Findings`, one group of `**Severity**`, `**Location**`, `**Problem**`, and `**Fix**` bullets per finding, and a `## Summary` section |
| No issues | A review whose first or last line is `No issues found.`. The rest of the text becomes the summary |
| Threshold marker | A review that is only `SEVERITY_THRESHOLD_MET`. The reviewer reported that every finding was below the minimum severity and recorded none, and the summary says so |

A review stays archived, with the reason in `migration_error`, when:

- a finding has no severity, problem, or fix, or its severity is not critical,
    high, medium, or low
- the review has no summary
- any text falls outside the recognized structure, because converting would drop
    or misplace it
- it is a combined panel review with findings that do not name which member
    reviews reported them
- the converted findings would change the verdict roborev already recorded

A converted review keeps its identity, job, verdict, closed state, and
completion time. Its `updated_at` advances, because the stored review changed,
and it syncs to the PostgreSQL mirror again. Older formats never stated the
agent's own verdict, so their documents use review schema version 1, which has
no `verdict` member. The review's pass or fail verdict is unchanged either way.

Reviews archived by an earlier release are converted with one command. Check
first with `--dry-run`, which only reads the database and can run while the
daemon is running. It reports how many reviews would convert and counts the rest
by reason:

```bash
roborev legacy-reviews --db /path/to/reviews.db convert --dry-run
```

Then stop the daemon, convert, and restart the daemon:

```bash
roborev legacy-reviews --db /path/to/reviews.db convert
```

`convert` validates each document the same way `import` does, keeps every
original archived, and is safe to run again. It only looks at reviews that are
still unresolved.

### Converting the remaining reviews with an AI agent

Reviews that stay archived need an AI agent, because reading them takes
judgment. When unresolved records remain, roborev tells you at startup. Stop the
daemon before importing results, and use the database path for the intended
installation:

```bash
roborev legacy-reviews --db /path/to/reviews.db export > migration-input.json
```

Give that file to your agent. It contains the original records, the conversion
errors, and the review and synthesis JSON schemas. Ask the agent to preserve all
findings and to leave any record it cannot convert faithfully unresolved. Import
each completed JSON document using the archive ID from the export:

```bash
roborev legacy-reviews --db /path/to/reviews.db import 1 < converted-review.json
```

An import validates the document before restoring the active review. The
original record remains archived with a resolution timestamp. Restart the daemon
after importing. Migration input contains private review data; keep it with the
local database rather than adding it to a repository.

The PostgreSQL mirror also archives legacy records and excludes them from active
reviews. Its archive retains the original row as JSON in
`legacy_reviews.record`. The mirror upgrade converts the same formats
automatically. Use `--postgres-url` instead of `--db` to convert, export, and
import these records. PostgreSQL archive IDs are UUIDs:

```bash
roborev legacy-reviews --postgres-url "$POSTGRES_URL" convert --dry-run
roborev legacy-reviews --postgres-url "$POSTGRES_URL" convert
roborev legacy-reviews --postgres-url "$POSTGRES_URL" export > migration-input.json
roborev legacy-reviews --postgres-url "$POSTGRES_URL" import <archive-uuid> < converted-review.json
```

## See Also

- [Responding to Reviews](/docs/guides/responding-to-reviews/): Fix findings and
    add comments
- [Code Analysis & Refactoring](/docs/guides/assisted-refactoring/): Targeted
    analysis with `roborev analyze`
- [Auto-Fix with Refine](/docs/guides/auto-fixing/): Automated fix loop
- [Agent Skills](/docs/guides/agent-skills/): Review and fix from within an
    agent session
