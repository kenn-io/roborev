## Review transport (MCP mode)

Use the workflow above with these changes for trial reviews:

1. Resolve the roborev MCP tools through the agent's tool discovery. If a tool
   is unavailable, report the connection error and stop; do not substitute CLI
   reads.
2. Configuration checks, hook installation, and review creation still use the
   CLI commands above. Start each approved trial review with
   `roborev review --from-skill --type <name> --panel none` or
   `roborev review --from-skill --panel <name>`, without `--wait`. When
   providers are restricted and budget routing is on, use only the panel
   form, as the workflow above requires. Record the
   job ID from the enqueue output. Enqueue registers the repository, so then
   resolve it with `roborev_list_repos` and retain its `root_path` for
   `repo_path` arguments. Poll `roborev_list_jobs` with `repo_path` and the
   current branch, following `next_cursor` until that job is found. Report
   failed or canceled jobs and stop.
3. Once the job is done, read it with `roborev_get_review`, passing `job_id`.
   For a panel, the enqueue ID is the combined review; read a member's review
   with the same tool and that member's job ID. An empty verdict is not a pass.
4. Do not comment on or close trial reviews unless the user asks.
