# Explicit Codex Skill Invocation Design

## Problem

Roborev's Codex skills are intended to be opt-in. An ordinary request such as
"Review the changes in this branch" must use Codex's native code-review
workflow and must not enqueue a roborev review.

The current skill frontmatter does not express that contract. For example,
`roborev-review-branch` is described as requesting a code review for the
current branch. Codex includes that description in the model-visible list of
implicitly available skills, so it closely matches an ordinary branch-review
request.

A safe live reproduction with `roborev` removed from `PATH` confirmed the
regression: gpt-5.6-sol loaded `roborev-review-branch` and attempted
`roborev review --branch --wait`; gpt-5.5 reviewed the diff directly.

## Invocation Contract

All bundled Codex roborev skills are explicit-only.

- `$roborev-<workflow>` explicitly invokes a CLI-installed personal skill.
- `$roborev:roborev-<workflow>` explicitly invokes a plugin-managed skill.
- Selecting a skill through Codex's structured skill UI also invokes it.
- Ordinary requests to review, fix, refine, comment on, or otherwise work with
  code do not invoke a roborev skill merely because a bundled workflow could
  perform the task.
- Pasted review findings remain direct task input unless the user explicitly
  requests a roborev skill or supplies identifiers that must be fetched.

The machine-readable policy is authoritative. Skill wording is a fallback for
Codex versions that do not support the policy and a guard if a skill body is
loaded unexpectedly.

## Design

### Machine-enforced activation policy

Each directory under `internal/skills/codex/` gains
`agents/openai.yaml` containing:

```yaml
policy:
  allow_implicit_invocation: false
```

Current Codex versions exclude skills with this policy from the model-visible
implicit skill catalog while retaining explicit `$skill-name` and structured
UI invocation. Hidden skills cannot be resolved from an unsigiled prose name;
the literal invocation syntax is therefore part of the contract. This prevents
semantic overlap in descriptions from activating roborev.

### Skill wording

Each Codex skill frontmatter description will state that it is used only when
the user explicitly invokes that skill. Each body will place the same activation
contract before workflow instructions and include representative non-trigger
examples where ambiguity is likely.

Descriptions remain useful for skill pickers, but Codex does not use them to
resolve explicit mentions and they are not an enforcement boundary.

The existing generator derives all Factory Droid skills and the Claude fix,
refine, and respond skills from Codex bodies. Explicit-only wording is intended
to propagate to those generated files. The Codex-specific policy metadata is
not generated or installed for other agents.

### Installation and status

The embedded-skill installer currently copies only `SKILL.md`. It will also
embed and copy `agents/openai.yaml` for Codex skills.

Status checks will treat a Codex skill as outdated when its installed policy
file is absent or differs from the embedded version. Therefore `roborev
update` repairs existing installations rather than reporting them current
while leaving implicit invocation enabled.

Claude Code and Factory Droid installations retain their existing file sets.
Codex plugin distribution uses the same skill directories, so it receives the
policy files without a separate implementation. Because Codex namespaces
plugin skills, the Codex plugin's suggested prompts will use literal
`$roborev:roborev-*` invocations instead of broad natural language such as
"Review the current branch with roborev," keeping the plugin entry points
consistent with the explicit-only contract.

## Evaluation

### Deterministic tests

Unit tests will verify that:

- every embedded Codex skill has an `agents/openai.yaml` policy disabling
  implicit invocation;
- installation writes the policy file for every Codex skill;
- a missing or changed installed policy marks the skill outdated;
- updating an older installation adds the policy file;
- Codex plugin default prompts explicitly name their target skills;
- derived skill files remain current.

### Live Codex conformance eval

An opt-in test target will run the installed `codex` CLI against a disposable
git repository and isolated `CODEX_HOME`. It will install the in-tree Codex
skills and place a harmless `roborev` stub first on `PATH`.

For each requested model, the eval will run at least these cases:

| Prompt | Expected behavior |
| --- | --- |
| `Review the changes in this branch.` | No roborev command |
| `Fix the issues you find in this branch.` | No roborev command |
| `$roborev-review-branch` | Invoke `roborev review --branch --wait` |

The target will default to the current model under investigation and accept a
comma-separated model list so gpt-5.5 and gpt-5.6-sol can be compared. It is
excluded from ordinary test runs because it requires Codex authentication,
network access, and model usage.

The stub never contacts a daemon or writes review state. The eval asserts on
Codex JSONL command-execution events rather than generated prose.

## Documentation

The agent-skills guide will say that Codex roborev skills are explicit-only and
show both personal `$roborev-*` and plugin `$roborev:roborev-*` invocation
syntax. Development documentation will include the opt-in conformance-eval
command and its prerequisites.

## Compatibility and Failure Handling

- Older Codex releases ignore unknown `agents/openai.yaml` metadata and still
  receive the explicit-only wording.
- A missing Codex executable, authentication, or model access causes the live
  eval to skip or fail with a specific prerequisite message; it never silently
  reports conformance.
- A failed negative case reports the exact attempted command. A failed
  positive case reports that explicit invocation did not reach the stub.
- Ordinary unit tests remain deterministic and offline.
