# Review Export Design

## Purpose

Add a daemon-backed JSON export for completed roborev reviews:

```bash
roborev export reviews --format json [--since T] [--until T] [--profile metadata|content] \
  [--closed-only] [--repo R] [--project P] [--limit N]
```

The export is intended for downstream collection and analysis of completed review events. It is a historical event log, not a latest-per-PR view.

## Non-Goals

- Do not add structured finding extraction in this version. roborev currently stores review output as text, not structured findings.
- Do not export failed, canceled, queued, running, task, insights, fix, classify, or compact jobs.
- Do not sanitize raw review output. In the content profile, review text is exported as stored, subject only to a large size cap.
- Do not expose prompts, raw diffs, patches, logs, transcripts, command lines, environment data, or secrets-bearing fields through the export path.

## Command Contract

`roborev export reviews` emits one JSON document to stdout.

Flags:

- `--format json`: output format. JSON is the only supported format and the default. The flag is intentionally present as a forward-compatible affordance even though it has one valid value in this version.
- `--profile content|metadata`: defaults to `content`.
- `--since T`: optional lower bound for completed review time.
- `--until T`: optional upper bound for completed review time.
- `--closed-only`: only include top-level reviews marked closed.
- `--repo R`: exact repo identity or repo root path filter. Repo root paths may be used for filtering but are not emitted in the export document.
- `--project P`: exact project display-name filter.
- `--limit N`: maximum number of top-level reviews emitted by the CLI.

Time flags accept RFC3339 timestamps or `YYYY-MM-DD` dates. `--since` is inclusive and `--until` is exclusive for both timestamp and date-only inputs. Date-only values are interpreted as UTC day boundaries: `--since 2026-06-01` means `2026-06-01T00:00:00Z`; `--until 2026-06-01` means `2026-06-02T00:00:00Z`.

When `--limit` is omitted, the CLI uses bounded daemon pages and follows the cursor until all matching rows have been emitted into the single JSON document. When `--limit` is present, the CLI stops after emitting at most that many top-level reviews.

## API Surface

Add a Huma endpoint:

```text
GET /api/export/reviews
```

The CLI is a thin client for this endpoint. The endpoint returns one bounded page. The CLI is responsible for following `next_cursor` and assembling the final document when the user did not provide `--limit`.

Endpoint query parameters mirror the CLI plus `cursor`:

- `format`
- `profile`
- `since`
- `until`
- `closed_only`
- `repo`
- `project`
- `limit`
- `cursor`

The daemon enforces a default page size and maximum page size so a single Huma response cannot marshal an unbounded review history into memory. A reasonable initial default is 500 top-level reviews per page, with a maximum of 5000.

## JSON Schema

Top-level document:

```json
{
  "schema_version": 1,
  "tool": "roborev",
  "tool_version": "dev",
  "generated_at": "2026-06-29T00:00:00Z",
  "profile": "content",
  "window": {
    "field": "completed_at",
    "since": null,
    "until": null
  },
  "truncated": false,
  "next_cursor": null,
  "reviews": []
}
```

Top-level review object:

```json
{
  "review_id": "review-uuid",
  "status": "done",
  "verdict": "pass",
  "created_at": "2026-06-29T00:00:00Z",
  "completed_at": "2026-06-29T00:00:10Z",
  "duration_ms": 10000,
  "project": "repo-name",
  "repo": "github.com/org/repo",
  "branch": "main",
  "commit_sha": "abcdef",
  "pr_number": null,
  "pr_url": null,
  "agent": "codex",
  "model": null,
  "cost": {
    "tokens_in": null,
    "tokens_out": null,
    "usd": null
  },
  "content": "raw review output",
  "subagents": []
}
```

Subagent object:

```json
{
  "review_id": "member-review-uuid",
  "name": "security",
  "agent": "codex",
  "model": null,
  "review_type": "security",
  "verdict": "fail",
  "completed_at": "2026-06-29T00:00:05Z",
  "duration_ms": 5000,
  "content": "raw member review output"
}
```

Profile behavior:

- `content`: includes `content` for top-level reviews and nested subagents.
- `metadata`: sets all `content` fields to `null` and does not select `reviews.output` in the export query.

Unknown optional scalar fields are encoded as JSON `null`, not omitted. `subagents` is always present and is `[]` for non-panel reviews.

## Row Semantics

Top-level export rows are canonical reviews only:

- Include `review_jobs.job_type IN ('review', 'range', 'dirty', 'synthesis')`.
- Include `review_jobs.status = 'done'`.
- Include rows with a joined `reviews` row and non-null `reviews.verdict_bool`.
- Exclude panel members from the top-level set with `COALESCE(review_jobs.panel_role, '') != 'member'`.

Panel and CI review behavior:

