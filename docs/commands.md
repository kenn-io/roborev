---
title: Command Cheat Sheet
description: Quick reference for all roborev commands and flags
---

<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/cli-help.svg" alt="roborev help output" loading="lazy">
</figure>

## Essentials

```bash
roborev init [--agent <name>]    # Initialize repo + daemon + hook
                                 # --no-daemon: skip auto-starting daemon
roborev fix                      # Fix open reviews
roborev daemon status            # Check daemon, web UI, and queue
roborev status                   # Backward-compatible status alias
roborev pause                    # Pause queue processing
roborev unpause                  # Resume queue processing
roborev cancel <job_id>          # Cancel one queued or running job
roborev summary                  # Aggregate review statistics
roborev cost                     # Approximate aggregate review cost
roborev insights                 # Analyze review patterns
roborev tui                      # Interactive terminal UI
                                 # --repo: pre-filter to repo
                                 # --branch: pre-filter to branch
                                 # --no-quit: suppress keyboard quit
                                 # --control-socket: custom socket path
roborev ui                       # Open the native browser application
roborev ui 42                    # Open browser review detail for local job 42
roborev version                  # Show version
roborev version --json           # Show stable machine-readable version data
```

When the binary was built without the production web assets, the human-readable
`roborev version` output appends
`(no embedded web assets; reinstall from an official release or build with 'make build')`
to the version line.

### Version JSON contract

`roborev version --json` prints one JSON object and exits successfully without
requiring a repository or a running daemon:

```json
{"name":"roborev","version":"v0.62.1","web_assets":true}
```

The stable fields are:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Canonical tool name; always `roborev` |
| `version` | string | Build version, using the release semantic version for release builds |
| `web_assets` | bool | Whether this binary embeds the production web assets required to serve the [browser UI](/web-ui/) |

Consumers should ignore additional fields so the contract can grow compatibly.

## Reviewing Code

```bash
# Single commits
roborev review                   # Review HEAD
roborev review <sha>             # Review specific commit

# Commit ranges
roborev review <start> <end>     # Review range (inclusive)
roborev review --since <commit>  # Review since commit (exclusive)
roborev review --since HEAD~5    # Review last 5 commits

# Branch reviews
roborev review --branch          # All commits since diverged from main
roborev review --branch=feature-xyz     # Review a specific branch
roborev review --branch --base develop  # Against specific base

# Uncommitted changes
roborev review --dirty           # Review working tree

# Review types
roborev review --type security   # Security-focused review
roborev review --type design     # Design-focused review
roborev review --type lookahead  # Time-series look-ahead bias review

# Review panels
roborev review --branch --panel branch_final  # Run a named review panel
roborev review --branch --panel none          # Force single-agent review
```

