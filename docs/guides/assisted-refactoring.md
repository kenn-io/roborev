---
title: Code Analysis and Assisted Refactoring
description: Run built-in code analysis and automatically apply fixes with roborev analyze and roborev fix
---

Use `roborev analyze` to run structured code analysis on your files, and `roborev fix` to automatically apply the suggested changes. Together they provide a repeatable workflow for codebase-wide refactoring driven by AI agents.

```bash
roborev analyze refactor 'src/handlers/*.go'                    # Analyze files for refactoring opportunities
roborev analyze complexity --fix internal/storage/            # Analyze and automatically fix
roborev analyze duplication --per-file --fix 'internal/*.go'    # Analyze and fix one file at a time
roborev fix                                                    # Fix all open findings
```

## Analysis Types

Seven built-in analysis types target common refactoring concerns:

| Type | Description |
|------|-------------|
| `duplication` | Finds exact and structural code duplication across files and suggests consolidation opportunities. |
| `refactor` | Identifies code smells like long functions, complex conditionals, poor naming, and tight coupling. |
| `complexity` | Looks for complex functions, deep nesting, and large conditionals, then suggests simplifications. |
| `test-fixtures` | Finds repeated test setup, duplicated mocks, and similar assertion sequences across test files. |
| `api-design` | Reviews public API surface for naming consistency, parameter ordering, and error handling patterns. |
| `dead-code` | Finds unused exports, unreachable code paths, and stale commented-out code. |
| `architecture` | Evaluates package organization, dependency flow, separation of concerns, and layering violations. |

List available types with `roborev analyze --list`, or preview a prompt template with `roborev analyze --show-prompt <type>`. If you have shell completions enabled, `roborev analyze <TAB>` will suggest analysis types with descriptions.

## Basic Usage

Point `analyze` at an analysis type and one or more file paths, directories, or glob patterns:

```bash
# Analyze test files for fixture opportunities
roborev analyze test-fixtures 'internal/storage/*_test.go'

# Find duplication across a package
roborev analyze duplication cmd/roborev/

# Review API design patterns
roborev analyze api-design 'pkg/api/*.go'

# Architecture review of a directory
roborev analyze architecture internal/storage/
```

Results appear in the TUI as jobs labeled with their analysis type (e.g. `test-fixtures`, `duplication`). Use `roborev show <job_id>` or the TUI to read findings. You can then run `roborev fix` to address open findings, or pass specific job IDs with `roborev fix <job_id>`.

## Branch Mode

Use `--branch` to automatically analyze files changed on the current branch since diverging from the base:

```bash
roborev analyze refactor --branch                  # Analyze current branch
roborev analyze complexity --branch --per-file     # Per-file analysis of branch changes
roborev analyze refactor --branch --base develop   # Compare against develop
```

To analyze a different branch without switching to it, specify the branch name with `=`:

```bash
roborev analyze refactor --branch=feature-xyz
roborev analyze duplication --branch=feature-xyz --per-file
```

Note: use `--branch=name` (with `=`), not `--branch name`. The space-separated form treats `name` as a positional file argument.

Branch mode automatically discovers changed files and filters out non-code files (`.md`, `.yml`, `.yaml`, `.json`, `.toml`, `.html`, `.css`, `.scss`). File contents are read from the git tree, not the working directory, so uncommitted edits are not included.

## Per-File Mode

By default, all files are bundled into a single analysis job. Use `--per-file` to create a separate job for each file:

```bash
roborev analyze complexity --per-file 'internal/storage/*.go'
roborev analyze refactor --per-file --wait '*.go'
```

Per-file mode is useful when:

- Total file content would be too large for a single prompt
- You want analysis jobs to run in parallel across the worker queue
- You plan to fix files one at a time

## Using Fix with Reviews

`roborev fix` runs **synchronously in the foreground**: it blocks until the agent finishes, applies changes directly to your working tree, and commits the result. It does not run in the background or queue work for the daemon.

`roborev fix` works with any review, not just analysis results. Use it to address commit reviews too:

