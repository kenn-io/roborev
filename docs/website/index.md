# roborev: local, continuous code review for the agentic loop

Find bugs faster and ship better quality code. roborev is a review daemon on
your machine. A git hook reviews each commit in the background with the agents
you already run, while you keep working.

```text
$ roborev init
Ready! Every commit will now be automatically reviewed.
$ git commit -m "Add retry to payment webhook"
$ roborev show HEAD                   # later, whenever you want it
Review for 8f2c1a0 (job 214, by codex)
------------------------------------------------------------
## Review Findings

- **Severity**: High
- **Location**: `internal/webhook/retry.go:41`
- **Problem**: A timeout after the POST was delivered retries it, so the
  webhook can be charged twice. Nothing makes the request idempotent.
```

One Go binary, no runtime dependencies, MIT licensed. Install commands are
under 10 / Start.

## 01 / The gap: agents commit faster than anyone reads

A coding agent can land dozens of commits in an hour. Review still arrives at
the pull request: once, late, after the agent has moved on and the reasoning
that produced the bug is gone from its context. roborev moves review to the
commit, runs it in the background on your own machine, and gives the result
back to the thing that can fix it fastest.

- **Trigger: commit.** A git post-commit hook queues the review and returns.
  No CI job, no bot install, nothing waits on the result.
- **Agents: 11.** Codex, Claude Code, Gemini, Copilot, OpenCode, Cursor, Kiro,
  Kilo, Droid, Pi, and Grok Build, auto-detected.
- **Hosted services: 0.** Reviews run through the agent CLIs already on your
  machine, on the subscriptions and keys you already pay for.
- **Binary: 1.** Daemon, CLI, terminal UI, and browser UI in one Go binary.
  SQLite underneath.
- **Findings closed by: you.** Reviews are a ledger. A finding stays open until
  a person or an agent addresses it and closes it.

## 02 / The loop: it runs while you work

Two automation layers close the loop. The post-commit hook reviews every commit
with any agent or editor. The agent hook watches supported coding-agent
sessions and, once review work piles up, delivers the exact review IDs to the
`roborev-fix` skill before the session goes cold. Neither layer touches your
working tree on its own.

- **Layer 1, post-commit.** `roborev init` installs the hook, starts the
  daemon, and registers the repo. Each commit gets a verdict and, when it
  fails, findings with severities and file locations. `post_commit_batch_size`
  reviews a run of small commits as one range.
- **Layer 2, agent hook.** `roborev agent-hook install` wires Claude Code,
  Codex, Copilot CLI, Cursor, Factory Droid, Gemini CLI, Hermes, and Qwen. After
  a configurable number of turns, commits, or failed reviews, the agent is told
  which reviews to fix, by ID, and told to verify and close them.
- **Skills.** `/roborev-fix` pulls every open failing review for the branch,
  fixes the valid findings, documents the invalid ones, and closes them.
  `/roborev-refine` reviews, fixes, commits, and re-reviews the whole branch
  until every review passes, before the PR exists.
- **Headless.** `roborev fix` hands open findings to an agent that applies
  changes and commits. `roborev refine` runs the iterate-until-green loop in an
  isolated worktree with `--max-iterations` and `--min-severity` as guardrails.

## 03 / The ledger: nothing closes itself

roborev keeps every review in a persistent queue with a verdict, findings, the
exact prompt, and the agent log. `roborev tui` shows the queue and the selected
review side by side in the terminal. `roborev ui` opens the browser workspace
served by the same daemon over the same SQLite history. Open findings stay
visible until someone closes them, with a response on record if they disagree.

## 04 / Agents: bring the agents you already have

roborev orchestrates the agent CLIs already installed on the machine, so the
reviewer can be a different vendor than the writer, a cheaper model than the
writer, or a local model behind a proxy.

- **Routing.** `review_agent_fast`, `fix_model_thorough`,
  `security_backup_agent`, and friends route each workflow to its own agent,
  model, and reasoning level.
- **Failover.** Transient failures retry; exhausted retries or a quota error
  fail over to the backup agent; rate-limited agents sit in a cooldown parsed
  from the provider's reset message.
