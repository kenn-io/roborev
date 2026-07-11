---
title: Agent Skills
description: Install slash commands that let AI agents request reviews and fix findings
---

Install slash commands that let AI agents request reviews and fix findings directly:

```bash
roborev skills install
```

!!! tip "Prefer the async workflow for day-to-day reviews"
    The recommended roborev workflow is **async reviews + TUI**: reviews run in the background (via hooks or `roborev review`), and you browse, address, and close them in `roborev tui`. This creates a persistent accountability loop where open findings stay visible until resolved.

    The review skills below are a convenience for **requesting ad-hoc reviews from within an agent session**. They use `--wait` internally so the agent can present findings inline. For routine reviews, rely on the post-commit hook and check the TUI rather than requesting reviews through your agent.

## Available Skills

| Skill | Description |
|-------|-------------|
| `/roborev-review [commit] [--type ...]` | Request a code review for a commit |
| `/roborev-review-branch [--base ...] [--type ...]` | Review all commits on the current branch |
| `/roborev-design-review [commit]` | Request a design review for a commit |
| `/roborev-design-review-branch [--base ...]` | Design review all commits on the current branch |
| `/roborev-lookahead-review [commit] [--panel <name>\|none]` | Check a commit for time-series look-ahead bias |
| `/roborev-lookahead-review-branch [--base <branch>] [--panel <name>\|none]` | Check all branch commits for time-series look-ahead bias |
| `/roborev-fix [job_id...]` | Discover and fix all open review findings in one pass |
| `/roborev-refine [--since ...] [--branch ...] [--max-iterations ...]` | Iterative review-fix-review loop until all reviews pass |
| `/roborev-respond <job_id> [message]` | Add a response to document changes |

## Usage

