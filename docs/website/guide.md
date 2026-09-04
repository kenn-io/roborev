# The roborev guide: a ten-stop tour of the review loop

A review loop you set up once and stop thinking about. Ten stops from a fresh
repo to a team running the same reviewers on every commit and every pull
request. Every stop links into the
[documentation](https://roborev.io/docs/) for exact commands.

## 01 / Install once, initialize per repo

roborev is a single Go binary. `roborev init` installs a post-commit hook,
starts the daemon if it is not running, and registers the repository. It
detects hook managers like Husky through `core.hooksPath` and never edits
tracked source. Point your coding agent at `roborev quickstart` and it will
inspect the repo and finish the setup.

```sh
curl -fsSL https://roborev.io/install.sh | bash
cd your-repo
roborev init
roborev quickstart --json
```

→ [Quick start](https://roborev.io/docs/quickstart/),
[Installation](https://roborev.io/docs/installation/)

## 02 / Commit and keep going; the review runs behind you

Each commit queues a background review through the daemon's worker pool and
returns immediately; neither you nor the agent waits on it. The
first installed agent in the fallback order reads the diff, the commit message,
your guidelines, and recent review history, and returns a verdict with findings,
severities, and file locations. Duplicate hook requests for the same target are
coalesced. Small commits can be batched with `post_commit_batch_size`.

```sh
git commit -m "Add retry to payment webhook"
roborev status                       # daemon, queue depth, running jobs
roborev show HEAD                    # the rendered review for the latest commit
roborev review --branch --type security   # or ask for one explicitly
```

→ [Post-commit reviews](https://roborev.io/docs/automation/post-commit-reviews/),
[Reviewing code](https://roborev.io/docs/guides/reviewing-code/)

## 03 / Read the ledger, not the scrollback

Every review lives in a persistent queue with its verdict, findings, the exact
prompt, and the agent log. `roborev tui` shows the queue in the terminal with
vim keys; `roborev ui` opens the browser workspace served by the same daemon.
Both read the same SQLite history. A review stays open until you or an agent
closes it.

→ [Terminal UI](https://roborev.io/docs/integrations/tui/),
[Browser UI](https://roborev.io/docs/web-ui/)

## 04 / Give the finding back to the agent

Press `y` in the TUI to copy a review and paste it into the agent session. From
Claude Code or Codex, `/roborev-fix` pulls every open failing review for the
branch, fixes valid findings, documents invalid ones, runs the tests, and closes
the reviews. With no session open, `roborev fix` hands the findings to an agent
that applies changes and commits; the new commit is reviewed automatically.

→ [Agent skills](https://roborev.io/docs/guides/agent-skills/),
[Responding to reviews](https://roborev.io/docs/guides/responding-to-reviews/)

## 05 / Let the session fix its own reviews

`roborev agent-hook install` detects Claude Code, Codex, Copilot CLI, Cursor,
Factory Droid, Gemini CLI, Hermes, and Qwen and installs their native lifecycle
hooks. The daemon counts turns, commits, and failed reviews per session; when a
threshold trips, the agent is reminded to run the fix skill for exactly the
review IDs that are open. Reminders never run `roborev fix` on their own, and
`roborev snooze` silences a worktree while you focus.

```toml
# ~/.roborev/config.toml
[agent_hook]
turn_threshold = 3
failed_review_threshold = 1
```

→ [Agent Hook](https://roborev.io/docs/agent-hook/)

## 06 / Refine the branch before anyone else reads it

`roborev refine` finds the oldest failed review on the branch, fixes it in an
isolated worktree, commits, waits for the re-review, and repeats. When every
per-commit review passes it runs a whole-branch review and loops on that too,
until green or `--max-iterations`. The same loop runs as `/roborev-refine`
inside the agent session.

```sh
roborev refine --max-iterations 5 --min-severity high
roborev refine --list                # preview, change nothing
roborev refine --since HEAD~3        # only the last three commits
```

→ [Auto-fix with refine](https://roborev.io/docs/guides/auto-fixing/)

## 07 / Choose the reviewer per repo and per job

A committed `.roborev.toml` names the agent, model, and reasoning level for
each workflow, a backup agent for quota failures, and the review guidelines
every reviewer must follow. Global defaults live in `~/.roborev/config.toml`.

```toml
agent = "codex"
review_agent_fast = "gemini"
security_agent = "claude-code"
security_model = "opus"
backup_agent = "claude-code"
post_commit_batch_size = 3

review_guidelines = """
Flag any network call without a timeout.
Migrations must be reversible.
"""
```

→ [Configuration](https://roborev.io/docs/configuration/),
[Supported agents](https://roborev.io/docs/agents/)

## 08 / Add reviewers when the change deserves them

A review panel fans one target out to named subagents, each with its own agent,
review type, instructions, and timeout, and a synthesis agent merges them into
one review. Built-in `security`, `design`, and `lookahead` types swap the
rubric; custom types wrap your own Go template. `roborev analyze` runs
duplication, complexity, dead-code, refactoring, API-design, architecture,
security, and test-fixture passes over existing code, and `roborev compact`
verifies open findings against the current tree.

```toml
[review.subagents.security]
agent = "claude-code"
review_type = "security"
reasoning = "thorough"

[review.panels.branch_final]
members = ["bug", "security", "design"]
synthesis_agent = "codex"
```

```sh
roborev review --branch --panel branch_final
roborev analyze dead-code ./... --fix
roborev compact
```

→ [Review panels](https://roborev.io/docs/advanced/subagent-review-panels/),
[Custom review types](https://roborev.io/docs/advanced/custom-review-types/),
[Code analysis](https://roborev.io/docs/guides/assisted-refactoring/)

## 09 / Run the same panel on every pull request

Enable the CI poller and the daemon lists open pull requests on GitHub or merge
requests on GitLab, runs one panel per PR head against the frozen merge-base
range, and posts one synthesized comment. Labels in `skip_labels` exempt drafts
and generated updates. Authenticate as a GitHub App for a bot with
repository-scoped permissions, or use your `gh` login.

```toml
[ci]
enabled = true
repos = ["acme/payments", "acme/ledger"]
poll_interval = "5m"
panel = "branch_final"
skip_labels = ["draft", "dependencies"]
```

→ [GitHub integration](https://roborev.io/docs/integrations/github/),
[GitLab integration](https://roborev.io/docs/integrations/gitlab/)

## 10 / Run it as a team, keep it on your machines

Every daemon keeps its own SQLite database. Turn on `[sync]` and each machine
pushes and pulls review history through a shared PostgreSQL database,
deduplicated by UUID. Review event hooks route outcomes to Slack, desktop
notifications, or a kata or beads issue tracker. Analytics and JSON exports show
what review costs, how long it takes, and what it catches; configuration
experiments compare two setups on real branches.

→ [PostgreSQL sync](https://roborev.io/docs/advanced/postgres-sync/),
[Review event hooks](https://roborev.io/docs/guides/hooks/),
[Analytics](https://roborev.io/docs/web-ui/#analytics)

## After the tour

Run `roborev init`, make a commit, open `roborev tui`. Add the agent hook when
the queue starts to matter.

→ [Start in five minutes](https://roborev.io/docs/quickstart/)
