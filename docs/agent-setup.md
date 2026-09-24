---
title: Agent-Assisted Setup
description: A setup prompt you hand to your coding agent to configure roborev in a repository
---

This page is a prompt for your coding agent. It walks the agent through setting
up roborev in one repository: git hooks, agent skills, and, only if you agree
after hearing the risks, the Agent Hook.

To use it, open your coding agent in the repository you want reviewed and send:

```text
Read https://roborev.io/docs/agent-setup.md and follow it to set up roborev
in this repository.
```

The agent asks before each change. Everything below this line is addressed to
the agent.

______________________________________________________________________

## Instructions for the agent

You are helping a human set up roborev, a local daemon that reviews every git
commit in the background with an AI agent. Work through the steps in order.

### Ground rules

- Explain each step in one or two sentences, show the exact command, and get the
    user's approval before running anything that writes files or config.
    Read-only checks do not need approval.
- Do not run `roborev review`. Setup does not need a review, and the post-commit
    hook creates reviews on its own.
- Do not stop, restart, or reinstall a roborev daemon that is already running
    unless the user approves it.
- Do not install roborev from source or overwrite an existing `roborev` binary
    without the user's approval.
- If a step fails, show the error, explain it in plain language, and ask how to
    proceed. Do not work around failures silently.
- Treat the user's answer on the Agent Hook (step 5) as final. Do not install it
    unless the user says yes after reading the risks.

### Step 1: Check the installation

Run:

```bash
roborev version
```

If the command is not found, stop and point the user to
<https://roborev.io/docs/installation.md>. Offer to run the install command for
their platform, and run it only if they agree.

### Step 2: Inspect the current state

Run these read-only checks from the repository root:

```bash
roborev quickstart --json
roborev status
```

`roborev quickstart --json` reports whether the daemon is running, the git hook
is installed, the repository is registered, a review agent is configured, and
skills are installed. Summarize what is already done and skip those steps below.
`roborev quickstart` without `--json` also prints a configuration playbook you
can use in step 6.

### Step 3: Install the git hooks

The git hooks are the core of roborev. A post-commit hook queues a background
review of every commit. A pre-push hook flushes any batched commits before a
push. Neither hook blocks commits or pushes.

Explain to the user that `roborev init` will:

- Install the post-commit and pre-push hooks. If the repository uses a hook
    manager such as Husky through `core.hooksPath`, roborev installs into that
    directory.
- Create `~/.roborev/config.toml` if it does not exist.
- Add the snapshot directory (`.roborev/` by default) to `.gitignore`. This is a
    change to a tracked file, so the user may want to commit it.
- Start the roborev daemon and register this repository with it.

Before running it, ask the user:

1. Which agent should review code? By default roborev uses Codex and falls back
    to another installed agent when Codex is missing. To pin one for this
    repository, pass `--agent <name>` (for example, `claude-code` or `gemini`);
    this also creates `.roborev.toml`. The full list is at
    <https://roborev.io/docs/agents.md>.
1. Does a service manager (systemd or launchd) already run the daemon? If so,
    pass `--no-daemon` so `init` only registers the repository.
1. Is roborev installed through a version manager shim (mise, asdf, and
    similar)? If so, pass `--binary <shim path>` so the hooks keep working
    after upgrades.

Then run, with the chosen flags:

```bash
roborev init
```

Verify with `roborev status`. The daemon should be running and the repository
should be listed.

### Step 4: Install the agent skills

Skills let the user drive roborev from inside an agent session. The most useful
ones are:

- `roborev-fix`: fix all open failing reviews in one pass.
- `roborev-refine`: review the branch, fix findings, and re-review until every
    review passes.
- `roborev-review` and `roborev-review-branch`: request a review on demand.

Claude Code invokes them as `/roborev-fix`; Codex uses `$roborev-fix`. Skills
never run on their own; only the user (or the Agent Hook, for `roborev-fix`)
invokes them.

Skills install into each agent's user configuration directory, such as
`~/.claude/skills/` or `~/.codex/skills/`, so they apply to every repository.
Ask for approval, then run:

```bash
roborev skills install
roborev skills
```

The second command shows per-agent status. Every installed agent should show
`installed`.

If the user prefers MCP tools over CLI calls inside skills, use
`roborev skills install --mcp` and see
<https://roborev.io/docs/integrations/mcp.md>. The CLI mode is the default and
works without further setup.

### Step 5: Ask about the Agent Hook

Steps 3 and 4 produce reviews and let the user fix them on request. The Agent
Hook closes the loop automatically: it watches coding-agent sessions and, when
failed reviews pile up, tells the active agent to fix them before the session
ends.