| Flag | Description |
|------|-------------|
| `--wait` | Block until review completes (intended for CI, scripting, and orchestrators; not recommended in interactive agent sessions) |
| `--quiet` | Only show progress/elapsed time |
| `--branch [name]` | Review all commits on branch since base (optionally specify branch name) |
| `--base <branch>` | Base branch for `--branch` comparison (default: auto-detect) |
| `--agent <name>` | Use a specific agent for review: a built-in (`codex`, `claude-code`, `gemini`, `copilot`, `opencode`, `cursor`, `kiro`, `kilo`, `droid`, `pi`, `grok`) or a configured ACP agent |
| `-m, --model <model>` | Model to use (format varies by agent) |
| `--type <type>` | Review type (`security`, `design`, `lookahead`); changes system prompt |
| `--reasoning <level>` | Set a reasoning level; prefer exact `low`/`medium`/`high`/`xhigh`/`max`, while legacy presets remain supported. See [Reasoning Levels](/configuration/#reasoning-levels) |
| `--fast` | Legacy shorthand for `--reasoning fast` |
| `--min-severity <level>` | Only report findings at or above this severity (`low`/`medium`/`high`/`critical`) |
| `--panel <name or none>` | Run a named review panel. Use `none` to bypass configured defaults |
| `--local` | Run review locally without the daemon (streams output to console) |
| `--repo <path>` | Specify repository path |

See: [Reviewing Code](/guides/reviewing-code/)

## Waiting for Reviews

```bash
roborev wait                     # Wait for most recent job for HEAD
roborev wait abc123              # Wait for most recent job for commit
roborev wait 42                  # Job ID (if "42" is not a valid git ref)
roborev wait 42 43 44            # Wait for multiple jobs
roborev wait --job 42            # Force as job ID
roborev wait --sha HEAD~1        # Wait for job matching HEAD~1
roborev wait --quiet             # Suppress output (for hooks/agents)
```

| Flag | Description |
|------|-------------|
| `--sha <ref>` | Git ref to find the most recent job for |
| `--job` | Force argument to be treated as job ID |
| `-q, --quiet` | Suppress output (exit code only) |

Unlike `roborev review --wait`, this does not enqueue a new review. It waits for
an already-running job, making it useful when a post-commit hook has already
triggered the review. For most interactive workflows, use `roborev tui` to
browse completed reviews instead of blocking.

See: [Reviewing Code](/guides/reviewing-code/)

## Viewing Reviews

```bash
roborev show                     # Show review for HEAD
roborev show <sha>               # Show review for commit
roborev show <job_id>            # Show review by job ID
roborev show --job <id>          # Force interpretation as job ID
roborev show --prompt <job_id>   # Show the prompt sent to the agent
roborev list                     # List jobs for current repo/branch
roborev list --open              # List only open reviews
roborev list --closed            # List only closed reviews
roborev tui                      # Interactive terminal UI
roborev tui --repo --branch      # Pre-filtered to current repo+branch
roborev ui                       # Open the browser review workspace
roborev ui 42                    # Deep-link to browser review detail
roborev log <job_id>             # View job log
```

| Flag | Description |
|------|-------------|
| `--job` | Force interpretation as job ID |
| `--prompt` | Show the prompt sent to the agent instead of the review output |
| `--json` | Output as JSON for machine-readable workflows |

When the argument is a numeric job ID, `--prompt` can display the stored prompt
while the job is queued or running; review output does not exist until the job
completes.

`roborev show` displays review comments after the review output when comments
exist, matching the layout in the TUI review detail view.

`roborev ui` starts the daemon when needed, reads the browser origin and path
prefix from the live daemon runtime, and opens `/reviews` below that prefix when
one is configured. An optional positive numeric job ID opens `/reviews/<job-id>`
below the same prefix. Job IDs are local to that daemon's SQLite database, so a
numeric deep link is not portable to another machine even when review data is
synchronized. Authentication tokens are never placed in the launch URL. The
browser listener is enabled on loopback by default, so an installed release
needs no additional configuration for local use: run `roborev ui` and the
application displays the reviews from the same SQLite database used by the CLI
and terminal UI.

See [Browser UI](/web-ui/) for analytics definitions, remote HTTPS access, and
the production Tailscale Serve setup.

For panel parent reviews, `roborev show` also displays a one-line reviewer
summary. `roborev show --json` includes an additive `panel` object with the run
UUID, panel name, synthesis job ID, and member reviewer statuses.

See: [Terminal UI](/integrations/tui/)

!!! tip

    Press `l` in the TUI to open the log viewer for any job (running or completed).

## Canceling a Job

```bash
roborev cancel 42                # Cancel job 42 if it is queued or running
```

`roborev cancel` accepts one positive numeric job ID. It asks the daemon to
cancel that job and prints `Job 42 canceled` when successful. Jobs that have
already reached a terminal state, including done, failed, or canceled jobs,
cannot be canceled and return an error.

## Exporting Reviews

```bash
roborev export reviews
roborev export reviews --profile metadata
roborev export reviews --since 2026-06-01 --until 2026-06-30
roborev export reviews --closed-only --repo github.com/org/repo
roborev export reviews --project my-workspace --limit 1000
roborev export reviews --cursor "$NEXT_CURSOR" --until 2026-07-01
```

| Flag | Description |
|------|-------------|
| `--format json` | Output format. JSON is the only supported format and the default |
| `--profile content\|metadata` | Export profile. `content` is the default |
| `--since <time>` | Inclusive `completed_at` lower bound. Accepts RFC3339 or `YYYY-MM-DD` |
| `--until <time>` | Exclusive `completed_at` upper bound. Accepts RFC3339 or `YYYY-MM-DD` |
| `--cursor <opaque>` | Resume strictly after a previous `next_cursor`. Mutually exclusive with `--since` |
| `--closed-only` | Include only reviews you have marked resolved |
| `--repo <id>` | Exact exported repo identifier, usually `github.com/org/repo` |
| `--project <name>` | Exact local project/workspace label |
| `--limit <n>` | Maximum top-level reviews to emit |

`roborev export reviews` emits one JSON document containing completed reviews.
The default `content` profile includes the raw review output text exactly as
stored, subject to a large size cap. The `metadata` profile keeps the same
review metadata but sets `content` fields to `null`.

Only finished review jobs with a verdict are exported. Task, fix, insights,
compact, queued, running, failed, and canceled jobs are excluded. Panel reviews
export as one top-level synthesis review with completed member reviews nested
under `subagents`; member reviews do not appear as separate top-level rows.

The export window filters on `completed_at`. Date-only bounds are interpreted as
UTC days, so `--since 2026-06-01 --until 2026-06-30` includes reviews from the
start of June 1 through the end of June 30 UTC. When `--limit` is omitted, the
CLI follows daemon cursors until all matching rows are included in the one JSON
document. With `--limit`, the CLI still pages through bounded daemon responses
until the requested top-level count is reached or no more rows match.

Export documents use `schema_version: 1` and include a stable `database_id` for
the local review database. Adding `database_id` does not bump `schema_version`
because it is an additive header field and existing consumers must continue to
ignore unknown header keys.

Rows are ordered by `(completed_at, review_id)` ascending. `next_cursor` is an
opaque, internally versioned token containing that compound position and the
`database_id`; version 1 cursors are stable across invocations and roborev
upgrades, and future cursor encoding changes must keep old cursor versions
resolvable or reject them clearly. Every non-empty export includes a
`next_cursor`, even when `truncated` is `false`; `truncated` only means more
matching rows are available immediately. `--cursor <opaque>` resumes strictly
after the cursor position, cannot be combined with `--since`, and still honors
`--until`, `--limit`, `--profile`, `--closed-only`, `--repo`, and `--project`. A
malformed, corrupt, stale, or no-longer-resolvable cursor fails instead of
silently producing a full or empty export. Consumers should treat any cursor
rejection as a signal to discard the cursor and retry with a completed-at window
backfill. A cursor from a different `database_id` is rejected distinctly as a
database reset, and the CLI exits with code `3` for that case so shell callers
can branch without parsing stderr.

Cursor resume is not an overlap scan. A review that completes later with
`completed_at` earlier than an already consumed cursor position will not be
returned by cursor resume. Consumers that need convergence for late-completing
reviews should run their own overlapping completed-at window separately.

!!! warning "Review content may be sensitive"

    The `content` profile exports raw review output as stored. Review text can
    include repository-specific details or other sensitive context. Use
    `--profile metadata` when you do not need review prose, and handle content
    exports with the same care as local review data.

## Exporting CI Metrics

```bash
roborev export ci-metrics
roborev export ci-metrics --since 2026-07-01 --until 2026-07-31
roborev export ci-metrics --limit 1000
roborev export ci-metrics --cursor "$NEXT_CURSOR" --until 2026-08-01
roborev export ci-metrics --legacy
```

| Flag | Description |
|------|-------------|
| `--format json` | Output format. JSON is the only supported format and the default |
| `--since <time>` | Inclusive `posted_at` lower bound. Accepts RFC3339 or `YYYY-MM-DD` |
| `--until <time>` | Exclusive `posted_at` upper bound. Accepts RFC3339 or `YYYY-MM-DD` |
| `--cursor <opaque>` | Resume strictly after a previous `next_cursor`. Mutually exclusive with `--since` |
| `--limit <n>` | Maximum panels to emit |
| `--legacy` | Export the frozen pre-panel CI era as a one-time backfill instead of panel runs |

`roborev export ci-metrics` emits one JSON document containing finalized CI
panel runs for external turnaround-time analysis. Each panel records its GitHub
repository, pull request, head SHA, creation and posting times, first-attempt
time, attempt count, terminal outcome, synthesis agent/model snapshot, and the
retained member and synthesis jobs with their timing and model metadata.

Terminal outcomes distinguish `review_posted`, `no_review_posted`,
`giveup_posted`, and `abandoned`. roborev backfills metrics for older finalized
panels from retained jobs and attempts when the daemon starts. If the source
rows have already been deleted, the panel remains exportable with outcome
`unknown` and unavailable fields set to `null`.

Export documents use `schema_version: 1` and the same stable `database_id`
contract as review exports. Rows are ordered by `(posted_at, panel_id)`
ascending. Every non-empty export includes an opaque `next_cursor`; pass it to
`--cursor` to resume strictly after the last panel. A cursor from a recreated
database is rejected with exit code `3`, so callers can discard it and backfill
from a time window. Other cursor rejections also require discarding the cursor.
Date-only `--until` bounds include the full named UTC day.

`--legacy` exports the frozen pre-panel CI era as `legacy_review` pseudopanels,
grouped from related completed review jobs before the database's first panel
activity. It is intended for a one-time historical backfill. Legacy and
panel-era cursors are namespaced and cannot be resumed against each other.

## Exporting CI Costs

```bash
roborev export ci-costs
roborev export ci-costs --since 2026-07-01 --until 2026-07-31
roborev export ci-costs --limit 1000
roborev export ci-costs --cursor "$NEXT_CURSOR" --until 2026-08-01
roborev export ci-costs --legacy
```

| Flag | Description |
|------|-------------|
| `--format json` | Output format. JSON is the only supported format and the default |
| `--since <time>` | Inclusive `finished_at` lower bound. Accepts RFC3339 or `YYYY-MM-DD` |
| `--until <time>` | `finished_at` upper bound. RFC3339 is exclusive; `YYYY-MM-DD` includes that full UTC day |
| `--cursor <opaque>` | Resume strictly after a previous `next_cursor`. Mutually exclusive with `--since` |
| `--limit <n>` | Maximum jobs to emit |
| `--legacy` | Export the frozen pre-panel CI era as a one-time backfill instead of panel jobs |

`roborev export ci-costs` emits one JSON document containing job-level cost
records for CI review work. Eligible terminal attempts are included even when a
later retry replaced them in a panel. Skipped, passthrough, pre-agent, and
manual jobs are excluded. Each row contains `job_uuid`, `finished_at`, `agent`,
`role`, `status`, and `cost_usd`.

Cost is approximate and can be partial. A job whose agent ran but whose usage
cannot be priced remains in the export with `cost_usd: null`. A known zero-cost
job uses `0`, which is distinct from missing pricing. Consumers can therefore
measure cost coverage without dropping eligible work.

Rows are ordered by `(finished_at, job_id)` ascending for stable pagination. A
fresh export over an overlapping window returns current pricing for every
matching job, so an idempotent consumer can pick up prices stored or backfilled
after an earlier export. Documents use the same stable `database_id`, opaque
cursor, and exit-code `3` database-reset behavior as the other exports. Without
`--limit`, the CLI follows cursors and emits all matching rows in one document.
With `--limit`, it stops after the requested number and preserves the daemon's
`truncated` and `next_cursor` fields for resumption.

`--legacy` exports structurally identified CI review jobs from the frozen
pre-panel era. It is intended for a one-time historical backfill. Legacy and
panel-era cursors are namespaced and cannot be resumed against each other.

## Job Logs

```bash
roborev log <job-id>             # Human-friendly rendered output
roborev log --raw <job-id>       # Raw NDJSON bytes
roborev log --path <job-id>      # Print the log file path
roborev log --db <path> <job-id> # Read metadata from a custom database

roborev log clean                # Remove logs older than 7 days
roborev log clean --days 3       # Remove logs older than 3 days
```

| Flag | Description |
|------|-------------|
| `--raw` | Print raw NDJSON without formatting |
| `--path` | Print the log file path instead of contents |
| `--db` | SQLite database used for log metadata |

Job logs are persisted to `~/.roborev/logs/jobs/` so agent output remains
available after daemon restarts. By default, `roborev log` renders NDJSON into
compact, human-readable progress lines showing tool calls and agent text. It
uses stored job metadata to select that renderer; use `--raw` for the original
NDJSON when scripting, debugging, or reading an orphaned log file. When the
daemon runs with a custom `--db` path, pass the same path to `roborev log`.

The `clean` subcommand removes log files older than the specified number of days
(default: 7).

## Commenting on Reviews

```bash
roborev comment <job_id> "message"   # Add comment with message
roborev comment <job_id>             # Opens editor
roborev close <job_id>               # Mark as closed
roborev close <job_id> --reopen      # Reopen a closed review
```

| Flag | Description |
|------|-------------|
| `--message, -m` | Comment message (inline) |
| `--commenter` | Name of commenter |
| `--job` | Force interpretation as job ID |

!!! note

    `roborev address` is still accepted as an alias for `roborev close`.

See: [Responding to Reviews](/guides/responding-to-reviews/)

## Auto-Fix Agentic Loop

```bash
roborev refine                   # Fix failed reviews on branch
roborev refine --max-iterations 5
roborev refine --since HEAD~3    # Refine specific range
roborev refine --quiet           # Show elapsed time only
roborev refine --list            # Preview what would be refined
roborev refine --all-branches    # Refine all branches with failures
roborev refine --branch feature  # Validate branch before refining
roborev refine --min-severity high  # Only fix high and critical findings
```

| Flag | Description |
|------|-------------|
| `--agent <name>` | Use specific agent |
| `-m, --model <model>` | Model to use (format varies by agent) |
| `--reasoning <level>` | Set reasoning depth |
| `--fast` | Shorthand for `--reasoning fast` |
| `--max-iterations <n>` | Limit fix attempts (default: 10) |
| `--since <commit>` | Refine commits since specific commit |
| `--branch <name>` | Validate current branch before refining |
| `--all-branches` | Discover and refine all branches with failed reviews (implies `--open`) |
| `--list` | List failed reviews that would be refined without running |
| `--newest-first` | Process newest first (requires `--all-branches` or `--list`) |
| `--quiet` | Only show progress/elapsed time |
| `--allow-unsafe-agents` | Allow agents without sandboxing |
| `--min-severity <level>` | Only fix findings at or above this severity (`low`/`medium`/`high`/`critical`) |

`refine` creates its own fix commits, so `fix_commit_author` and
`fix_commit_co_authored_by` are applied directly with Git's `--author` and
`--trailer` options. See
[Fix Commit Metadata](/configuration/#fix-commit-metadata).

See: [Auto-Fix Agentic Loop with Refine](/guides/auto-fixing/)

## Fixing Reviews

```bash
roborev fix                        # Fix all open reviews on this branch
roborev fix 123                    # Fix a specific review by job ID
roborev fix 42 43 44               # Fix multiple reviews sequentially
roborev fix --batch                # Batch all open into one agent prompt
roborev fix --batch 42 43 44       # Batch specific jobs into one prompt
roborev fix --batch-size 5         # Pack up to 5 reviews per agent invocation
roborev fix --resume               # Reuse agent session across calls
roborev fix --branch main          # Fix all open on a specific branch
roborev fix --all-branches         # Fix all open across all branches
roborev fix --list                 # List open reviews without fixing
roborev fix --min-severity medium  # Skip low-severity findings
```

| Flag | Description |
|------|-------------|
| `--agent <name>` | Use specific agent |
| `-m, --model <model>` | Model to use |
| `--reasoning <level>` | Set reasoning depth |
| `--quiet` | Suppress agent output |
| `--branch <name>` | Filter by branch (default: current branch) |
| `--all-branches` | Include open jobs from all branches |
| `--batch` | Concatenate multiple reviews into a single agent prompt instead of fixing one at a time |
| `--batch-size <n>` | Pack up to N reviews into each agent invocation, bounded by `max_prompt_size`. Multiple invocations are issued when more than N reviews are open. Mutually exclusive with `--batch` and `--list`. |
| `--resume` | Reuse the agent's session ID across calls within a single fix run so chained fixes build on prior context |
| `--list` | List open reviews with details (job ID, ref, branch, agent, verdict) without running any fixes |
| `--newest-first` | Process jobs newest first instead of oldest first |
| `--min-severity <level>` | Only fix findings at or above this severity (`low`/`medium`/`high`/`critical`) |

For foreground `fix` and `analyze --fix` flows, the selected agent owns the
commit. `fix_commit_author` and `fix_commit_co_authored_by` are included as
prompt instructions only, so agent-level Git config can still add its own
trailers. See [Fix Commit Metadata](/configuration/#fix-commit-metadata).

See: [Assisted Refactoring](/guides/assisted-refactoring/)

## Consolidating Reviews

```bash
roborev compact                              # Enqueue consolidation (background)
roborev compact --wait                       # Wait for completion
roborev compact --branch main                # Compact jobs on main branch
roborev compact --all-branches               # Compact jobs across all branches
roborev compact --dry-run                    # Show what would be done
roborev compact --limit 10                   # Process at most 10 jobs
roborev compact --agent claude-code          # Use specific agent for verification
roborev compact --reasoning thorough         # Use thorough reasoning level
```

| Flag | Description |
|------|-------------|
| `--wait` | Block until consolidation completes |
| `--branch <name>` | Filter by branch (default: current branch) |
| `--all-branches` | Compact jobs across all branches |
| `--dry-run` | Preview what would be done without running |
| `--limit <n>` | Maximum number of jobs to process (default: 20) |
| `--agent <name>` | Agent for verification |
| `-m, --model <model>` | Model to use |
| `--reasoning <level>` | Set reasoning depth (`thorough`/`standard`/`fast`) |
| `--timeout <duration>` | Timeout for `--wait` mode (default: 10m) |
| `--quiet` | Suppress progress output |

Compact discovers open completed reviews, sends them to an agent for
verification against the current codebase, and consolidates related findings
into a single review job. Original jobs are automatically closed when
consolidation finishes. This adds a quality layer between `review` and `fix` to
reduce false positives.

If the verification reports that findings remain, `compact` requires each one to
be repeated with actionable detail (severity, file/line, description). Outputs
that mention remaining findings only as counts or summaries are rejected and the
job fails, rather than producing a review that cannot be acted on. A clean
verification with no remaining findings is still recorded as a review.

!!! note

    Avoid running multiple compact commands concurrently on the same branch. The
    operation is not atomic and concurrent runs can produce inconsistent state.

## Review Statistics

```bash
roborev summary                     # Last 7 days, current repo
roborev summary --all               # Last 7 days, all repos
roborev summary --since 30d         # Last 30 days
roborev summary --branch main       # Filter by branch
roborev summary --repo /path/to/repo
roborev summary --json              # Structured output for scripting
```

| Flag | Description |
|------|-------------|
| `--since <duration>` | Time window (e.g. `24h`, `7d`, `30d`; default: `7d`) |
| `--branch <name>` | Scope to a single branch |
| `--repo <path>` | Scope to a single repo (default: current repo) |
| `--all` | Show summary across all repos (mutually exclusive with `--repo`) |
| `--json` | Structured output for scripting |

The summary includes:

- **Overview**: Job counts by status (done, failed, canceled, queued, running)
- **Verdicts**: Pass/fail counts, pass rate, and resolution rate for addressed
    failures
- **Agent breakdown**: Per-agent job counts, pass rate, and median review
    duration
- **Duration**: Review and queue time percentiles (p50, p90, p99)
- **Job types**: Counts by job type (review, fix, task, etc.)
- **Repos** (with `--all`): Per-repo breakdown with pass/fail/addressed counts
- **Failures**: Total failures, retries, and error categories
- **Cost**: Approximate agent spend for eligible jobs in the same time window,
    with coverage when only some jobs reported cost

## Aggregate Cost

```bash
roborev cost                     # All-time, current repo
roborev cost --all               # All-time, all repos
roborev cost --branch main       # Filter by branch
roborev cost --repo /path/to/repo
roborev cost --since 30d         # Last 30 days
roborev cost --json              # Structured output for scripting
```

| Flag | Description |
|------|-------------|
| `--since <duration>` | Time window (e.g. `24h`, `7d`, `30d`; default: all time) |
| `--branch <name>` | Scope to a single branch |
| `--repo <path>` | Scope to a single repo (default: current repo) |
| `--all` | Aggregate across all repos (mutually exclusive with `--repo`) |
| `--json` | Structured output for scripting |

Cost is approximate and partial by design. roborev sums stored `cost_usd` values
from jobs where an agent actually ran, so the result is a lower bound when some
agents or models do not report cost. Human output shows coverage, for example
`Approx cost: ~$12.50  (8/10 jobs reported cost)`. JSON output includes
`total_usd`, `jobs_with_cost`, `jobs_total`, and `complete`.

See [Token Usage](#token-usage) for how per-job token and cost data is
collected.

## Insights

```bash
roborev insights                          # Analyze last 30 days
roborev insights --since 7d              # Last 7 days
roborev insights --branch main           # Filter to main branch
roborev insights --repo /path/to/repo    # Specific repository
roborev insights --agent gemini          # Use specific agent
roborev insights --wait=false            # Enqueue without waiting
roborev insights --json                  # Output as JSON
```

| Flag | Description |
|------|-------------|
| `--since <duration>` | Time window for reviews (e.g. `7d`, `30d`, `90d`, `2w`; default: `30d`) |
| `--branch <name>` | Scope to a single branch |
| `--repo <path>` | Scope to a single repo (default: current repo) |
| `--agent <name>` | Agent to use for analysis |
| `-m, --model <model>` | Model to use |
| `--reasoning <level>` | Set reasoning depth (`thorough`/`standard`/`fast`) |
| `--wait` | Wait for completion and display result (default: true) |
| `--json` | Output job info as JSON |

Analyzes failing code reviews to identify recurring patterns and suggest
improvements to review guidelines. The command queries completed reviews
(focusing on failures) within the time window, includes the currently resolved
`review_guidelines` from global and repo config as context, and sends the batch
to an agent for structured analysis.

The agent produces:

- Recurring finding patterns across reviews
- Hotspot areas (files/packages with concentrated failures)
- Noise candidates (findings consistently dismissed without code changes)
- Guideline gaps (patterns flagged by reviews but not covered by guidelines)
- Suggested guideline additions (concrete text for `.roborev.toml` or
    `~/.roborev/config.toml`)

If no failing reviews match the time window and branch filter, the command exits
with a message instead of queuing a job.

## Token Usage

Token usage is tracked automatically for completed jobs when `agentsview` is
installed. Usage appears in the TUI review header and `roborev show` output
(e.g. `118.0k ctx · 28.8k out`).

When agentsview 0.30.0 or newer is installed, the usage summary also includes a
model-pricing cost estimate (e.g. `118.0k ctx · 28.8k out · ~$0.42`), and the
TUI queue displays a default-visible "Cost" column with the per-job estimate.
Older agentsview versions still record token counts; the cost column stays blank
for unpriced models and for jobs whose usage has not yet been fetched. The tilde
marks the value as a model-pricing estimate rather than a billed amount.

Fresh agent sessions can finish before agentsview has indexed their final usage.
roborev briefly retries a missing session lookup before storing the job-log
token fallback. The daemon then continues reconciling recent unpriced jobs (up
to a week old) in the background, including after a restart, so a price that
appears later is stored without an operator running a backfill. If the database
missed the session ID, the daemon first recovers it from the per-job JSONL log
when that log is still available.

If you run a central usage service, configure `[cost] endpoint` to fetch usage
over HTTP instead of through the local `agentsview` CLI. See
[Cost Usage Endpoint](/configuration/#cost-usage-endpoint).

To backfill token data for older jobs:

```bash
roborev backfill-tokens             # Fetch token data for all eligible jobs
roborev backfill-tokens --dry-run   # Preview without writing
```

| Flag | Description |
|------|-------------|
| `--dry-run` | Preview candidates without fetching or storing data |

The backfill scans eligible terminal jobs that need token or price data. This
includes failed, canceled, and skipped jobs when an agent ran. Jobs whose
session files have been deleted are skipped. Sessions reused by more than one
started job are also skipped because their cumulative usage cannot be assigned
safely to one job.

## CI Review

```bash
roborev ci review                            # Auto-detect from GitHub Actions / GitLab CI env
roborev ci review --ref HEAD~3..HEAD         # Explicit ref range
roborev ci review --gh-repo myorg/myrepo --pr 42  # Explicit GitHub repo and PR
roborev ci review --gl-repo mygroup/myproject --pr 42  # Explicit GitLab project and MR
roborev ci review --agent codex,gemini        # Multiple agents
roborev ci review --comment                  # Post results as PR/MR comment
```

| Flag | Description |
|------|-------------|
| `--ref <range>` | Git ref or range to review (default: auto-detect from `GITHUB_REF` or `CI_COMMIT_SHA`) |
| `--comment` | Post results as a PR comment (GitHub) or MR note (GitLab) |
| `--gh-repo <owner/repo>` | GitHub repo (default: `GITHUB_REPOSITORY` env var) |
| `--gl-repo <group/project>` | GitLab project path, subgroups allowed (default: `CI_MERGE_REQUEST_PROJECT_PATH`, then `CI_PROJECT_PATH`); mutually exclusive with `--gh-repo` |
| `--gl-host <url>` | GitLab server URL or hostname; selects GitLab (default: `CI_SERVER_URL`, then `GITLAB_HOST`/`GL_HOST`); hardcode it in the job script when `GITLAB_TOKEN` is protected; mutually exclusive with `--gh-repo` |
| `--pr <number>` | PR number / GitLab MR IID (default: `GITHUB_EVENT_PATH` or `CI_MERGE_REQUEST_IID`) |
| `--agent <names>` | Agents to use (comma-separated, default: auto-detect) |
| `--review-types <types>` | Review types to run (comma-separated: `security`, `design`, `lookahead`, `default`) |
| `--reasoning <level>` | Legacy (`fast`/`standard`/`thorough`/`maximum`) or exact (`low`/`medium`/`high`/`xhigh`/`max`) reasoning |
| `--min-severity <level>` | Minimum severity to report (`low`/`medium`/`high`/`critical`) |
| `--upsert-comments` | Update the previous roborev comment instead of adding one (overrides `[ci] upsert_comments`) |
| `--synthesis-agent <name>` | Agent for combining multi-job results |

Runs a one-shot review without a daemon or database. Designed for CI pipelines
where you want review results as part of the build, not as a background service.
With `--comment`, roborev calls the forge only when at least one completed agent
produced substantive review output. Empty responses and roborev's empty-output
placeholder do not qualify. If every agent fails before producing output, the
command writes its diagnostic summary to the CI log, makes no comment request,
and exits nonzero for actionable failures. An all-quota batch keeps its existing
successful exit because there is no actionable review failure.

In GitHub Actions, `ci review` auto-detects `GITHUB_REPOSITORY`, `GITHUB_REF`,
and `GITHUB_EVENT_PATH` so you can run it with no flags. Outside GitHub Actions,
pass `--gh-repo` and `--ref` explicitly.

In GitLab CI, `ci review` auto-detects the project
(`CI_MERGE_REQUEST_PROJECT_PATH`, the merge request's target project, falling
back to `CI_PROJECT_PATH`), `CI_MERGE_REQUEST_IID`, `CI_SERVER_URL`, and the
review range: `CI_MERGE_REQUEST_DIFF_BASE_SHA` as the base and
`CI_MERGE_REQUEST_SOURCE_BRANCH_SHA` (merged results pipelines) or
`CI_COMMIT_SHA` as the head. Forge detection precedence is: explicit flags
(`--gl-repo`/`--gl-host` for GitLab, `--gh-repo` for GitHub), then GitLab CI
(`GITLAB_CI=true`), then GitHub Actions (`GITHUB_ACTIONS=true`) — the GitLab
indicator wins when both are set because a GitLab pipeline starter can inject
`GITHUB_ACTIONS=true`, while the reverse requires committed workflow code.
Posting an MR note requires a project access token with the `api` scope exposed
as `GITLAB_TOKEN` — `CI_JOB_TOKEN` cannot create notes. When that token is a
protected variable, hardcode `--gl-host https://gitlab.example.com` in the job
script: `CI_SERVER_URL` is overridable by whoever starts the pipeline, the
script is not. See the [GitLab Integration](/integrations/gitlab/) trust model.

Exit codes: `0` on success or when all agents were skipped due to quota
exhaustion, non-zero on real failures.

See: [GitHub Integration](/integrations/github/),
[GitLab Integration](/integrations/gitlab/)

## GitHub Actions Setup

```bash
roborev init gh-action                          # Generate workflow file
roborev init gh-action --agent codex            # Specify agents
roborev init gh-action --output .github/workflows/review.yml
roborev init gh-action --force                  # Overwrite existing
roborev init gh-action --roborev-version 0.34.0 # Pin version
```

| Flag | Description |
|------|-------------|
| `--agent <names>` | Agents to include in the workflow (repeatable) |
| `--output <path>` | Output path (default: `.github/workflows/roborev.yml`) |
| `--force` | Overwrite an existing workflow file |
| `--roborev-version <ver>` | Pin roborev version in the workflow (default: latest) |

Generates a GitHub Actions workflow that:

1. Checks out the repository
1. Downloads and installs roborev with SHA256 verification
1. Runs `roborev ci review --comment` on each PR
1. Posts review results as PR comments

Agent API keys are read from repository secrets (e.g. `ANTHROPIC_API_KEY` for
Claude Code, `OPENAI_API_KEY` for Codex). Add the required secrets in your
repository's Settings > Secrets and variables > Actions.

See: [GitHub Integration](/integrations/github/)

## Code Analysis

```bash
roborev analyze duplication ./...                   # Find duplication
roborev analyze refactor --fix *.go                # Suggest and apply refactors
roborev analyze complexity --per-file src/*.go      # One job per file
roborev analyze test-fixtures internal/*_test.go   # Find test helper opportunities
roborev analyze refactor --branch                  # Analyze changed files on branch
roborev analyze refactor --branch=feature-xyz      # Analyze a specific branch
roborev analyze security ./...                     # Security-focused analysis
roborev analyze --list                             # List analysis types
```

| Flag | Description |
|------|-------------|
| `--agent <name>` | Use specific agent |
| `-m, --model <model>` | Model to use |
| `--reasoning <level>` | Set reasoning depth |
| `--wait` | Wait for completion and display result |
| `--quiet` | Suppress output |
| `--branch [name]` | Analyze files changed on branch (optionally specify branch name) |
| `--base <branch>` | Base branch for `--branch` comparison (default: auto-detect) |
| `--per-file` | One analysis job per file |
| `--fix` | Analyze then apply fixes automatically |
| `--fix-agent <name>` | Agent for fix step |
| `--json` | Output job info as JSON |
| `--list` | List available analysis types |
| `--show-prompt <type>` | Show prompt template |

See: [Assisted Refactoring](/guides/assisted-refactoring/)

## Custom Agent Tasks

```bash
roborev run "Explain the architecture"
roborev run --wait "Review src/auth/ for security issues"
roborev run "Find simplification opportunities in src/utils/"
roborev run --agentic "Add input validation to all endpoints"
roborev run --json "Explain the architecture"
cat review-checklist.txt | roborev run --wait
```

| Flag | Description |
|------|-------------|
| `--wait` | Wait for completion and display result |
| `--quiet` | Only show progress/elapsed time |
| `--agent <name>` | Use specific agent |
| `--reasoning <level>` | Set reasoning depth |
| `--agentic, --yolo` | Enable agentic mode (can modify files) |
| `--no-context` | Don't include repository context |
| `--label <string>` | Custom label displayed in TUI (default: `run`) |
| `--json` | Emit one launch receipt with `job_id`, `job_uuid`, `git_ref`, and `status`; incompatible with `--quiet`, `--wait`, and global `--verbose` |

See: [Custom Agent Tasks](/advanced/custom-tasks/)

## Configuration

```bash
roborev config get <key>             # Get value (merged: local then global)
roborev config get <key> --global    # Get from global config only
roborev config get <key> --local     # Get from repo config only

roborev config set <key> <value>     # Set in repo config (default)
roborev config set <key> <value> --global  # Set in global config

roborev config list                  # List merged config
roborev config list --show-origin    # Show where each value comes from
```

| Flag | Description |
|------|-------------|
| `--global` | Use global config (`~/.roborev/config.toml`) |
| `--local` | Use repo config (`.roborev.toml`) |
| `--show-origin` | Show origin column (global/local/default) in list output |

See: [Configuration](/configuration/)

## Repository Management

```bash
roborev repo list                       # List all repos
roborev repo show <name>                # Show repo details
roborev repo rename <old> <new>         # Rename display name
roborev repo move <name-or-path> <new>  # Update root path after a directory move
roborev repo delete <name>              # Remove from tracking
roborev repo merge <src> <dst>          # Merge reviews between repos
```

See: [Repository Management](/guides/repository-management/)

## Daemon & Hooks

```bash
roborev daemon start             # Start background daemon
roborev daemon stop              # Stop daemon
roborev daemon restart           # Restart daemon
roborev daemon status            # Show daemon, web UI, and queue status
roborev daemon run               # Run in foreground
roborev pause                    # Pause queue processing
roborev unpause                  # Resume queue processing
roborev cancel <job_id>          # Cancel one queued or running job

roborev status                   # Backward-compatible status alias
roborev daemon status --json     # Structured status for scripting

roborev post-commit              # Hook entry point (called by git hook)
roborev install-hook             # Install post-commit hook
roborev install-hook --force     # Overwrite existing hook with a fresh one
roborev uninstall-hook           # Remove hook
```

| Flag | Description |
|------|-------------|
| `--json` | Emit daemon, web UI, and queue status as JSON. Includes the canonical browser origin as `web_url` (with `web_disabled_reason` set to `config` or `missing-web-assets` when the browser listener is not running), active snoozes under `daemon.active_snoozes`, and the active daemon endpoint as `network`, `address`, and `port` fields alongside queue counters and version fields |
| `--force` | Overwrite an existing post-commit hook with a fresh one |

`roborev daemon start`, `roborev daemon restart`, and `roborev daemon status`
print the canonical browser URL. If the running daemon has no browser listener,
they explain why when the daemon published a reason —
`Web UI: disabled (this build has no embedded web assets; reinstall from an official release)`
or `Web UI: disabled ([web] enabled = false)` — and print `Web UI: unavailable`
otherwise, instead of silently omitting the application. The older
`roborev status` command remains an alias with identical output.

When Agent Hook reminders are snoozed, `roborev daemon status` lists every
active scope with its repository, exact worktree, branch, and local expiry time.
The section is omitted when no snoozes are active. JSON output exposes the same
records under `daemon.active_snoozes`.

If daemon access is denied, `roborev daemon status` reports the status as
unavailable and suggests allowing loopback or Unix-socket access when running in
a sandbox. It does not treat permission denial as proof that the daemon is
stopped, and it does not start or restart the daemon. JSON output keeps
`running: true` and includes the access error.

`pause` and `unpause` are daemon-wide queue controls. Pausing prevents workers
from starting new queued jobs, but running jobs continue to completion. A paused
queue survives daemon restarts and is shown in `roborev daemon status` and the
TUI. Use `cancel` when you need to stop one queued or running job instead of
pausing the whole queue.

Daemon shutdown also stops workers from claiming new jobs. If work is active,
restart reports that it is waiting, lets running jobs and worker finalization
finish without a timeout, and only then starts the replacement daemon.

!!! tip "Broken post-commit hook?"

    If your post-commit hook was corrupted during a previous upgrade (e.g. a stray
    `fi` or missing lines), run:

    ```bash
    roborev install-hook --force
    ```

    This replaces the hook entirely with a known-good version.

### Post-Commit Hook Entry Point

`roborev post-commit` is the command the git hook calls after each commit. You
do not need to run it manually. It silently exits on any error so hooks never
block commits.

```bash
roborev post-commit              # Called by the git post-commit hook
```

| Flag | Description |
|------|-------------|
| `--repo <path>` | Path to git repository (default: current directory) |
| `--base <branch>` | Base branch for branch review comparison |

By default, `post-commit` reviews the single commit at HEAD. To review the
entire branch (all commits since diverging from the base branch) instead, set
`post_commit_review = "branch"` in `.roborev.toml`:

```toml
# .roborev.toml
post_commit_review = "branch"
```

When set to `"branch"`, each commit triggers a `merge-base..HEAD` range review.
On the base branch itself, detached HEAD, or any error, it falls back to a
single-commit review.

To reduce review noise from small commits, set a repository-local batch size:

```toml
# .roborev.toml
post_commit_batch_size = 5
```

The hook queues one review after five commits instead of one review per commit.
A partial batch is flushed before push. Values below `2` disable batching.

See: [Configuration](/configuration/#post-commit-review-mode) and
[Post-Commit Review Batching](/configuration/#post-commit-review-batching)

## Agent Hook

```bash
roborev agent-hook install              # Install profiles for detected agents
roborev agent-hook install --agent all  # Install all nine integrations
roborev agent-hook install --agent hermes --config ~/.hermes/config.yaml
roborev agent-hook install --binary ~/.local/bin/roborev
roborev agent-hook dump --agent qwen    # Native JSON config on stdout
roborev agent-hook dump --agent hermes  # Native YAML config on stdout
roborev agent-hook run --agent cursor   # Harness runtime; --agent is required
roborev agent-hook status               # Tracked session counters as JSON
roborev agent-hook reset <session-id>   # Reset one session (or --all)
```

| Flag | Description |
|------|-------------|
| `--agent <name>` | `claude`, `codex`, `copilot`, `cursor`, `droid`, `gemini`, `hermes`, `qwen`, `grok`, or `all` for install |
| `--dry-run` | Report whether each target needs changes without writing (`install`) |
| `--config <path>` | Override the native config path for one explicit profile |
| `--command <cmd>` | Override the full command for one explicit profile; it must select the same agent |
| `--binary <path>` | Resolve and bake this roborev binary path into installed agent hooks. Mutually exclusive with `--command` |

The default install detects agents by executable or existing config directory;
`--agent all` skips detection. Factory Droid remains user-scoped and rejects
project `.factory/hooks.json` paths. Hermes queues post-tool reminders for a
later `Stop`. Cursor records the same events as other profiles but emits no
control response.

If the old release provides `roborev agent-hook daemon`, run that release's
`roborev agent-hook daemon stop` before installing or starting the new release.
The new release uses only the regular roborev daemon and does not take over an
old auxiliary process.

After upgrading existing hooks, run `roborev agent-hook install` once. It
replaces recognizable Codex, Claude, and Factory Droid registrations from the
previous installer with profile-bearing commands while preserving unrelated
hooks. Replace `--codex-config` or `--claude-config` with
`--agent NAME --config PATH`; remove `--scope user`.

See [Agent Hook](/agent-hook/) for profile detection, threshold configuration,
the fallback fix workflow, and declarative config details.

## Checking Agents

```bash
roborev check-agents                # Smoke-test all installed agents
roborev check-agents --agent codex  # Test a specific agent
roborev check-agents --timeout 30   # Set timeout per agent (seconds)
```

| Flag | Description |
|------|-------------|
| `--agent <name>` | Test only this agent |
| `--timeout <secs>` | Timeout per agent (default: 60) |

Health checks honor configured `*_cmd` overrides, so they test the same binary
or wrapper that roborev uses for review and agentic jobs.

## Agent Skills

```bash
roborev skills install           # Install skills for agents
roborev skills update            # Update installed skills
```

See: [Agent Skills](/guides/agent-skills/)

## Sync & Streaming

```bash
roborev sync status              # Show PostgreSQL sync status
roborev sync now                 # Trigger immediate sync

roborev stream                   # Stream all events (JSONL)
roborev stream --repo .          # Filter to current repo
```

See: [PostgreSQL Sync](/advanced/postgres-sync/),
[Event Streaming](/advanced/streaming/)

## Multi-Repo Workspaces

`roborev list` looks in immediate child subfolders for repositories, so you can
run it from a parent directory that contains multiple repos. `roborev review`
suggests repo-level review commands when run from a workspace root, making it
easy to review across projects.

## Global Flags

These flags work across most commands:

| Flag | Description |
|------|-------------|
| `--server <addr>` | Daemon address (default: `http://127.0.0.1:7373`). Accepts `unix://` for Unix domain sockets |
| `-v, --verbose` | Verbose output |

## Update

```bash
roborev update                       # Update to latest version
roborev update --force               # Replace a development build
roborev update --running=wait        # Finish active reviews first
roborev update --running=interrupt   # Requeue active attempts, then update
roborev update --running=abort       # Update only when no reviews are active
roborev update --no-restart          # Install without daemon coordination
```

The updater coordinates daemon replacement with the review queue:

- `--running=wait` prevents new reviews from starting and waits for active
    reviews to finish.
- `--running=interrupt` cleanly stops active attempts and requeues them without
    consuming a retry.
- `--running=abort` updates only when the daemon atomically confirms that no
    reviews are running. A busy result exits nonzero.
- Without `--running`, interactive updates prompt when reviews are active.
    `--yes` defaults to `wait`.
- `--no-restart` skips daemon preparation, restart, hook repair, and skill
    updates.

When an interactive update finds active reviews, it asks once:

```text
3 reviews are currently running.

  [w] Wait for them to finish, then update
  [u] Update now; interrupt and restart them automatically
  [a] Abort

Choice [a]:
```

The daemon continues accepting enqueues during an update drain but does not
claim them until the replacement daemon is ready. A user cancellation remains
terminal. An update interruption starts a fresh attempt and discards the partial
attempt log. Non-interactive waits have no updater-specific deadline; they are
bounded by the configured job timeout (30 minutes by default).

Successful updates use a compact phase summary. The final success line appears
only after the replacement daemon is responsive and reports the installed
version:

```text
Downloading  100% (20.3 MB)
Installing   done
Daemon       restarted (v0.65.0)
Git hooks    done
Skills       done

Updated roborev to v0.65.0
```

If no daemon is running initially, the updater checks again before and after
installation so a daemon started concurrently is still drained, restarted, and
version-verified. The daemon phase says `not running` only when both checks stay
empty. Pressing Ctrl-C before installation releases the update drain. Pressing
it after installation exits nonzero and tells you to run
`roborev daemon restart` rather than claiming the update completed.
