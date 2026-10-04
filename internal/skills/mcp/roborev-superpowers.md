## Review transport (MCP mode)

Use the workflow above with these changes for review access:

1. Resolve the roborev MCP tools through the agent's tool discovery. If a tool
   is unavailable, report the connection error and stop; do not substitute CLI
   reads. Resolve the repository with `roborev_list_repos` and retain its
   `root_path` for `repo_path` arguments.
2. Start each authorized goal review with
   `roborev review --from-skill --type goal --agent <reviewer>` and the selected
   spec and plan arguments. Omit `--wait` from the examples above. Record the
   job ID from the enqueue output, then poll `roborev_list_jobs` with
   `repo_path` and the current branch, following `next_cursor` until that job
   is found. Read running output with `roborev_get_job_output` if useful.
   Report failed or canceled jobs and stop.
3. Once the job is done, read its result with `roborev_get_review` and its
   comments with `roborev_list_comments`, passing `job_id`. An empty verdict
   is not a pass. Use these tools for existing code reviews too.
4. Record review dispositions with `roborev_add_comment`. Close a review with
   `roborev_close_review` only after every finding is addressed within the
   authorized scope. Goal findings require artifact or task edits; do not
   send them to a code-fix agent.
