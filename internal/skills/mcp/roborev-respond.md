## Instructions

Use the roborev MCP server. Discover tools by their names below, allowing for
the agent's server prefix. Report connection failures instead of switching
transports silently.

1. Require a job ID. Call `roborev_get_review` with `job_id`. For a panel member,
   resolve its synthesis parent from known context or ask for the parent ID.
   Comment on and close the parent only.
2. If the invocation has no message, ask the user what to record.
3. Call `roborev_add_comment` with `job_id`, `commenter: "roborev-respond"`,
   and the user's message as `comment`.
4. After the comment succeeds, call `roborev_close_review` with `job_id`.
5. Verify the comment using `roborev_list_comments` and verify `closed: true`
   using `roborev_get_review`. Report any failed operation.
