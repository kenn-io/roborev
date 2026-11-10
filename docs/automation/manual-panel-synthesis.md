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

This option requires an explicitly named foreground panel. It cannot be combined
with `source`, `custom_prompt`, `review_prompt`, or a goal review. It does not
supply panel policy: the panel's normal configuration still applies. Without
`hold_synthesis`, panels synthesize automatically as before, including CI and
post-commit panels.