- A panel run exports as one top-level synthesis review.
- Member reviews never appear as top-level rows.
- Member reviews are included under `subagents` for their synthesis review only when the member row has `review_jobs.status = 'done'`, a joined `reviews` row, and non-null `reviews.verdict_bool`. Failed, skipped, queued, running, or empty-output members are omitted from `subagents` in this version.
- Subagents are ordered by `(panel_member_index ASC, job_id ASC)`.
- Subagent `name` comes from `review_jobs.panel_member_name`; if it is empty, use `review_jobs.agent`.
- Subagent `completed_at` comes from normalized member `reviews.created_at`, matching top-level review semantics.
- Subagent `duration_ms` is execution time for the member job: member `review_jobs.finished_at - review_jobs.started_at`.
- `--limit` counts only top-level rows; nested subagents do not count against it.
- Historical PR exports can contain multiple rows with the same `pr_number` when a PR was reviewed at multiple head SHAs. `commit_sha` distinguishes those events.

Compact jobs are excluded from this version. They produce review-like verification output, but they are not literal commit, range, dirty, or CI synthesis reviews.

## Time Semantics

`created_at` is based on `review_jobs.enqueued_at`: the time the job entered the queue. It is normalized and emitted as RFC3339 UTC.

`completed_at` is based on `reviews.created_at`, not `review_jobs.finished_at`.

Reasons:

- `reviews.created_at` belongs to the row being exported.
- `review_jobs.finished_at` has mixed local-offset formatting and is not a safe cursor key.
- Synced `reviews.created_at` values may still be RFC3339 while local values may be SQLite `datetime('now')` text, so all comparisons must normalize timestamp text before filtering or ordering.

Use a shared timestamp normalization expression for SQL comparisons and output conversion for stored timestamp text, including `review_jobs.enqueued_at` and `reviews.created_at`:

```sql
datetime(
  CASE
    WHEN <timestamp_column> GLOB '*[+-][0-9][0-9]:[0-9][0-9]' OR <timestamp_column> LIKE '%Z'
    THEN <timestamp_column>
    ELSE <timestamp_column> || 'Z'
  END
)
```

Extract this into a shared helper so export, sync, and future SQL paths use one canonical normalization rule for roborev timestamp text.

Output timestamps are always RFC3339 UTC.

`created_at`, `completed_at`, and `duration_ms` intentionally use different sources. `created_at` is enqueue time from the job row. `completed_at` is the stable export event time from the review row. `duration_ms` is execution time from the job row: `review_jobs.finished_at - review_jobs.started_at`, parsed with the existing timestamp parser. If either side is missing or unparsable, emit `null`.

## Cursor And Ordering

Reviews are ordered by:

```text
(normalized reviews.created_at ASC, reviews.uuid ASC)
```

The cursor is opaque and encodes the last emitted `(completed_at, review_id)` pair. The next page filters rows strictly after that pair:

```sql
normalized_completed_at > normalized_cursor_completed_at
OR (
  normalized_completed_at = normalized_cursor_completed_at
  AND reviews.uuid > cursor_review_id
)
```

`truncated` is true when a page or explicit CLI `--limit` stops before all matching rows are emitted. `next_cursor` is set when additional rows are available.

## Repo, Project, And PR Fields

Review field sources:

- `status`: `review_jobs.status`, filtered to `done`.
- `agent`: `review_jobs.agent`.
- `model`: `review_jobs.model`, with empty string emitted as `null`.
- `branch`: `review_jobs.branch` when non-empty, otherwise `review_jobs.ci_base_branch` when non-empty, otherwise `null`. This matches the existing `ReviewJob.HookBranch()` semantics so CI synthesis rows report the PR base branch.

`project` is `repos.name`.

`repo` is `repos.identity` when present, otherwise `repos.name`. Do not emit `repos.root_path` as a fallback because it can reveal local usernames and filesystem layout. The `project` field also uses `repos.name`, so local-only repos without an identity may have matching `repo` and `project` values.

`--repo` matches either `repos.identity` or `repos.root_path` exactly. This keeps local path filtering available to the CLI without exposing the path in the output document. `--project` matches `repos.name` exactly.

`commit_sha` comes from `commits.sha` for single-commit reviews. For range, dirty, and synthesis rows, use the best stable reviewed ref available:

- commit review: joined `commits.sha`.
- range and synthesis reviews: the end ref when `git_ref` is a two-dot or three-dot range and the end ref is SHA-like; otherwise `null`.
- dirty review: `null`.

For CI panel synthesis rows, join `ci_pr_panels` on `review_jobs.panel_run_uuid = ci_pr_panels.panel_run_uuid`:

- `pr_number` is `ci_pr_panels.pr_number`.
- `pr_url` is `https://github.com/{github_repo}/pull/{pr_number}`.
- `head_sha` from `ci_pr_panels` can be used as an authoritative cross-check for the synthesis range end. If both are present and disagree, prefer `ci_pr_panels.head_sha` and surface the mismatch through logging or telemetry rather than exporting the range text as `commit_sha`.

