# Roborev MCP guidance

These tools read review data from the local roborev daemon. They never enqueue,
cancel, close, or comment on reviews; use the `roborev` CLI for those actions.

## Suggested flow

1. Call `roborev_status` to confirm the daemon is reachable and see queue
   counts.
2. Call `roborev_list_repos` to discover tracked repository root paths. Pass
   the exact `root_path` value as `repo_path` in later calls.
3. Call `roborev_list_jobs` with `repo_path` and optionally `branch`,
   `status`, or `closed` to find review jobs. Rows are metadata only; the
   review text is not included.
4. Call `roborev_get_review` with a `job_id` (or a commit `sha`) to read the
   review output, verdict, and finding counts.
5. Call `roborev_list_comments` to read developer responses attached to a
   review, and `roborev_get_job_output` to read the agent's streamed output
   for a running or failed job.

## Interpreting results

- Use `web_url` when linking to a review. It identifies the owning daemon and
  includes its configured browser base path. It is omitted when the browser
  listener is unavailable; do not invent a URL from a job ID.

- `verdict` is `pass`, `fail`, or empty when the review has no verdict yet.
- `closed` is true when a developer has marked the review as addressed.
- Job `status` is one of `queued`, `running`, `done`, `failed`, `canceled`,
  `applied`, `rebased`, or `skipped`.
- Listings are capped; use `next_cursor` from `roborev_list_jobs` to page.