- **Guidelines.** Per-repo `review_guidelines` in `.roborev.toml` (or a
  `REVIEW.md`) ride along on every review, with global guidelines underneath.
  Kata task context comes too when the repo is bound to a kata project.
- **Anywhere.** Point Claude Code at Ollama or LiteLLM with a `model@base_url`
  spec, or add any Agent Client Protocol adapter as a named agent.

## 05 / Depth: several reviewers, one verdict

- **Panels.** Define `[review.subagents.*]` once, group them into
  `[review.panels.*]`, and run `roborev review --branch --panel branch_final`.
  A synthesis agent merges the members into one review; optional members can
  flake without blocking it.
- **Review types.** `--type security`, `--type design`, and `--type lookahead`
  swap the system prompt. Custom types wrap a Go template rubric in the same
  schema, severity filtering, and verdict.
- **Analysis.** `roborev analyze` runs duplication, complexity, dead-code,
  refactor, api-design, architecture, security, and test-fixtures passes over
  existing code. Findings land in the same queue; `--fix` applies them.
- **Verification.** `roborev compact` re-checks open findings against the
  current code, drops the ones that no longer hold, and consolidates the rest.

## 06 / Pull requests: the same panel on every PR

The daemon polls GitHub and GitLab, runs one panel per PR head against the
frozen merge-base range, and posts one synthesized comment with the member
reviewers, runtimes, and costs in the footer.

- **GitHub.** Install as a GitHub App for a bot with repository-scoped
  permissions and checks, or use your `gh` login for a personal project.
- **GitLab.** Merge request notes from the same daemon, with quick-action
  escaping.
- **Control.** `[ci].skip_labels` exempts drafts and generated updates; per-repo
  `.roborev.toml` overrides the panel, agents, and reasoning for one project.
- **Runs anywhere.** The CI poller is the roborev daemon under systemd or
  launchd on a box you own. No per-seat pricing, no code leaving your network.

## 07 / Visibility: know what review costs and what it catches

- **Analytics.** The browser UI reports cost, latency, reliability, and
  outcomes, filtered by project, source, agent, model, or time range, with the
  filters in the URL. `roborev cost`, `summary`, and `insights` on the CLI.
- **Exports.** `roborev export reviews`, `ci-metrics`, and `ci-costs` emit
  stable, cursor-resumable JSON for your own dashboards.
- **Experiments.** Assign branches to a default or experimental review
  configuration and compare; `roborev config validate` checks an experiment
  before it runs.
- **Events.** `[[hooks]]` run shell commands on review events; built-in kata
  and beads hooks file findings as tracked issues.

## 08 / Ownership: local by construction, shared when you choose

The daemon runs on your machine and writes to SQLite under `~/.roborev`.
Nothing leaves except the calls your agents already make. PostgreSQL sync lets
every machine keep its local database while the team's review history
converges in one place, deduplicated by UUID. Interfaces: CLI, TUI, browser UI,
HTTP API with server-sent events, bundled agent skills. Telemetry is anonymous
daemon counts only and off with `ROBOREV_TELEMETRY_ENABLED=0`.

## 09 / Boundary: the layer before the pull request

Hosted review bots read your code from someone else's cloud, once, when the
pull request opens. Human review is still the gate, and it should be. roborev is
the layer in between: continuous, local, running on the agents you already pay
for, so what reaches the PR has already been reviewed and fixed several times.

## 10 / Start

- macOS / Linux: `curl -fsSL https://roborev.io/install.sh | bash`
- Homebrew: `brew install kenn-io/tap/roborev`
- Windows: `powershell -ExecutionPolicy ByPass -c "irm https://roborev.io/install.ps1 | iex"`

See [all install options](https://roborev.io/docs/installation/).

- [The guide](https://roborev.io/guide.md): ten stops from `roborev init` to
  CI panels and team-wide history.
- [Documentation](https://roborev.io/docs/): every command, configuration key,
  and integration.
- [GitHub](https://github.com/kenn-io/roborev): source, issues, releases. MIT.