```bash
roborev fix                        # Fix all open reviews and analysis findings on this branch
roborev fix 123                    # Fix a specific review by job ID
roborev fix 42 43 44               # Fix multiple reviews sequentially
roborev fix --batch                # Batch all open into one agent prompt
roborev fix --batch 42 43 44       # Batch specific jobs into one prompt
roborev fix --min-severity medium  # Skip low-severity findings
```

The agent reads the review findings, applies changes, commits, and closes the review. This is a one-shot fix. For an iterative loop with re-review, see [`roborev refine`](/guides/auto-fixing/).

Use `--batch` to concatenate multiple reviews into a single prompt so the agent sees all findings at once. This is faster and gives the agent more context to make coordinated fixes across related issues. Reviews are packed into batches respecting the configured max prompt size.

For finer control, use `--batch-size N` to pack up to N reviews per agent invocation. roborev will issue multiple invocations when more than N open reviews remain, with each invocation still bounded by `max_prompt_size`. This is useful when `--batch` would exceed the prompt budget but you still want coordinated multi-finding fixes per call. Pair with `--resume` to reuse the agent's session ID across calls within the run, so each invocation builds on prior context instead of starting fresh.

## Applying Fixes from Analysis

There are two ways to apply fixes from analysis results.

### Inline with `--fix`

Add `--fix` to run analysis and then immediately invoke an agent to apply the suggested changes:

```bash
roborev analyze refactor --fix ./...
roborev analyze duplication --fix --fix-agent claude-code src/
```

When combined with `--per-file`, files are analyzed and fixed sequentially:

```bash
roborev analyze complexity --per-file --fix 'internal/storage/*.go'
```

### Separately with `roborev fix`

Run analysis first, review the results, then apply fixes for specific jobs:

```bash
# Step 1: Analyze
roborev analyze test-fixtures 'internal/storage/*_test.go'
# => Job 42

# Step 2: Review findings in TUI or with roborev show
roborev show 42

# Step 3: Apply fixes
roborev fix 42
```

Fix multiple jobs sequentially:

```bash
roborev fix 42 43 44
```

Or batch them into a single agent prompt for faster, more coordinated fixes:

```bash
roborev fix --batch 42 43 44
```

When complete, the agent commits its changes and the jobs are closed.

### Batch Analyze, Then Fix

Queue multiple analysis jobs at once and let `roborev fix` work through them all:

```bash
# Queue a batch of analysis jobs
roborev analyze test-fixtures 'internal/*_test.go'
roborev analyze duplication internal/
roborev analyze complexity --per-file 'cmd/*.go'

# Fix all open reviews (analysis results are reviews too)
roborev fix

# Or batch all open into a single agent prompt
roborev fix --batch
```

Without `--batch`, `roborev fix` processes open reviews one at a time. As it commits fixes, the daemon automatically reviews the new commits, and if those new reviews fail, `roborev fix` picks them up and fixes them too. This means a single `roborev fix` invocation can work through a chain of analysis findings and their follow-on reviews without manual intervention.

With `--batch`, all open reviews are concatenated into a single prompt (respecting the configured max prompt size), so the agent fixes everything in one pass. This is particularly useful after queuing several analysis jobs.

## Severity Filtering

Use `--min-severity` to limit which findings the agent addresses:

```bash
roborev fix --min-severity high          # Only fix high and critical findings
roborev fix --min-severity medium        # Skip low-severity findings
roborev fix --batch --min-severity high  # Severity filter works with batch mode
```

The severity filter is injected into the agent prompt as an instruction. Findings below the threshold are ignored. The filter does not apply to task or analysis jobs, which have free-form output without severity labels; if a batch includes any task jobs, the severity filter is suppressed for the entire batch.

Set a default per repo in `.roborev.toml`:

```toml
fix_min_severity = "medium"
```

The CLI flag overrides the config value.

## Stream Output

When `roborev fix` or `analyze --fix` runs an agent, output is formatted as compact progress lines showing tool calls grouped with a gutter prefix:

```
│ Read   internal/gmail/ratelimit_test.go
│ Edit   internal/gmail/ratelimit_test.go
│ Bash   go test ./internal/gmail/ -run TestRateLimiter
│ Edit   internal/gmail/ratelimit_test.go
│ Grep   TODO  internal/
```

