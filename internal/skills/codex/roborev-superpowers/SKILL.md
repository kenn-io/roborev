---
name: roborev-superpowers
description: Use only when the user explicitly invokes $roborev-superpowers
---

# roborev-superpowers

Carry a feature from intent to verified implementation with Superpowers, Kata,
and Roborev. Keep the user's outcome and constraints visible throughout the work:
Superpowers supplies the spec and linked implementation plan, Kata records the
work and evidence, and Roborev critiques intent and implementation.

## Usage

```text
$roborev-superpowers <feature or existing spec/plan>
```

## Explicit invocation only

Invocation must be explicit: literal personal `$roborev-superpowers`, plugin
`$roborev:roborev-superpowers`, or structured Codex skill selection.
Requests such as “help me implement this feature” without one of these explicit
mechanisms must use native behavior and must not run roborev.

Honor existing session authorization and project AGENTS.md instructions. This
invocation authorizes the review loop; commits, branches, pushes, external
messages, and changes to requirements still follow the user's authorized scope.
Resume from existing artifacts, open tasks, and evidence rather than restarting
completed stages. Use the installed Superpowers skills for their respective
stages and preserve their human decision points unless already waived by the user.
If Superpowers, Kata, or a goal-capable reviewer is unavailable, report the
missing prerequisite and the work that remains unverified.

## Connect intent to tracked work

1. Use Superpowers brainstorming to establish the intended outcome, constraints,
   non-goals, and observable success criteria. Keep its spec authoritative.
2. Search Kata first; reuse or create a parent for this outcome. Record the spec
   path, attention state, and existing branch when applicable. Review the spec
   with `roborev review --type goal --agent pi --wait --spec <spec-path>`.
   Quote paths as shell arguments. Select a configured Pi or supported bare-mode
   Claude reviewer; the implementing agent can be Codex or another agent.
3. Resolve findings against the captured artifacts. Correct incomplete intent or
   explain a false positive with evidence. A changed requirement returns to
   brainstorming and the user's decision; it is not an implementation shortcut.
   Request a new goal review after edits. Reruns review the old frozen snapshot.
4. Use Superpowers writing-plans to produce a plan linked through `**Spec:**`.
   Map each implementable task to a Kata child with its spec/plan references,
   deliverable, verification command, and expected result. Parent links group
   work; dependency links represent actual prerequisites. Mark deliberate
   deferrals explicitly. Leave unrelated project work intact.
5. Review the linked plan and the populated open graph together:

   ```bash
   roborev review --type goal --agent pi --wait \
     --spec docs/superpowers/specs/feature-design.md \
     --plan docs/superpowers/plans/feature.md
   ```

   Address in-scope findings in the spec, plan, or task graph. Record the review
   snapshot and outcome on the parent before executing the agreed plan.

For proposed Kata creates/edits, an installed Kata integration can call
`POST /api/goal-review` with the candidate before committing it. Respect its
`blocked` result and the user's gate policy. The endpoint only simulates a write;
atomic review-and-write and freshness checks belong to that integration. Without
such an integration, author the graph normally and run the explicit goal review
above. Do not claim that direct Kata writes were gated.

## Execute, review, and keep evidence

Use the selected Superpowers execution skill, inline or with authorized agents.
For each task, claim its Kata issue and maintain attention state. When delegating,
create tracked children and assign one writer to each issue's work metadata.

Follow Superpowers TDD: observe the intended test fail, implement the smallest
change that passes, and run the task's verification. Commit coherent slices when
authorized. Record commands, results, and commit references on the task. Read
Roborev code reviews at the configured cadence, verify each finding, fix valid
in-scope defects with regression tests, and record the disposition. Intent review
findings require artifact/task edits; they are not code-fix candidates.

When implementation exposes a plan defect or scope change, reconcile the spec,
plan, and Kata first and request a fresh goal review. Proceed against the updated
intent. Automatic watching is useful feedback, but a queued job is not a pass and
checkbox changes alone do not request another review.

## Complete the outcome

Use Superpowers verification-before-completion and finishing-a-development-branch
within the user's authorized delivery scope. Completion evidence includes the
success criteria exercised, verification commands and results, delivered commit
or PR, and review dispositions tied to the delivered code or intent snapshot.
Await required CI and current-head reviews before claiming them green. Close Kata
children with their evidence, then the parent once all required work is complete.
Leave both attention state and an explanation current for unfinished handoffs.

## Sandbox access

roborev uses a local daemon. If a command fails with `permission denied`, the
sandbox may be blocking its loopback port or Unix socket. Retry the same command
with `sandbox_permissions: "require_escalated"` when supported and authorized.
Do not start or restart the daemon because a sandboxed status probe cannot reach
it. Never replace a user's installed roborev binary to run this workflow.
