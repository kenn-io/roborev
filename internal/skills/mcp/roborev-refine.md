## Instructions (MCP mode)

1. Resolve roborev MCP tools using tool discovery. If unavailable, report the
   connection error and stop. Use `roborev_list_repos` to resolve `repo_path`.
2. Validate the requested branch against the current branch. Validate `--since`
   as a commit and ancestor of HEAD. Require `--since` on the default branch.
   Honor `--max-iterations` (default 10). Pass refs as separate arguments or
   quoted shell variables, never interpolate them into shell source.
3. Start the authorized review with `roborev review --from-skill --since <commit>` when
   supplied, otherwise `roborev review --from-skill --branch`. Do not use `--wait` or shell
   out to the refine CLI. Record the enqueue job ID; for panels this is the
   synthesis parent.
4. Poll `roborev_list_jobs` with `repo_path` and branch, following `next_cursor`
   to find the queued job. On completion, call `roborev_get_review` and
   `roborev_list_comments` with `job_id`. Report errored/canceled jobs and empty
   verdicts as errors. Only a `pass` verdict on this full-scope review ends the
   loop successfully.
5. For a `fail` verdict, verify each finding against the code before editing.
   Fix valid in-scope findings, respect developer comments, and record evidence
   for rejected findings. Leave valid deferred findings open and ask for
   direction. Run the project's tests and commit according to its instructions.
6. Call `roborev_add_comment` with the original job ID, `commenter` set to
   `roborev-refine`, and `comment` describing fixes and rejected findings.
   Only after that succeeds, call `roborev_close_review` with the job ID if
   every finding is resolved. Audit with `roborev_get_review`.
7. Track a severity stall streak across full-scope re-reviews. The initial
   review is the baseline. Count one round only after a new fix commit and its
   completed `fail` review; a retry without a new commit does not count. Use
   `finding_counts.high + finding_counts.medium` from `roborev_get_review`.
   Critical and Low do not count. If the total is positive and no smaller than
   the preceding positive total, increment the streak; otherwise reset it to
   zero. Always replace the preceding total. A missing `finding_counts`, a
   passing or closed review, a no-change fix, or a failed re-review resets the
   streak. MCP mode cannot read finding locations, so it does not track the
   repeated-file signal.
8. When the streak reaches exactly three, print one advisory that names the
   signal and ends with: "Change the plan, or split the change into smaller
   reviewable pieces." These signals are heuristics, not proof that refine
   cannot converge. Save it with `roborev_add_comment` on the newly failed job
   with `commenter` set to `roborev-refine`, and warn if that fails. Advise even
   on the final iteration. Continue by default. With `--stop-on-stall`, stop
   before another fix and leave the review open.
9. Repeat from step 3 using the same branch/range scope. Do not substitute a
   commit-only hook review for the full-scope re-review. Stop at the iteration
   limit and report remaining findings. Do not fetch or close unrelated jobs.

Use MCP for all polling, review retrieval, comments, and closure. CLI use in
this workflow is limited to Git, project checks, and authorized review creation.
