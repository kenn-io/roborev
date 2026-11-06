---
last_edited: 2026-11-05
---

# Roborev MCP guidance

These tools read review data, comment on and close existing reviews, snooze
Agent Hooks, and complete hook fix sessions. Review creation and cancellation
remain CLI operations. Use the MCP connection for the same daemon that issued
the review IDs or fix-session UUID.

## Bundled guides

Call `roborev.skills {}` to list the bundled guide names, titles, and summaries.
Read one with `roborev.skills {"name":"roborev-fix"}`. Each guide reuses the
shipped skill with its MCP instructions. Reading it does not invoke a skill,
authorize its commands, or widen the review IDs supplied by an Agent Hook.
Named guides explain the explicit invocation spellings used by different
clients. Some workflows still require local CLI, Git, or code-editing access.

## CLI commands and MCP tools

| CLI operation | MCP tool | Difference |
| --- | --- | --- |
| `roborev status` | `roborev_status` | Structured daemon and queue status |
| `roborev repo list` | `roborev_list_repos` | Use the returned `root_path` as `repo_path` |
| `roborev job list` | `roborev_list_jobs` | Follow `next_cursor`; no prompts or diffs |
| `roborev job show` | `roborev_get_review` | Pass `job_id` or `sha`; verdict is `pass`, `fail`, or empty |
| `roborev search` | `roborev_search_reviews` | Pass filters as fields; read coverage flags |
| `roborev job comment` | `roborev_add_comment` | Pass `job_id`, `commenter`, and `comment` |
| Comments in `roborev job show` | `roborev_list_comments` | Fetch responses separately when needed |
| `roborev job close` | `roborev_close_review` | Close one existing review by `job_id` |
| `roborev job log` | `roborev_get_job_output` | Bounded output snapshot, not a live stream |
| `roborev agent hook snooze` | `roborev_snooze` | Supply repo, worktree, branch and absolute expiry |
| `roborev agent hook fix-done` | `roborev_complete_fix` | Use only the exact supplied fix-session UUID |
| Review creation/cancellation and configuration | No MCP tool | Requires authorized CLI access |
| Git operations and code edits | No MCP tool | Requires local repository access |

CLI flags such as `--json` and `--from-skill` are not MCP arguments.
Use only fields in each tool's schema. MCP clients without local capabilities
must report those workflow limits instead of inventing replacement tools.

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
