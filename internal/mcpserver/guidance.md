# Roborev MCP guidance

These tools read review data, comment on and close existing reviews, snooze
Agent Hooks, and complete hook fix sessions. Review creation and cancellation
remain CLI operations. Use the MCP connection for the same daemon that issued
the review IDs or fix-session UUID.

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

## Updating existing reviews

Use `roborev_add_comment` with `job_id`, `commenter`, and `comment` to record
what changed or why a finding is invalid. After the comment succeeds and all
findings are resolved or invalid, call `roborev_close_review` with `job_id`.
Read back the review and comments to verify. For panels, use the synthesis
parent; `job.panel_role` identifies members. Ask for the parent ID if it is
not already known.

Use `roborev_snooze` with `repo_path`, `worktree_path`, `branch`, and `enabled`.
When enabling, supply a future RFC3339 `snoozed_until`. Disabling resumes
reminders. Reviews continue running.

When Agent Hook supplies a fix-session UUID, finish auditing the original
reviews, then call `roborev_complete_fix` with that exact `fix_session_id`, even
when deferred findings remain open. Never invent or discover a session UUID.
