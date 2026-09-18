## Instructions

Use the roborev MCP server's `roborev_snooze` tool. Discover it by name,
allowing for the agent's server prefix. Report connection failures.

1. Resolve the current worktree root and branch using Git. Use
   `roborev_list_repos` to identify the tracked repository's `root_path`.
   If it is not tracked, report that without registering or initializing it.
2. Call `roborev_snooze` with `repo_path`, `worktree_path`, and `branch`.
   With no action or `on`, set `enabled: true` and `snoozed_until` to the
   current time plus the requested duration, expressed as an RFC3339 timestamp.
   The default duration is eight hours. For `off`, set `enabled: false` and
   omit `snoozed_until`.
3. Report the returned snooze state and expiry. Snoozing affects Agent Hook
   reminders only; reviews continue running.
