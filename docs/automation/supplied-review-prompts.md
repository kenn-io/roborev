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

These requests still use normal execution configuration, agent permissions,
retries, and backup agents. Repository configuration comes from the caller's
worktree while it exists, or the main checkout otherwise. It can control file
exclusions, binary evidence, and tool mode. Supplying a prompt does not freeze
this configuration source or its settings, or isolate the agent from repository
files. Use this mode only with execution settings you trust.
