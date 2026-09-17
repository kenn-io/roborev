---
last_edited: 2026-09-17
---

# Roborev MCP guidance

These tools read review data from the local roborev daemon. They never enqueue,
cancel, close, or comment on reviews; use the `roborev` CLI for those actions.

## Suggested flow

1. Call `roborev_status` to confirm the daemon is reachable and see queue
   counts.
2. Call `roborev_list_repos` to discover tracked repository root paths. Pass
   the exact `root_path` value as `repo_path` in later calls.
3. Call `roborev_search_reviews` first when looking for relevant historical
   reviews. Use `auto` for discovery, `lexical` for exact paths, identifiers,
   or quoted errors, and `semantic` only when wording-independent retrieval is
   specifically needed.
4. Fetch the exact review with `roborev_get_review` and the returned `job_id`.
5. Fetch its responses with `roborev_list_comments` only when needed.
6. Use `roborev_list_jobs` for queue browsing and `roborev_get_job_output` for
   streamed output from a running or failed job.

## Interpreting results

- Use `web_url` when linking to a review. It identifies the owning daemon and
  includes its configured browser base path. It is omitted when the browser
  listener is unavailable; do not invent a URL from a job ID.

- `verdict` is `pass`, `fail`, or empty when the review has no verdict yet.
- `closed` is true when a developer has marked the review as addressed.
- `partial` means the search covered only part of the available review or
  embedding history.
- `bounded` means semantic retrieval reached its candidate ceiling.
- `degraded` means `auto` mode fell back to lexical retrieval.
- Job `status` is one of `queued`, `running`, `done`, `failed`, `canceled`,
  `applied`, `rebased`, or `skipped`.
- Listings are capped; use `next_cursor` from `roborev_list_jobs` to page.
