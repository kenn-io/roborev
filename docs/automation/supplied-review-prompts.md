# Supplying a review prompt

Automation can send `review_prompt` to `POST /api/enqueue` when it has already
assembled the review instructions and context. Unlike `custom_prompt`, which
creates an ad-hoc task, this keeps the commit or range as the review target:

```json
{
  "repo_path": "/path/to/repository",
  "git_ref": "base-sha..head-sha",
  "agent": "codex",
  "review_prompt": "Review this range for data loss. Inspect the diff with git."
}
```

Replace the path and refs with a local repository and commits it contains. A
single commit or resolvable ref also works. The daemon resolves refs when the
request is accepted and returns a normal review job. Go callers use
`generated.EnqueueRequest.ReviewPrompt` through `pkg/client`; results and
cancellation use the existing job endpoints.

The agent runs in a fresh detached checkout of the resolved head, even when
`isolate_reviews` is disabled. The supplied prompt replaces RoboRev's normal
prompt construction, including its repository guidelines and discussion
collection. Include the instructions, context, and diff-reading directions the
agent needs. RoboRev still adds its structured-output instruction and applies
normal prompt preparation and binary-evidence settings.

This mode creates one review. It does not select a configured default panel, run
review experiments, or enqueue an automatic design review. Do not combine it
with `custom_prompt`, dirty input, `agentic`, a named panel, `source`,
`job_type`, or goal/analysis inputs. An empty `panel` or `"none"` is accepted.
The stored job has source `review_prompt`.

Automatic retries and manual reruns keep the supplied prompt and resolved
revision, including reruns of jobs imported through sync. Post-rewrite commit
remapping leaves these jobs at their original revision, even when a rebase
preserves the patch. Prompt pruning retains the prompt, as it does other inputs
that cannot be rebuilt; deleting the job removes it. If the stored prompt is
missing, the job fails without invoking an agent, retrying, or switching to a
backup agent. Submit a new request with the intended prompt to recover.

## Execution settings

Without `review_settings`, these requests use normal execution configuration.
Repository configuration comes from the caller's worktree while it exists, or
the main checkout otherwise. It can control exclusions, binary evidence, and
tool mode. Omitting `min_severity` uses the configured `review_min_severity`.

To supply policy independently of the checkout, include `review_settings`:

```json
{
  "repo_path": "/path/to/repository",
  "git_ref": "base-sha..head-sha",
  "agent": "codex",
  "model": "your-model",
  "reasoning": "high",
  "min_severity": "medium",
  "review_prompt": "Review this range for data loss. Inspect the diff with git.",
  "review_settings": {
    "tools": { "mode": "read-only", "mcp": {} },
    "binary_evidence": true,
    "exclude_patterns": [],
    "backup_agent": "claude-code",
    "backup_model": "your-backup-model"
  }
}
```

Replace the model names with models available to your agents. Go callers use
`generated.EnqueueRequest.ReviewSettings`. The object requires `review_prompt`
and has these semantics:

| Field | Behavior when omitted or empty |
| --- | --- |
| `tools` | Uses the selected adapter's ordinary non-agentic review tools. To require restricted tools, supply `{"mode":"read-only","mcp":{}}`. Both agents must support the policy. |
| `binary_evidence` | Does not add committed binary files to the prompt. |
| `exclude_patterns` | Does not exclude any files from daemon-prepared diff or binary evidence. The daemon does not filter text the caller already included in the prompt. |
| `backup_agent` | Does not use a backup, even if one is configured locally. |
| `backup_model` | Lets the supplied backup use its default model. Requires `backup_agent` when nonempty. |

An empty `review_settings: {}` is meaningful: it opts out of repository policy
with all the defaults above. It does not inherit local exclusions or review-type
tool settings. `min_severity` comes only from the request; omitting it retains
all findings. Agent, model, and reasoning defaults resolve from the daemon's
configuration at enqueue time. The resolved choices and supplied settings remain
with the job through retries, sync, and reruns. Rerunning after backup failover
restores the original choice. To change the agent or policy, enqueue a new job.
Automatic budget routing does not substitute another agent for these jobs.

Repository configuration cannot suppress these requests through branch or commit
exclusions. A malformed `.roborev.toml` does not block enqueue or rerun.
Daemon-created snapshots use `.roborev` in the detached checkout. Include prior
reviews and discussion in `review_prompt`; the dynamic prior-review placeholder
is rejected because it would add mutable inputs on each attempt.

The daemon still owns executable configuration, credentials, MCP allowlists,
agent permissions, timeouts, cooldowns, automatic closure of passing reviews,
and within-job retries. The host's prompt size threshold chooses inline text or
a complete prompt file without dropping content. Supplied settings do not
sandbox an agent or prevent it from reading repository files or its own native
configuration. Use an execution environment appropriate for the repositories you
review.

## Reading invocation history

Reviews submitted with `review_settings` record each call to the review agent
locally. Read that history with `GET /api/job/invocations?job_id=123`, replacing
`123` with the job ID returned by enqueue. Generated Go clients expose
`GetJobInvocations` through `pkg/client`.

The response contains an `invocations` array in execution order. Each record has
its own local ID, the job's enqueue time, selected agent/model/reasoning,
reviewed revision, start and finish times, session ID, adapter output, validated
review JSON when available, and any error. Retries, backup-agent runs, and
manual reruns add records; they do not replace earlier outcomes. A failed
invocation can have partial output and usage even when a later invocation
completes the review. `raw_output` contains the adapter's returned text, or its
captured output stream when it returns no text. This preserves partial responses
that a native adapter rejects before returning a review document. Invalid UTF-8
bytes in diagnostics are replaced with the Unicode replacement character
(`U+FFFD`) so they cannot prevent saving the report or retrying the review.

`status` is `succeeded`, `failed`, `canceled`, or `unfinished`. An unfinished
record means the daemon saved the start but has no recorded outcome. It does not
mean an agent is still running: the daemon may have stopped during execution or
failed to save the result. A start record is written before calling the agent.
If that write fails, no agent runs. If saving the outcome fails, the job fails
without an automatic retry, and the record stays unfinished.

`usage` contains the token and cost evidence available in that invocation's
adapter output stream when it ends, captured independently of the best-effort
job log. Rendered review text is excluded, so quoted provider events cannot
replace measured usage. A log-file failure cannot substitute an earlier
attempt's usage. Codex, Claude and Pi expose provider events that can supply
these measurements. Adapters that expose only assistant text, including Copilot,
Muse and ACP, leave invocation usage unavailable even if their text quotes usage
events. Usage is `null` when no measurements are available. Within a usage
object, `has_cost: false` means cost is unknown, not free. These snapshots do
not perform later AgentsView lookups or replace the job's existing cost
reconciliation. A `usage_error` reports a failure reading that evidence.

History covers calls made with this feature enabled, not earlier runs or
individual provider requests inside an agent. Ordinary reviews and the CI poller
do not create these records. Invocation history is local to the daemon that
executed it and is not copied by PostgreSQL sync; an empty list does not prove
that a synced job never ran. Collect reports from the executing daemon before
discarding its data. Reruns and prompt cleanup keep this history; deleting the
repository and its jobs removes it. The endpoint uses the local daemon API and
is not exposed by the remote browser interface.
