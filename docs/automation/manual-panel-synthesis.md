# Start synthesis when you are ready

An automation client can run a panel's reviewers, inspect their results, then
request the combined review. Set `hold_synthesis` when queueing a named panel
through the local daemon API:

```json
{
  "repo_path": "/path/to/repository",
  "git_ref": "base-sha..head-sha",
  "panel": "thorough",
  "hold_synthesis": true
}
```

Send this body to `POST /api/enqueue`. Replace the path and refs with a local
repository and its commits, and `thorough` with a
[configured panel](../advanced/subagent-review-panels.md). The response gives
you the `panel_run_uuid`, `member_job_ids`, and synthesis job `id`.

The members run normally, including their configured retries and backup agents.
Synthesis remains queued with `claim_blocked: true`, even after the members
finish or the daemon restarts. Inspect the jobs with
`GET /api/jobs?panel_run=<panel_run_uuid>` and each member's result with
`GET /api/review?job_id=<member_job_id>`.

Once all voting members have finished, send this body to
`POST /api/panel/synthesize`:

```json
{
  "panel_run_uuid": "b92a669a-0a17-47a1-b868-89928b1851f0"
}
```

Use the UUID returned by enqueue. The response is the existing synthesis job; it
may still be queued while waiting for a worker. Repeating this request returns
the same job without running synthesis again. If an acknowledgement is lost,
retry this request with the same UUID.

A request made before the voting members finish returns `409 Conflict`. So does
a request for a canceled panel or one that was not held. An unknown panel
returns `404 Not Found`. Advisory, non-voting members do not delay synthesis.
Existing synthesis rules still apply: a single usable review or a clean panel
can finish without a synthesis model call.

To abandon the panel, cancel its synthesis job through `POST /api/job/cancel`.
That also cancels unfinished members. Starting synthesis cannot revive it. A
whole-panel rerun through `POST /api/job/rerun` creates a new held panel with a
new UUID and requires a new synthesis request.

Go clients use `generated.EnqueueRequest.HoldSynthesis` and
`StartPanelSynthesis` through `pkg/client`. These are local daemon operations
with the same authentication as other native job operations; they are not
exposed through the remote browser API.

On `POST /api/enqueue`, this option requires an explicitly named foreground
panel. It cannot be combined with `source`, `custom_prompt`, `review_prompt`, an
insights job, or a goal review. It does not supply panel policy: the panel's
normal configuration still applies. Without `hold_synthesis`, panels synthesize
automatically as before, including CI and post-commit panels.

## Supply a panel through the API

To choose every reviewer's prompt and policy without editing `.roborev.toml`,
send a complete panel to `POST /api/panel/enqueue`:

```json
{
  "repo_path": "/path/to/repository",
  "git_ref": "base-sha..head-sha",
  "hold_synthesis": true,
  "members": [
    {
      "name": "correctness",
      "agent": "codex",
      "review_prompt": "Review this range for correctness. Inspect the diff with git.",
      "review_settings": {}
    },
    {
      "name": "data-safety",
      "agent": "claude-code",
      "review_prompt": "Review this range for data loss. Inspect the diff with git.",
      "review_settings": {}
    }
  ],
  "synthesis": {
    "agent": "codex",
    "instructions": "Verify each finding against the code before retaining it."
  }
}
```

Both agents must be installed and configured on the daemon's host. The request
returns `201 Created` with the same panel UUID, member job IDs, and synthesis
job as a configured panel. Use the result, cancellation, and synthesis endpoints
above. Go callers use `EnqueueSuppliedPanel` through `pkg/client`.

Every member needs a unique `name`, an `agent`, a nonempty `review_prompt`, and
`review_settings`. An empty settings object opts out of repository policy; it
does not inherit the checkout's exclusions or tool configuration. The
[supplied review settings](supplied-review-prompts.md#execution-settings)
describe the fields and prompt placeholders. Include the review instructions and
discussion context the agent needs. All supplied members vote in synthesis.

Each member can also set `model`, `provider`, `reasoning`, `review_type`, and
`min_severity`. Synthesis accepts `model`, `provider`, `reasoning`,
`instructions`, `tools`, `backup_agent`, and `backup_model`. Omitted execution
choices resolve from the daemon's review configuration at enqueue time: members
use their `review_type`, and synthesis uses the default review type's model and
reasoning, not `synthesis_model` or `synthesis_reasoning`. Send explicit values
to choose them independently of those defaults. Empty backup choices stay empty;
local backup settings cannot fill them. A supplied synthesis tool policy must be
supported by both its primary and backup agents, just as for members.

RoboRev resolves the commit or range once for the whole panel. Members and
synthesis run in fresh checkouts of that head. Branch labels are inferred from
that head; ambiguous branches remain unlabeled. The panel uses the supplied
policy even if `.roborev.toml` changes or becomes invalid. It does not select
local panel defaults, experiments, automatic design reviews, reusable sessions,
or repository hooks. Global daemon hooks still run. Invalid member or synthesis
settings reject the request before any jobs are queued. Each successful enqueue
creates a new panel; enqueue requests do not have replay receipts.

Retries preserve each member's prompt and settings. Whole-panel reruns create
new jobs with the original choices, including the primary agent after backup
failover. A held panel's rerun is held again. Without `hold_synthesis`,
synthesis starts automatically once the members finish. The usual no-agent
outcomes remain: no usable review fails the synthesis job, and a single usable
review without synthesis instructions passes through. A clean panel with no
retained findings completes without a synthesis call.

Member and synthesis calls appear in
[invocation history](supplied-review-prompts.md#reading-invocation-history). Use
each member's job ID for its calls and the panel's returned `id` for synthesis
calls. Retries and backup agents keep separate records with their outputs,
errors, and available usage. Whole-panel reruns create new jobs; the original
jobs keep their history. Passthrough, clean-panel completion, and failures
before an agent runs create no synthesis invocation. The final combined review
still uses the review endpoint.

Restricted Claude synthesis also records emitted tokens and cost in the job and
spend summaries, including terminal failures, without looking up an unsaved
session. Supplied policy does not sandbox native agent configuration. The daemon
still owns credentials, runtime controls, retries, and cooldowns.
