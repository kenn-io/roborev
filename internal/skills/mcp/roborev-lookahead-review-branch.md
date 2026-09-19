## Instructions (MCP mode)

1. Resolve the roborev MCP tools through the agent's tool discovery. If a tool
   is unavailable, report the connection error and stop; do not substitute CLI
   reads. Resolve the current repository with `roborev_list_repos` and retain
   its `root_path` for `repo_path` arguments.
2. Validate any supplied base branch with Git and pass it as the
   `--base` argument. Review the current branch against its merge-base.
   Pass user-supplied refs as separate arguments or quoted shell variables,
   never interpolate them into shell source. Honor the requested panel and
   review type; omit optional flags when absent.
3. Start the authorized review with `roborev review --branch --type lookahead` and the
   selected arguments. Do not use `--wait`: record the job ID from the enqueue
   output, then poll `roborev_list_jobs` with `repo_path` and the current branch,
   following `next_cursor` until that job is found. Read running output with
   `roborev_get_job_output` if useful. Report errors or canceled jobs and stop.
4. Once the job is done, call `roborev_get_review` with `job_id` and
   `roborev_list_comments` with that ID. For a panel, the enqueue ID is the
   synthesis parent; use its verdict, not a member's verdict.
5. Present the verdict (`pass`, `fail`, or empty) and findings to the user.
   An empty verdict is not a pass. Do not fix code, comment, close reviews,
   or create another review unless separately authorized.