Agent text between tool calls is rendered as wrapped Markdown, adapting to your terminal width and light/dark background.

When piped (non-TTY), raw JSON is passed through for logging or scripting:

```bash
roborev fix 42 | tee fix-log.json
```

## JSON Output

Use `--json` for programmatic output, useful for agent-driven workflows:

```bash
roborev analyze refactor --json 'cmd/*.go'
```

```json
{
  "jobs": [
    {"id": 1, "agent": "codex"}
  ],
  "analysis_type": "refactor",
  "files": ["main.go"]
}
```

In `--per-file` mode, each job includes a `file` field:

```json
{
  "jobs": [
    {"id": 1, "agent": "codex", "file": "a.go"},
    {"id": 2, "agent": "codex", "file": "b.go"}
  ],
  "analysis_type": "complexity",
  "files": ["a.go", "b.go"]
}
```

## Large File Handling

When file content exceeds the configured prompt size limit (default 200KB), roborev switches to **agentic mode** automatically. Instead of embedding file contents in the prompt, it passes file paths and lets the agent read them directly.

Configure the limit in `config.toml`:

```toml
default_max_prompt_size = 204800   # bytes (default: 200KB)
```

Or per-repository in `.roborev.toml`:

```toml
max_prompt_size = 204800
```

## `roborev analyze` Flags

| Flag | Description |
|------|-------------|
| `--agent <name>` | Agent to use for analysis (default: from config) |
| `--model <model>` | Model for analysis agent |
| `--reasoning <level>` | Reasoning depth: `fast`, `standard`, or `thorough` |
| `--branch [name]` | Analyze files changed on branch (optionally specify branch name with `--branch=name`) |
| `--base <branch>` | Base branch for `--branch` comparison (default: auto-detect) |
| `--wait` | Wait for job to complete and show result |
| `--quiet` | Suppress output (just enqueue) |
| `--per-file` | Create one job per file |
| `--fix` | Run analysis then apply fixes automatically |
| `--fix-agent <name>` | Agent for fix step (default: same as `--agent`) |
| `--fix-model <model>` | Model for fix step (default: same as `--model`) |
| `--json` | Output job info as JSON |
| `--list` | List available analysis types |
| `--show-prompt <type>` | Show the prompt template for an analysis type |

## `roborev fix` Flags

| Flag | Description |
|------|-------------|
| `--agent <name>` | Agent to use for fixes (default: from config) |
| `--model <model>` | Model for fix agent |
| `--reasoning <level>` | Reasoning depth: `fast`, `standard`, or `thorough` |
| `--quiet` | Suppress agent output |
| `--open` | Fix all open reviews on the current branch (default when no job IDs given) |
| `--batch` | Concatenate multiple reviews into a single agent prompt instead of fixing one at a time |
| `--batch-size <n>` | Pack up to N reviews into each agent invocation, bounded by `max_prompt_size`. Multiple invocations are issued when more than N reviews are open. Mutually exclusive with `--batch` and `--list`. |
| `--resume` | Reuse the agent's session ID across calls within a single fix run so chained fixes build on prior context |
| `--branch <name>` | Filter by branch (requires `--open` or `--batch`) |
| `--all-branches` | Include jobs from all branches (requires `--open` or `--batch`) |
| `--newest-first` | Process jobs newest first (requires `--open` or `--batch`) |
| `--list` | List open reviews with details without running any fixes |
| `--min-severity <level>` | Only fix findings at or above this severity (`low`/`medium`/`high`/`critical`) |

## See Also

- [Custom Agent Tasks](/advanced/custom-tasks/): Ad hoc analysis with `roborev run`
- [Auto-Fix Agentic Loop with Refine](/guides/auto-fixing/): Fix failed reviews automatically
- [Consolidating Reviews](/commands/#consolidating-reviews): Verify and deduplicate findings before fixing
- [Terminal UI](/integrations/tui/): Browse analysis results interactively
- [Configuration](/configuration/): Prompt size and agent settings