!!! note "Explicit invocation"
    All bundled roborev skills are **explicit-only**. An ordinary request such
    as "Review the changes in this branch" uses your agent's native behavior;
    it must not activate a roborev skill or run roborev.

    **Claude Code** enforces this in skill metadata: the bundled skills set
    `disable-model-invocation: true`, so the model never selects a roborev
    skill on its own. Invoke a skill by typing its slash command
    (`/roborev-review-branch`) or picking it from the `/` menu. Plugin-managed
    skills use the plugin namespace: `/roborev:roborev-review-branch`. The one
    exception is `/roborev-fix`, which stays model-invocable so
    [`roborev agent-hook`](../agent-hook.md) can instruct a session to run it;
    its description still permits only explicit invocation.

    **Codex** explicit invocation has three supported forms:

    - For skills installed by `roborev skills install`, replace the leading `/`
      in the examples below with `$`: `$roborev-review-branch`.
    - For plugin-managed skills, qualify the same skill with the plugin namespace:
      `$roborev:roborev-review-branch`.
    - Select the roborev skill directly in Codex's structured skill picker.

    The namespace distinguishes plugin-contributed skills from personal skills
    that may have the same name. See the [syntax table](#agent-specific-syntax)
    for more examples.

### Review a commit

Request a code review without leaving your agent session:

```
/roborev-review
/roborev-review abc123
/roborev-review --type security
```

The skill enqueues a review and waits for the result so it can present findings inline. If you already have reviews queued from the post-commit hook, use `/roborev-fix` to address them instead of requesting new ones.

### Review a branch

Review all commits since the current branch diverged from main:

```
/roborev-review-branch
/roborev-review-branch --base develop
/roborev-review-branch --type security
```

The skill enqueues a branch review and waits for results so the agent can present them inline.

### Design review

Request a design-focused review that evaluates completeness, feasibility, and task scoping:

```
/roborev-design-review
/roborev-design-review abc123
```

Enqueues a design review and waits for the result, following the same pattern as the other review skills.

### Design review a branch

Review all commits on the current branch with a design-focused lens:

```
/roborev-design-review-branch
/roborev-design-review-branch --base develop
```

This is the branch equivalent of `/roborev-design-review`.

### Look-ahead review a commit

Request a time-series review that checks whether a change uses information that
would not have been available at the point being predicted:

```
/roborev-lookahead-review
/roborev-lookahead-review abc123
/roborev-lookahead-review --panel forecasting
```

With no commit argument, the skill reviews `HEAD`. Use `--panel none` to disable
an otherwise configured review panel.

### Look-ahead review a branch

Run the same future-data leakage check across all commits on the current branch:

```
/roborev-lookahead-review-branch
/roborev-lookahead-review-branch --base develop
/roborev-lookahead-review-branch --panel forecasting
```

The skill compares the branch with its merge base by default, or with the branch
specified by `--base`, and waits to present the result inline.

### Fix all open reviews at once

The most powerful skill is `/roborev-fix`. With no arguments it discovers all open failed reviews on recent commits and fixes them in a single pass:

```
/roborev-fix
```

You can also target specific jobs:

```
/roborev-fix 1019 1021
```

The agent:
1. Discovers open reviews (or uses provided job IDs)
2. Fetches all reviews and collects findings
3. Groups findings by file and prioritizes by severity
4. Fixes all issues across all reviews
5. Runs tests to verify
6. Records a comment on each closed review
7. Offers to commit

This is the interactive equivalent of `roborev fix --batch` -- the agent sees all findings at once and can make coordinated fixes across related issues.

### Fix a single review

Target a specific job ID with `/roborev-fix`:

```
/roborev-fix 1019
```

The agent fetches the review, fixes issues by priority, runs tests, and offers to commit.

!!! note
    The `/roborev-address` skill is deprecated. Use `/roborev-fix <job_id>` instead, which handles both single and multi-review fixes.

### Refine a branch

`/roborev-refine` runs an iterative review-fix-review loop on your branch. It finds failed reviews, fixes them, waits for re-review, and repeats until everything passes or the iteration limit is reached:

```
/roborev-refine
/roborev-refine --max-iterations 5
/roborev-refine --since HEAD~3
/roborev-refine --branch feature-xyz
```

| Flag | Description |
|------|-------------|
| `--since <commit>` | Refine commits after this commit (exclusive); required on the default branch |
| `--branch <name>` | Validate that the current branch matches before refining |
| `--max-iterations <n>` | Maximum fix-review cycles (default: 10) |

Unlike `roborev refine` on the CLI, the skill performs the full workflow inside your agent session: it reviews via the daemon, fixes findings inline, commits, and re-reviews. This gives the agent direct access to the codebase while fixing, which can produce better results than the CLI's isolated worktree approach.

## Agent-Specific Syntax

| Agent | Syntax |
|-------|--------|
| Claude Code, personal install | `/roborev-review`, `/roborev-review-branch`, `/roborev-design-review`, `/roborev-design-review-branch`, `/roborev-lookahead-review`, `/roborev-lookahead-review-branch`, `/roborev-fix`, `/roborev-refine`, `/roborev-respond` |
| Claude Code, plugin install | `/roborev:roborev-review`, `/roborev:roborev-review-branch`, `/roborev:roborev-design-review`, `/roborev:roborev-design-review-branch`, `/roborev:roborev-lookahead-review`, `/roborev:roborev-lookahead-review-branch`, `/roborev:roborev-fix`, `/roborev:roborev-refine`, `/roborev:roborev-respond` |
| Factory Droid | `/roborev-review`, `/roborev-review-branch`, `/roborev-design-review`, `/roborev-design-review-branch`, `/roborev-lookahead-review`, `/roborev-lookahead-review-branch`, `/roborev-fix`, `/roborev-refine`, `/roborev-respond` |
| Codex, personal install | `$roborev-review`, `$roborev-review-branch`, `$roborev-design-review`, `$roborev-design-review-branch`, `$roborev-lookahead-review`, `$roborev-lookahead-review-branch`, `$roborev-fix`, `$roborev-refine`, `$roborev-respond` |
| Codex, plugin install | `$roborev:roborev-review`, `$roborev:roborev-review-branch`, `$roborev:roborev-design-review`, `$roborev:roborev-design-review-branch`, `$roborev:roborev-lookahead-review`, `$roborev:roborev-lookahead-review-branch`, `$roborev:roborev-fix`, `$roborev:roborev-refine`, `$roborev:roborev-respond` |

Codex can also invoke either installation by selecting the skill in its
structured skill picker. Skill descriptions intentionally state only the
explicit invocation requirement; workflow details live in the skill body so
ordinary prose cannot semantically match a capability summary. Claude Code
skills additionally set `disable-model-invocation: true` in their frontmatter
(except `roborev-fix`, which the agent-hook instruction invokes), so Claude
Code never auto-selects a roborev skill — only user invocation via the slash
command or `/` menu loads it.

## Checking Skill Status

See which skills are installed and whether any need updating:

```bash
roborev skills
```

The output shows each skill with per-agent status. Skills are checked for both Claude Code and Codex (if installed):

```
Skills:

  roborev-fix
  Discover and fix all open review findings in one pass

    Claude Code (installed)     /roborev-fix
    Codex (not installed)       $roborev-fix
```

Status values: `installed`, `outdated`, `not installed`, `no agent` (binary not found).

## Updating Skills

Skills are updated automatically when you run:

```bash
roborev update
```

## How It Works

Skills are installed as agent-specific configuration:

- **Claude Code**: Custom slash commands under
  `$CLAUDE_CONFIG_DIR/skills/` when `CLAUDE_CONFIG_DIR` is set, otherwise
  `~/.claude/skills/`
- **Codex**: Custom agent skills under `$CODEX_HOME/skills/` when `CODEX_HOME`
  is set, otherwise `~/.codex/skills/`
- **Factory Droid**: Custom skills under `~/.factory/skills/`

The same resolved directories are used when installing, updating, and checking
skill status. Claude Code agent-hook installation also honors
`CLAUDE_CONFIG_DIR`; Codex agent-hook installation honors `CODEX_HOME`.

The review skills use `--wait` internally so the agent can present results inline. The fix skills call `roborev show --job <id> --json` to fetch review data, then parse and present findings to the agent in a structured format. All reviews (whether requested via skills or the post-commit hook) appear in the TUI queue.

## Plugin Distribution

Starting in 0.56, the roborev repository also ships agent plugin manifests that point at the same skill trees:

- `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` for the Claude Code plugin marketplace.
- `.codex-plugin/plugin.json` for the Codex plugin system.

These let you install roborev skills through each agent's native plugin channel as an alternative to `roborev skills install`. The skill content is identical; the difference is who manages updates: `roborev skills install` is updated when you run `roborev update`, while plugin-managed installs follow each agent's plugin lifecycle.

Codex namespaces skills supplied by plugins to avoid collisions with personal
skills. Invoke a plugin-managed skill as `$roborev:roborev-<workflow>` (for
example, `$roborev:roborev-fix`); invoke a personal skill installed by roborev
as `$roborev-<workflow>` (for example, `$roborev-fix`). Both forms are explicit
invocations. General requests such as "fix the issues in this branch" remain
native Codex tasks and do not select roborev.

Claude Code likewise namespaces plugin-managed skills: invoke them as
`/roborev:roborev-<workflow>` (for example, `/roborev:roborev-fix`). Personal
skills installed by `roborev skills install` keep the plain `/roborev-<workflow>`
form. Either way, the bundled `disable-model-invocation: true` policy means
only you can invoke them; Claude never selects a roborev skill for an
ordinary request. `roborev-fix` alone omits the policy so the
[agent-hook](../agent-hook.md) instruction can invoke it, and relies on its
explicit-only description instead.

## Waiting for Hook-Triggered Reviews

When a post-commit hook already enqueues reviews, agents don't need
`roborev review --wait` (which would create a duplicate job). Use
`roborev wait` instead:

```bash
git commit -m "Fix auth validation"   # Hook triggers review
roborev wait --quiet                  # Block until verdict (exit 0=pass, 1=fail)
```

This is more token-efficient than polling `roborev list` or
`roborev show` because the agent makes a single blocking call and
reads the exit code. See [Waiting for a Review Without Enqueuing](/guides/reviewing-code/#waiting-for-a-review-without-enqueuing) for the full flag reference.

## Skills vs Async Reviews

For most workflows, the **async approach** is better: reviews run automatically via the post-commit hook, results accumulate in the TUI, and you address them when ready. This keeps your agent session focused on writing code and creates a persistent record of what needs attention.

Skills are useful when you want to **explicitly request a review** during an agent session, for example to review uncommitted changes or to get a design review before committing. The `/roborev-fix` skill is valuable in any workflow because it pulls findings from the TUI queue and addresses them within your session. The `/roborev-refine` skill goes further, running an iterative loop that re-reviews after each fix until everything passes.

For **fully automated** fixing outside an agent session, use `roborev fix --batch` (headless, no agent interaction) or `roborev refine` (iterative loop until all reviews pass).

## See Also

- [Auto-Fix Agentic Loop with Refine](/guides/auto-fixing/): Automated fix loop
- [Commands Reference](/commands/): Full command list