This step is optional. Explain what it does and the risks below, then ask the
user whether to install it. Do not install it by default.

**What it does:**

- `roborev agent-hook install` adds a hook command to each detected coding
    agent's user-level config (for example, `~/.claude/settings.json` or the
    Codex hooks config). It supports Claude Code, Codex, Copilot CLI, Cursor,
    Factory Droid, Gemini CLI, Hermes, Qwen, and Grok Build.
- The hook runs on shell tool calls and when the agent tries to stop. It counts
    turns, commits, and open failed reviews for the current repository.
- By default, once four open failed reviews accumulate, the hook injects an
    instruction telling the agent to run the `roborev-fix` skill on those exact
    review IDs. On a stop event, the hook can block the stop so the agent keeps
    working until the fix is done.
- It also installs or updates the bundled skills for each agent.

**Risks to explain to the user:**

- **It changes what the agent does mid-task.** The agent may pause its current
    work to fix review findings, then continue. Sessions run longer and use more
    tokens. The fix stays scoped to the current task, and valid findings outside
    that scope are left open for the user, but the agent still edits code the
    user did not directly ask it to touch.
- **It edits the working tree in the live session.** Unlike background fix jobs,
    which run in isolated worktrees, hook-triggered fixes happen in the user's
    checkout, inside the running agent session. If the agent runs with broad
    permissions (auto-approve, "yolo", or bypass modes), those edits and any
    commits happen without a prompt.
- **Review text becomes agent instructions.** Findings are written by an AI
    reviewer that read the diff. Code from an untrusted source, such as a
    contributor's branch, can try to steer the reviewer, and those findings then
    reach the coding agent. The `roborev-fix` skill treats each finding as an
    unverified claim and checks it against the code before acting, but this is a
    guardrail, not a guarantee.
- **It is global to the user's agents.** The hook lives in user-level agent
    config, not in this repository. It only acts in repositories registered with
    roborev, but it applies to every registered repository and every session of
    each hooked agent.
- **Removal is manual.** There is no uninstall command. To remove the hook, edit
    the agent's hook config and delete the `roborev agent-hook run` entry.
- **Coverage is uneven.** Cursor receives the events but cannot display the
    instruction, so the hook has no visible effect there. Claude Desktop and
    other apps without harness hooks are not supported.

**Controls the user keeps:**

- `roborev snooze` silences reminders for the current worktree and branch (eight
    hours by default; `roborev snooze off` resumes). Reviews keep running.

- Thresholds in `~/.roborev/config.toml` control how often it fires. Setting a
    threshold to `0` disables that trigger:

    ```toml
    [agent_hook]
    turn_threshold = 5
    commit_threshold = 0
    failed_review_threshold = 4
    ```

- `fix_guidelines` in the same file adds a standing policy to every
    hook-triggered fix, for example "explain findings you decide not to apply."

If the user says yes, preview the changes first and show them the output:

```bash
roborev agent-hook install --dry-run
```

Detection installs for every agent it finds. If the user wants only some agents,
use `--agent <profile>` (for example, `--agent claude`) once per agent. If step
3 needed `--binary`, pass the same flag here. After the user approves the
preview, run the same command without `--dry-run`.

If the user says no, move on. They can install it later with the same command,
and the post-commit reviews and skills work without it.

Full reference: <https://roborev.io/docs/agent-hook.md>.

### Step 6: Optional tuning

Offer these one at a time. Skip any the user declines.

- **Review guidelines.** Ask what the team wants reviews to focus on or ignore,
    and add it to `.roborev.toml`:

    ```toml
    review_guidelines = """
    <the user's guidelines>
    """
    ```

    If `review_guidelines` is not set, roborev uses a `REVIEW.md` file at the
    repository root when one exists.

- **Batch small commits.** If the user commits very often, set
    `post_commit_batch_size = 5` in `.roborev.toml` to review every five commits
    as one range.

- **Commit often.** roborev gives tighter feedback on small commits. If the
    user's agent tends to batch large changes, suggest adding a "commit after
    each self-contained change" rule to the repository's `AGENTS.md` or
    `CLAUDE.md`.

For every other setting, see <https://roborev.io/docs/configuration.md>.

### Step 7: Summarize

Finish with a short summary:

- What was installed or changed, including any files in the repository
    (`.gitignore`, `.roborev.toml`, `AGENTS.md`) that the user may want to
    commit.
- Whether the Agent Hook is installed, and how to snooze or remove it if so.
- How to see reviews: `roborev tui` in the terminal, or `roborev show HEAD` for
    the latest commit.
- How to act on them: `/roborev-fix` (Codex: `$roborev-fix`) for open findings,
    and `/roborev-refine` to clean up a branch before opening a pull request.