For local panels and non-panel reviews, PR fields are `null`.

## Cost Fields

Cost fields are extracted from `review_jobs.token_usage` JSON. The export must not emit the raw token usage blob.

- `cost.tokens_in`: `$.input_tokens` when present and non-zero; otherwise `null`. Many existing rows do not carry input-token data, so this field may commonly be `null`.
- `cost.tokens_out`: `$.total_output_tokens` when present and non-zero; otherwise `null`.
- `cost.usd`: `$.cost_usd` only when `$.has_cost` is true; otherwise `null`.

The extraction should use structured JSON access (`json_valid`, `json_extract`, or Go JSON decoding), not string parsing.

## Verdict Backfill

Add an automatic idempotent migration in the existing DB-open migration chain that populates `reviews.verdict_bool` for legacy rows:

```text
WHERE verdict_bool IS NULL AND output != ''
```

The migration is a Go-side row scan because `ParseVerdict` is Go code, not SQL. It should batch updates in a transaction. It uses the existing deterministic verdict parser and stores the result. This permanently bakes the parser's current interpretation into legacy rows. That tradeoff is intentional: metadata export can then avoid loading raw review output while still including legacy reviews. The migration is idempotent and should do no work after all legacy rows have been populated.

The existing hidden backfill command can remain, but export should not require users to run a manual command first.

## Content Caps

Raw review content is usually small, but the export should still be bounded:

- Cap each `content` string at 1 MiB.
- Truncate by bytes at a valid UTF-8 boundary.
- Append `...[truncated]` when truncating.
- Apply the same cap to top-level review content and subagent content.

Other string fields should be bounded conservatively in the export mapping, for example 4096 bytes for identifiers and paths. The cap should preserve valid UTF-8 and should not mutate stored data.

## Privacy Boundary

The export path enforces privacy by column projection. It must not select or load:

- `reviews.prompt`
- `review_jobs.prompt`
- `review_jobs.diff_content`
- `review_jobs.dirty_files`
- `review_jobs.patch`
- `review_jobs.command_line`
- job logs or output streams
- agent transcripts or traces
- environment variables or credentials

In the `content` profile, the export does select `reviews.output`. This is raw review content as stored. It may contain sensitive repository details and should be handled carefully by downstream systems. It is not sanitized.

Repo identity fallback is also privacy-sensitive. The export must not emit absolute `repos.root_path` values when `repos.identity` is absent; use the repo display name instead.

Tests should assert forbidden columns and sentinel values are absent from both profiles, while allowing intentionally exported review output in the content profile.

## Tests

Storage tests:

- Metadata golden export excludes `content` text and does not load `reviews.output`.
- Content golden export includes raw `reviews.output` exactly as stored, except for JSON escaping and configured truncation.
- Legacy verdict rows are included after automatic backfill.
- Empty-output rows with null `verdict_bool` are excluded.
- Job type filtering excludes task, insights, fix, classify, and compact rows even when they have review rows and verdicts.
- Top-level panel export includes synthesis only and nests member reviews under `subagents`.
- Subagent export includes only completed member reviews with verdicts and omits failed, skipped, queued, running, and empty-output members.
- CI synthesis export includes `pr_number` and `pr_url`; local panel rows emit `null` PR fields.
- Synthesis `commit_sha` uses the range end or CI panel head SHA, never the raw synthesis range.
- `created_at` is sourced from normalized `review_jobs.enqueued_at`.
- CI synthesis `branch` falls back to `review_jobs.ci_base_branch`.
- Field-source mapping covers `status`, `agent`, `model`, and `branch`.
- `--since` and `--until` filter on normalized `reviews.created_at`.
- RFC3339 and date-only windows use inclusive `since` and exclusive `until` semantics.
- Cursor pagination is deterministic for rows sharing the same completed timestamp.
- `--closed-only` filters on the canonical top-level review.
- Cost fields are extracted as scalars from `review_jobs.token_usage` and the raw token usage JSON is not emitted.
- Local repos without identity do not emit absolute root paths in `repo`.
- Content caps preserve valid UTF-8 and append the truncation marker.
- Forbidden private columns and sentinel values are absent from metadata and content profiles.

Daemon/API tests:

- Endpoint validates unsupported format/profile values as usage errors.
- Endpoint enforces default and maximum page sizes.
- Endpoint returns `truncated` and `next_cursor` when more rows exist.

CLI tests:

- `roborev export reviews` defaults to JSON content profile.
- `--profile metadata` emits the same row metadata with `content: null`.
- CLI follows daemon cursors when `--limit` is omitted and returns a single JSON document.
- Explicit `--limit` stops after the requested number of top-level reviews.
- Runtime failures and usage errors follow existing CLI conventions.
