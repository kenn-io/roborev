---
title: Changelog
description: Release history for roborev
---

All notable changes to roborev, grouped by minor release.

## 0.61.0
<small>2026-06-30</small>

**New features**

- Completed review export. `roborev export reviews` emits completed review history as JSON, with `content` and `metadata` profiles plus date, repo, project, closed-only, and limit filters. Panel synthesis reviews export as top-level rows with completed member reviews nested under `subagents`. See [Exporting Reviews](/commands/#exporting-reviews).
- Lookahead reviews. `roborev review --type lookahead` adds a time-series correctness reviewer focused on look-ahead bias, future-data leakage, point-in-time joins, temporal split mistakes, and related peekahead defects. See [Review Types](/guides/reviewing-code/#review-types).
- Factory Droid support for the agent-hook and skill workflows. `roborev agent-hook install --agent droid` installs user-scoped Factory Droid hooks, and roborev now ships Droid-compatible review, fix, refine, respond, design-review, and lookahead skill files, plus their branch variants. See [Agent Hook](/agent-hook/).
- Per-analysis agent configuration. Analysis types can now pin their own agent, model, and reasoning with `[analyze.<type>]`, so `roborev analyze refactor` and similar workflows can use analysis-specific defaults. See [Workflow-Specific Agent and Model](/configuration/#workflow-specific-agent-and-model).
- CI Discord failure notifications. `[ci] discord_webhook_url` enables best-effort Discord webhook alerts for CI review job failures, with sensitive URL masking in config output. See [CI Options Reference](/integrations/github/#ci-options-reference).

**Improvements**

- Post-commit hook request timeouts are now configurable with `hook_timeout_seconds`, with Windows defaulting to 30 seconds and other platforms defaulting to 3 seconds. The hook resolves repo config without spawning git, so large repos and Windows checkouts can raise the bound without adding more hook latency.
- `roborev update` now allows slower release downloads and repairs registered roborev-managed git hooks after an update so managed installs keep pointing hooks at the new binary.
- Documentation now covers `[analyze.<type>]` configuration for fieldless review types such as `lookahead`.

**Bug fixes**

- PostgreSQL sync now sanitizes invalid UTF-8 and NUL bytes in job text fields such as prompts, diffs, and errors before writing to Postgres, preventing binary review data from breaking sync.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for completed review export, lookahead reviews, Factory Droid support, CI Discord notifications, update download resilience, PostgreSQL text hardening, and release/doc maintenance.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for per-analysis agent configuration, configurable post-commit hook timeouts, `[analyze.<type>]` documentation, and release maintenance improvements.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for managed hook repair after roborev updates.

---

## 0.60.0
<small>2026-06-25</small>

**New features**

- `roborev quickstart` now provides guided setup and automation onboarding for repository setup, review tuning, and post-commit review workflows.
- Codex jobs now support explicit `-c` config passthrough overrides through `[agent.codex.config]`, so teams can opt into custom Codex providers without loading the full user config. See [Custom Codex Config](/configuration/#custom-codex-config-model-providers).

**Improvements**

- Onboarding docs now center automation-first workflows, including post-commit reviews, agent hooks, review tuning, and agent-oriented setup guidance.
- Git hook and agent hook installers now prefer managed `roborev` shims when available, keeping generated hook commands stable across version-manager upgrades.

**Bug fixes**

- `roborev refine` now rejects dirty or changed submodule state before applying agent output, avoiding accidental overwrite of local submodule edits.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for `roborev quickstart`, the automation-first docs revamp, and safer submodule handling in `roborev refine`.
- Thanks to [Matt Topol](https://github.com/zeroshade) for Codex config passthrough overrides.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for managed shim preference in hook installation.

---

## 0.59.2
<small>2026-06-24</small>

**New features**

- Copilot review output now prefers structured JSON when the installed CLI supports it, so roborev can store complete assistant findings instead of truncated process output.
- `roborev fix` now includes completed `analyze` jobs when discovering open findings, so queued analysis results can be fixed without passing each job ID explicitly. See [Applying Fixes from Analysis](/guides/assisted-refactoring/#applying-fixes-from-analysis).

**Improvements**

- Homebrew tap updates now open pull requests against `kenn-io/homebrew-tap` instead of pushing directly to the protected tap branch.
- Workflow-configured reviews, fixes, panels, CI members, and synthesis now use only the preferred agent or an explicitly configured backup. See [Backup Agents](/configuration/#backup-agents).
- `roborev analyze` agent invocation is more stable across local setups, including Gemini/Antigravity selection, capability probes from deleted worktrees, and Codex stored-prompt jobs.

**Bug fixes**

- Quota-only or provider-unavailable panel runs no longer store misleading failed synthesis reviews when every member was skipped for availability reasons.

---

## 0.59.1
<small>2026-06-22</small>

**New features**

- Configurable fix commit metadata. Set `fix_commit_author` and `fix_commit_co_authored_by` globally or per repo to control author metadata for `roborev refine` commits, background fix commits applied from the TUI, and prompt instructions for foreground `roborev fix` and `roborev analyze --fix` commits. See [Fix Commit Metadata](/configuration/#fix-commit-metadata).
- Tolerated panel member failures. Mark a panel subagent with `allow_failure = true` when that reviewer is useful but flaky, so a transient failure or cancellation does not fail an otherwise usable panel result. See [Subagents](/advanced/subagent-review-panels/#subagents).

**Improvements**

- Pi job logs now render trace-style message and tool output instead of exposing raw event JSON, making Pi review sessions easier to inspect in CLI and TUI logs.
- Panel parent rows and CI comment footers now surface known member costs even when some panel work is still pending or unpriced, with partial coverage clearly marked where comments include costs.
- Expanded documentation for configuration, GitHub integration, review workflows, auto-fix/refine workflows, and subagent review panels.

---

## 0.59.0
<small>2026-06-22</small>

**New features**

- Repository-hosted documentation. The roborev.io docs now live in this repository, with guides, command reference, integration docs, troubleshooting, and a local Zensical build/check workflow. See [Docs Maintainer Guide](/development/#documentation).
- Global review guidelines. Set `review_guidelines` in `~/.roborev/config.toml` to apply shared reviewer instructions across repositories. Repo-level `review_guidelines` are appended after the global text by default; set `review_guidelines_supersede_global = true` in `.roborev.toml` when a repo should replace the global guidance. See [Review Guidelines](/configuration/#review-guidelines).
- Hook-only auto-design routing. `[auto_design_review] hook_enabled = true` runs the design-review router for post-commit hook reviews without also enabling it for manual reviews or CI. See [Auto Design Review](/configuration/#auto-design-review).

**Improvements**

- `roborev status` is more responsive because it no longer performs slow repository config probes while rendering status.
- CLI startup is faster because terminal capability probing is no longer imported during startup.
- CI review retries keep using the agents configured for the PR instead of being derailed by daemon quota cooldowns.
- Agent quota cooldowns can now be capped with `agent_quota_cooldown`, a global Go-duration config value that defaults to `30m`.
- CI review worktrees are grouped under repo-named parent directories, making daemon clone/worktree storage easier to inspect.
- Security review prompts are stricter about reporting only concrete, exploitable issues and avoiding generic low-signal findings.
- Agent-hook continuation prompts now tell the agent to fix roborev issues and then continue the interrupted task, reducing session derailment.
- Self-update and install release discovery are more resilient when GitHub API rate limits are hit.

**Bug fixes**

- Hidden Windows console windows for daemon, git, and agent child processes.
- Fixed Windows summary filtering behavior.
- Fixed Codex token usage backfill and reporting.
- Fixed `check-agents` availability checks so configured command overrides such as `codex_cmd` and `claude_code_cmd` are honored.
- Improved `roborev fix` discovery so it targets failing reviews and follows hook lineage correctly, including branchless and detached-head flows.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for repository-hosted docs, faster `status`, lighter CLI startup, capped quota cooldowns, CI retry/worktree improvements, self-update resilience, Windows summary and Codex token fixes, and release/doc maintenance.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for global review guidelines, clearer agent-hook continuation prompts, tighter security review prompts, `roborev fix` discovery improvements, and Windows child-process hardening in git and agent paths.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for hook-only auto-design routing.
- Thanks to [Ewan Dawson](https://github.com/EwanDawson) for fixing `check-agents` availability checks with resolved agent commands.
- Thanks to [Christophe Dervieux](https://github.com/cderv) for suppressing extra daemon child-process console windows on Windows.

---

## 0.58
<small>2026-06-11</small>

**New features**

- Kata integration. Local reviews can include task context from a repo's bound Kata project, either from the Kata issues referenced in reviewed commit messages or from the open Kata backlog. Review hooks can also file failed reviews and review findings back into Kata. See [Kata Integration](/configuration/#kata-integration) and [Built-in: Kata Integration](/guides/hooks/#built-in-kata-integration).
- Branch filtering for review hooks. Add `branches = ["main", "release/*"]` to a `[[hooks]]` entry to run it only for matching branches. Local reviews match the job branch; CI PR reviews match the PR base branch so protected-branch workflows can target `main` or release branches. See [Branch Filtering](/guides/hooks/#branch-filtering).
- Queue pause and resume controls. `roborev pause`, `roborev unpause`, and the TUI `P` shortcut pause queue processing without canceling running jobs. Queued jobs remain queued until the queue is resumed. See [Queue Pause](/integrations/tui/#queue-pause).
- Aggregate review cost tracking. `roborev cost` shows approximate all-time or scoped agent spend, and `roborev summary` now includes windowed cost coverage. See [Aggregate Cost](/commands/#aggregate-cost).
- Public daemon Go client. External integrations can import `go.kenn.io/roborev/pkg/client` for a typed client generated from the daemon OpenAPI contract, with raw helpers for streaming/log endpoints. See [Public Go Client](/advanced/streaming/#public-go-client).
- Binary overrides for agent hooks. `roborev agent-hook install --binary <path>` bakes a stable roborev shim or explicit binary path into Codex and Claude Code hook configs, mirroring the git-hook `roborev init --binary` workflow. See [Agent Hook Quick Start](/agent-hook/#quick-start).

**Improvements**

- Preserve user `safe.directory` Git config when running with Git config isolation.
- TUI queue and status displays now surface paused queues with a persistent `[PAUSED]` marker and show approximate aggregate cost for the active filter scope.
- CI panel synthesis now defers and retries on quota or transient synthesis failures instead of posting degraded raw member output.
- Pi classifier setup guidance now explicitly points users at the JSON schema extension install step. See [Pi Structured Output](/agents/#pi-structured-output).

**Bug fixes**

- Fixed Windows daemon restart failures.
- Prevented unwanted Windows console pop-ups when starting daemon and process-management commands.
- Improved Windows daemon/process cleanup behavior.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for queue pause controls, the generated public daemon client, agent-hook binary overrides, preserving user `safe.directory` config, Windows daemon/process fixes, and clearer Pi classifier extension setup guidance.

---

## 0.57.1
<small>2026-06-08</small>

**New features**

- Ship Windows releases as both `.zip` and `.tar.gz` archives.
- Add a daemon route for agentsview token backfill.

**Improvements**

- Improve TUI queue rendering performance.
- Speed up TUI queue startup by reducing repeated display-name lookups.
- Reduce git command overhead on Windows ARM systems.

**Bug fixes**

- Fix PowerShell 5.x install failures on Windows when handling redirects.
- Fix TUI freezes when using multi-repo filters.
- Allow agentsview usage without version-gating.

---

## 0.57
<small>2026-06-05</small>

**New features**

- Agent hook integration. The new `roborev agent-hook` command plugs into Codex and Claude Code harness hooks and prompts the agent to run `$roborev-fix` once review work piles up, so reviews get fixed inside the agent session. See [Agent Hook](/agent-hook/).
- Subagent review panels for CI and manual daemon reviews. A panel fans one review target out to named reviewer specs, then produces one synthesis parent review that is the actionable row for `show`, `list`, `wait`, fix workflows, and the TUI. CI now uses the same panel system; existing `agents`, `review_types`, and `[ci.reviews]` configs become an implicit panel when `[ci] panel` is not set. See [Subagent Review Panels](/advanced/subagent-review-panels/) and [Named CI Panels](/integrations/github/#named-ci-panels).
- Safer CI review retries. The CI poller now tracks retry state per PR HEAD, defers transient provider outages without posting misleading comments, retries genuine member failures up to a bounded cap, and rechecks PR open state, HEAD SHA, and repo identity before retrying or posting. See [Safe CI Retries](/integrations/github/#safe-ci-retries).
- Anonymous daemon telemetry. roborev sends limited anonymous daemon lifecycle telemetry on startup and once every 24 hours while running, with repo and review counts plus high-level feature flags. It does not send repo names, paths, remotes, prompts, review output, provider tokens, usernames, or IP geolocation. Disable with `ROBOREV_TELEMETRY_ENABLED=0` or `TELEMETRY_ENABLED=0`. See [Telemetry](/configuration/#telemetry).
- DEB and RPM release artifacts. Linux releases now include `.deb` and `.rpm` packages for `amd64` and `arm64`, including user-level systemd units. See [Linux Packages](/installation/#linux-packages-deb-and-rpm).

**Improvements**

- TUI review and queue panel refinements, plus a new `i` toggle in log and prompt views that expands or collapses the full command line used for a job. See [Terminal UI](/integrations/tui/#log-view).
- `compact` now requires the consolidated review to repeat each remaining finding with actionable detail (severity, file/line, description). Outputs that report findings only as counts or summaries are rejected instead of stored, so a compacted review cannot claim findings it never lists. Clean verifications are unaffected. See [Consolidating Reviews](/commands/#consolidating-reviews).
- Cost lookup can be routed through a configurable HTTP usage endpoint instead of the local `agentsview` CLI. See [Cost Usage Endpoint](/configuration/#cost-usage-endpoint).
- Update discovery now uses GitHub's HTML redirect endpoint for latest release checks, avoiding GitHub API rate limits.
- Raised the default CI `batch_timeout` from 3 minutes to 15 minutes. Set `batch_timeout = "0"` to disable. See [CI Options Reference](/integrations/github/#ci-options-reference).
- Improved dependency metadata filtering to reduce false-positive findings.
- Default review prompts now put `No issues found.` on its own line for cleaner pass output.
- Improved classifier support, including Pi schema-based classifier output through the configured JSON schema extension. See [Pi Classifier Options](/configuration/#pi-classifier-options).

**Bug fixes**

- Fixed CI reviews using stale checkouts.
- Fixed worktrees finding `.roborev.toml` when it is gitignored in the main repo.
- Fixed review retry log handling and retained classifier job logs.
- Fixed Claude classifier structured output parsing.
- Fixed hook binary resolution for managed installs.
- Fixed the OpenCode install source. See [Supported Agents](/agents/#supported-agents).
- Fixed Copilot streaming behavior when the agent supports disabling streaming.
- Grounded reviewer version checks in the project toolchain.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for anonymous daemon telemetry, kit daemon lifecycle and git helper adoption, hook binary resolution fixes, Pi classifier schema support, Copilot streaming handling, and the OpenCode install source fix.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for Nix vendorHash handling on Dependabot PRs, safer review retry logging, classifier log retention, configurable cost lookup, CI poller test isolation, Claude classifier parsing fixes, the daemon repo resolve endpoint, shared self-update integration, DEB and RPM artifacts, the TUI full-command-line toggle, and reviewer version checks grounded in the project toolchain.
- Thanks to [Chris K Wensel](https://github.com/cwensel) for falling back to the main repo when `.roborev.toml` is gitignored in a worktree.

---

## 0.56
<small>2026-05-24</small>

**New features**

- Per-job cost estimate in the TUI queue. A new default-visible "Cost" column shows the agentsview-provided cost estimate alongside the existing token counts; the review detail header appends `· ~$0.42` to the usage summary. Cost data requires agentsview 0.30.0 or newer (which adds `agentsview session usage <id> --format json` with `cost_usd`/`has_cost` fields); on older versions the column stays blank and token counts are unaffected. Run `roborev backfill-tokens` to refresh existing jobs once agentsview is upgraded. See [Token Usage](/commands/#token-usage).
- Agent plugin manifests for Claude Code and Codex. The repository now ships `.claude-plugin/marketplace.json`, `.claude-plugin/plugin.json`, and `.codex-plugin/plugin.json` pointing at the same Claude and Codex skill trees that `roborev skills install` uses. This lets users install roborev skills through each agent's plugin distribution channel in addition to the existing CLI installer.
- Repo-local oversized-diff snapshots. The default snapshot root moves from OS temp space to `.roborev/` under the repo, set per-repo via a new `snapshot_dir` field in `.roborev.toml`. `roborev init` ensures the configured directory is ignored in `.gitignore`; snapshot creation also writes a local `.git/info/exclude` fallback for repos whose ignore setup is stale. `snapshot_dir` must be a relative path under the repo root and may not be inside `.git`. See [Prompt Size Budget](/configuration/#prompt-size-budget).

**Improvements**

- Prefer the Antigravity `agy` CLI for the Gemini agent. Google has deprecated the legacy `gemini` CLI; roborev now picks `agy` first when both are installed and falls back to `gemini` otherwise. Antigravity runs in `--print` mode with prompts piped over stdin and maps review/agentic permissions to `--sandbox` and `--dangerously-skip-permissions`. Antigravity does not yet accept a `--model` flag, so model overrides automatically reroute to `gemini` if it is installed; when only `agy` is available, an explicit `--model` returns a clear error instead of being silently ignored. See [Supported Agents](/agents/#supported-agents).
- Design review prompt gains an "Internal contradictions" check as the new top-priority item. This flags places where two parts of a spec, PRD, or task list conflict even when each part is individually clear, so downstream agents do not resolve the conflict differently and produce inconsistent implementations. The original five-bullet rubric (completeness, feasibility, task scoping, missing considerations, clarity) is preserved as items 2 through 6.
- CLI errors no longer dump the full usage block on runtime failures. The usage block now prints for invocation errors only (unknown flags, mutually exclusive flags, bad enum values, unknown subcommands, invalid `--server`); runtime errors (daemon down, invalid git ref, network failure) print just `Error: ...`. Caller-controlled exits (`review --wait` with a Fail verdict, `wait` with multiple jobs) remain silent with the correct exit code. `--quiet` continues to silence the verdict output but still surfaces runtime error messages, matching CLI convention.
- The `/roborev-fix` skills for Claude Code and Codex now require closing each addressed review before handling any post-fix auto-reviews, and add explicit per-job `closed=true` audit guidance so the skill cannot leave reviews open after applying fixes.

**Bug fixes**

- Retry backoff is now enforced per job rather than per worker. A new `retry_not_before` column on `review_jobs` (added by a SQLite migration and the postgres v13 schema) stamps the earliest claim time when a job is retried, and `ClaimJob` filters on it across the entire worker pool. The previous in-worker `time.Sleep` only paused the worker that failed; with `--max-workers > 1`, other workers would claim the retry immediately and inherit the same broken state. Failover clears the column so a fresh agent is not held by the prior gate. Default backoff is 2s.
- The CI batch poller no longer unclaims batches that were finalized by a racing event path. Stale-claim recovery now checks the batch's terminal state before reverting it. Permanent GitHub access errors on moved or deleted repositories finalize the batch instead of retrying forever.
- `roborev init` works in worktrees backed by `git clone --bare`. The git helper now detects linked worktrees whose common git directory is bare and resolves them to the checkout root, so Middleman-style bare-backed worktrees can be registered. Behavior for normal linked worktrees and submodule worktrees is unchanged.

**Acknowledgements**

- Thanks to [Lev Konstantinovskiy](https://github.com/tmylk) for adding the internal-contradictions check to the design review prompt.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for preferring Antigravity for Gemini, supporting bare-backed worktrees, adding agent plugin manifests, writing oversized-diff snapshots under repo-local storage, and fixing Codex resume arguments.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for daemon retry backoff via `retry_not_before`, enabling `errorlint`, and improving CLI usage/error output behavior.

---

## 0.55
<small>2026-05-15</small>

**New features**

- `security` analyze type runs `roborev analyze security <files>` with a security-focused prompt covering authn/authz, trust boundaries, injection, file/secret handling, cryptography, and dependency risks. Jobs are tagged `review_type = security` so `security_agent` and `security_model` config (and the per-reasoning-level variants) apply automatically. See [Code Analysis](/commands/#code-analysis).
- `roborev status --json` emits structured daemon and queue status, including the active daemon endpoint as `network`, `address`, and `port` fields so scripts and integrations can discover the listening transport without reading runtime files.

**Improvements**

- Review templates for Claude Code, Codex, and Gemini drop the "diff and static analysis only" framing and explicitly permit reading other repo files to verify claims. Agents are still prohibited from building, running tests, or executing code; the change reduces false-negative findings (e.g. "this package doesn't exist") on PRs that reference code outside the diff. Non-templated agents (Copilot, OpenCode, Cursor, Kiro, Kilo, Droid, Pi) use the fallback prompt and are unchanged.
- The TUI queue hides auto-design-router classifier jobs (`job_type = classify`) and skipped design rows (`status = skipped`, scoped to `source = auto_design`) by default to reduce per-commit routing noise. A new `show_classify_jobs` global config (with nullable per-repo override) and an `s` TUI hotkey toggle visibility; the queue footer and `?` help screen reflect the current state. Pressing `l` on a hidden classifier or skipped row shows the classifier verdict and `skip_reason` above the streamed log. The daemon status endpoint's skipped/triggered counters are unchanged. See [Auto Design Review](/configuration/#auto-design-review).
- Codex review jobs default to `skills.include_instructions=false` and `--ignore-user-config`, so review prompts run without skill instructions or user-level Codex config. Two global toggles under `[agent.codex]` control this (`disable_review_skills`, `ignore_review_user_config`); both default to `true`. Fix jobs are not affected. See [Codex Review Options](/configuration/#codex-review-options).

**Bug fixes**

- Gemini-based reviews can now read external diff snapshots. The Gemini agent receives the per-snapshot temp directory in its allowed-paths list (matching the Codex `--add-dir` behavior introduced in 0.54), restoring access to the expected context for large diffs.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for disabling Codex review skill context by default, adding security analyze mode, and exposing the daemon endpoint in JSON status output.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for allowing review agents to read repo files when verifying diffs, exposing Gemini diff snapshot directories, and hiding auto-design-router noise from the queue by default.

---

## 0.54
<small>2026-05-07</small>

**New features**

- `--batch-size N` flag on `roborev fix` to pack up to N reviews into a single agent invocation, still bounded by the configured `max_prompt_size`. This sits between the default one-fix-per-prompt mode and `--batch` (everything in one prompt): you get coordinated multi-finding fixes per call without exceeding the prompt budget. Mutually exclusive with `--batch` and `--list`. See [Fixing Reviews](/commands/#fixing-reviews).
- `--resume` flag on `roborev fix` to reuse the agent's session ID across calls within a single fix run, so chained fixes build on prior context instead of starting fresh each call. Defaults to off.

**Improvements**

- Large diff prompts are kept in external snapshot files instead of being expanded back into agent prompts. roborev now applies an agent-agnostic final prompt-size gate before submission and treats context-window failures as non-retryable failover candidates rather than retrying the same oversized prompt. Snapshots are written to per-snapshot temp directories that are readable by all agents.
- Review prompts now instruct agents to separate multiple findings with `---` on its own line so findings render as distinct entries instead of running together. Applies across the default, dirty, range, and security templates as well as the Claude Code, Codex, and Gemini variants.
- The README header logo uses a dark-background variant on GitHub dark mode (via a `<picture>` element with `prefers-color-scheme`), so the dark-text logo no longer becomes unreadable on dark backgrounds.

**Bug fixes**

- The foreground `roborev fix` loop now classifies agent errors and aborts cleanly when it detects a quota or session limit, instead of demoting these failures to per-job warnings during discovery mode. A new `internal/agent` rate-limit classifier (`LimitKind`, `LimitClassification`, `ParseResetDuration`, `ParseResetTime`) is shared between the daemon worker and the CLI fix loop, so cooldown behavior is consistent across both paths and unmatched agent errors are logged with a truncated preview instead of being silent.
- Codex large-diff snapshots are written to per-snapshot subdirectories, and Codex receives the snapshot directory via `--add-dir` instead of full `/tmp` access. This restores Codex's ability to read external diff snapshots while avoiding exposure of unrelated `/tmp` contents. Snapshot-shaped paths in prompts are ignored unless they resolve to existing files inside a private roborev snapshot directory.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for exposing Codex diff snapshot directories and keeping large diff prompts external.

---

## 0.53.1
<small>2026-05-04</small>

**Improvements**

- ACP session validation: the first ACP session update is validated against the agent's configured session ID before being stored. Mismatches are logged but no longer return errors, avoiding connection breaks during normal operation.
- ACP model resolution: `ModelForSelectedAgent` falls back to the model configured in the `[acp]` section when no workflow-specific model is set, giving consistent model selection across review, analyze, fix, refine, and daemon paths.
- Clearer error messages for invalid ACP configuration and model settings, with improved permission handling during session setup.

**Bug fixes**

- `review --branch`, `analyze --branch`, and `refine` correctly auto-detect the branch base when the upstream is configured as a raw URL (e.g. `https://example.com/fork.git`) rather than a registered remote name. URL-shaped values previously produced invalid refs like `refs/remotes/https://.../main` that failed to resolve, breaking branch-base detection for affected repos. roborev now detects URL-shaped remote values via prefix and `://` checks, after first confirming the value is not a registered remote name, and falls through to the next detection step.
- Per-branch `branch.<name>.base` overrides are now consulted by review, refine, and analyze flows. The override was already documented as a per-branch base override but was being skipped in favor of upstream-tracking detection.

**Acknowledgements**

- Thanks to [Veit Sanner](https://github.com/VeitSanner) for improving ACP session validation and model resolution.

---

## 0.53
<small>2026-04-30</small>

**New features**

- Opt-in automatic design-review router. roborev can now decide per commit whether to dispatch a `--type design` review, using cheap heuristics first (path globs, diff size, file count, commit-subject regexes) and a schema-constrained classifier as a fallback for ambiguous cases. Off by default; turn it on globally with `[auto_design_review] enabled = true` in `~/.roborev/config.toml` or per repo in `.roborev.toml`. The post-commit, `roborev review`, range, and dirty paths and the CI poller all consult the router; design jobs are emitted only when the router says yes, otherwise a skipped row is recorded with the deciding reason and rendered dimmed in the TUI. Classifier behavior is tunable via `classify_agent`, `classify_model`, `classify_reasoning`, `classify_backup_agent`, and `classify_backup_model`. The classifier requires a `SchemaAgent`-capable backend (Claude Code, Codex); other agents are rejected at config-resolve time. See [Auto Design Review](/configuration/#auto-design-review).

**Improvements**

- Daemon HTTP API consolidated under [Huma](https://huma.rocks/). Routes, request and response types, and handlers move to a single Huma-backed registration in `internal/daemon/routes.go`, and the OpenAPI spec served at `/openapi.json` is now available in both 3.1 (default) and 3.0 (downgraded) flavors. The TUI talks to the daemon through a generated OpenAPI client (`internal/daemon_client`) for normal JSON calls; streaming endpoints continue to use plain handlers. This is internal plumbing; existing CLI and TUI behavior is unchanged.
- The CI poller runs the auto design-review router on the PR head SHA when `design` is not already in the configured review matrix. Heuristic-input failures (missing diff, changed-files, or commit message) degrade to the classifier instead of being silently skipped, so misconfigured repos surface a real outcome.
- Shell completion for `roborev review --type` suggests `security` and `design`, matching the existing pattern for `--agent` and `--reasoning`. Tab-complete the value directly without typing it out.
- New `roborev repo move <name-or-path> <new-path>` subcommand updates a tracked repository's stored root path after a directory rename or move on disk, so existing jobs and reviews stay attached to the same repo entry. See [Repository Management](/guides/repository-management/#moving-or-renaming-a-repository).

**Bug fixes**

- `roborev fix` now discovers reviews when run from a detached HEAD. Open-job filtering walks the commit chain back to the first reachable branch tip and matches jobs against the detached commits and review ranges that end on them. `dirty` jobs and unrelated refs remain excluded.
- The TUI's `auto_filter_repo` startup filter now reconciles renamed and moved repos through their stored display name and identity, so renamed repositories still surface their existing reviews instead of appearing empty.
- `review --branch`, `analyze --branch`, `refine`, and the post-commit branch-review path now resolve the merge-base against the current branch's `@{upstream}` (e.g. `upstream/main` in fork workflows), falling back to the configured default branch only when no upstream is set. The previous behavior pulled in commits already merged upstream when the local `origin/main` was behind. The `currentBranch == LocalBranchName(base)` guardrail is replaced by `git.IsOnBaseBranch`, which generalizes the `origin/` shortcut, handles non-origin remotes, and stops misclassifying local branches whose names contain slashes (e.g. `feature/foo`).
- Tab-completing `roborev review --type <TAB>` no longer falls through to filename completion when no value has been typed yet. The completion now returns just `security` and `design` and disables filename fallback.
- Claude Code's durable scheduled-task files (`.claude/scheduled_tasks.json`, `.claude/scheduled_tasks.lock`) are added to `.gitignore` so the harness's local cron state does not get accidentally tracked.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for fixing detached-HEAD review discovery, migrating the daemon API to Huma, and completing review type flag support.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for ignoring Claude Code scheduled task files, adding the automatic design-review router, and fixing branch upstream resolution for branch reviews.

---

## 0.52
<small>2026-04-19</small>

**New features**

- Route Claude Code through an OpenAI- or Anthropic-compatible proxy (Ollama, LiteLLM, LM Studio, etc.) via a `<model>@<base_url>` model spec. When a proxy URL is present, roborev pins all Claude Code tier aliases (Opus/Sonnet/Haiku/subagent) to the specified model and points the agent at the given endpoint, making it possible to use local models for reviews and fixes. See [Routing Claude Code to a proxy](/agents/#routing-claude-code-to-a-proxy).

**Improvements**

- Review prompts are more consistent across agents, with reduced low-value noise in review output.
- Streamed tool-call names and input fields are normalized across agents for cleaner agent output in the TUI and daemon logs.
- OpenCode output shows tool calls and drops migration noise from `stderr`.

**Bug fixes**

- TUI clipboard copy works over SSH by falling back to OSC52 escape sequences when a local clipboard is unavailable.
- `j`, `k`, and `q` can be typed normally while editing TUI filter searches, instead of being captured as navigation/quit shortcuts.
- Gemini severity threshold parsing no longer fails when marker strings include internal whitespace.

!!! warning
    Breaking change: when the `claude-code` agent runs, roborev now strips inherited `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL`, and `CLAUDE_CODE_SUBAGENT_MODEL` from the child environment. If you were routing Claude Code by exporting these variables in your shell, switch to the `<model>@<base_url>` spec or configure `anthropic_api_key` in `~/.roborev/config.toml`.

**Acknowledgements**

- Thanks to [Luis Gonzalez](https://github.com/lgonzalezsa) for clipboard support over SSH with OSC52.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for hardening review prompts and refactoring prompt construction around shared templates and golden snapshots.
- Thanks to [graycoldknight](https://github.com/graycoldknight) for allowing internal whitespace in Gemini severity threshold markers.
- Thanks to [Chris K Wensel](https://github.com/cwensel) for routing Claude Code through a proxy with the `<model>@<base_url>` model spec.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for stream formatting cleanup and improved OpenCode tool-call output.

---

## 0.51
<small>2026-04-08</small>

**New features**

- OpenAPI spec for the daemon REST API, served at `/openapi.json` (OpenAPI 3.1.0 via Huma). The spec covers the primary query and mutation endpoints used by integrations (jobs, reviews, comments, repos, branches, status, summary, cancel, rerun, close). Internal endpoints used by the CLI, TUI, and daemon subsystems (enqueue, streaming, sync, fix orchestration) are not part of the OpenAPI surface. See [Streaming & Daemon API](/advanced/streaming/#daemon-api).
- Cascading `review_min_severity` setting and `--min-severity` flag on `roborev review` to filter review findings by severity. The setting cascades from CLI flag to per-repo `.roborev.toml` to global `config.toml`, matching the existing pattern for `fix_min_severity` and `refine_min_severity`. Global defaults for all three (`review_min_severity`, `fix_min_severity`, `refine_min_severity`) are now supported in global config. See [Configuration](/configuration/#per-repository-options).

**Improvements**

- Branch review prompts include per-commit review context. When reviewing a commit range, the prompt includes summaries and verdicts from individual per-commit reviews, with instructions to focus on cross-commit interactions instead of re-raising known issues.
- Fix prompts include user comments and prior tool attempts. Developer comments and previous automated fix attempts are separated and included in the prompt, giving the fix agent more context about what has already been tried and what the developer flagged.
- Global reasoning defaults are honored consistently across review, fix, refine, and related workflows. Resolution order: explicit CLI flag > per-repo config > global config > default.
- The TUI lets you inspect the full prompt while a job is still queued, before it starts running. Press `p` on any queued job that has a stored prompt (task, fix, compact, insights).

**Bug fixes**

- `roborev fix` correctly discovers open jobs on the current branch. Previously, it could include jobs from unrelated branches or miss jobs when run from `main` due to unreachable SHAs from squashed or amended commits.
- Codex sandbox compatibility improvements. A new `disable_codex_sandbox` config option bypasses `bwrap` sandboxing on systems where it is unavailable. Read-only sandboxed reviews fall back to inline diff snapshots when `.git/` is inaccessible to the agent. See [Configuration](/configuration/#global-options).
- Codex review jobs now store and display the actual command line used, fixing incorrect command reporting in the TUI.
- CI repo matching resolves ambiguous repositories (multiple repos sharing the same git identity) by preferring auto-cloned repos instead of failing.
- GitHub Actions release checksums use the expected `SHA256SUMS` filename.

**Acknowledgements**

- Thanks to [Phillip Cloud](https://github.com/cpcloud) for min-severity cascading, review-level filtering, config/worktree/CI reasoning fixes, and global reasoning default handling.
- Thanks to [Stephan Hoyer](https://github.com/shoyer) for including per-commit reviews in branch review prompts and adding user comments and tool attempts to fix prompts.
- Thanks to [Ben Sedat](https://github.com/bsedat) for switching GitHub Action artifacts to `SHA256SUMS`.
- Thanks to [Axon](https://github.com/axonstone) for resolving ambiguous repository matches in CI.

---

## 0.50
<small>2026-03-31</small>

**New features**

- `auto_close_passing_reviews` config option to automatically close reviews that pass with no findings. When enabled, pass reviews are closed immediately instead of staying open in the queue. See [Configuration](/configuration/#auto-close-passing-reviews).
- Bundled `roborev-refine` skills for Claude Code and Codex to run iterative review-fix-review loops from within an agent session. The skill performs the full refine workflow inline (review, fix, commit, re-review) rather than shelling out to the CLI. See [Agent Skills](/guides/agent-skills/#refine-a-branch).
- Bundled systemd service and socket unit files for Linux daemon deployments. The service uses `Type=notify` for readiness signaling. Socket activation is supported for on-demand daemon startup. See [Persistent Daemon](/configuration/#persistent-daemon).

**Improvements**

- The TUI updates instantly by subscribing to the daemon event stream (SSE) instead of polling on a timer. Polling is retained as a 15-second fallback.
- CI review prompts now include human PR discussion (issue comments, review summaries, and inline review comments) from trusted collaborators with maintain or admin access. Discussion is treated as untrusted context with safety guardrails.
- The daemon socket path prefers `$XDG_RUNTIME_DIR/roborev/daemon.sock` when the variable is set and points to an existing absolute directory. Falls back to the platform temp directory otherwise. See [Configuration](/configuration/#unix-domain-socket).

**Bug fixes**

- Preserve the requested model when rerunning reviews. Previously, rerunning a review could resolve a different model from config defaults instead of preserving the model specified in the original request. A separate `requested_model` field now tracks explicit user intent.
- Enforce a `batch_timeout` (default: 3 minutes) on CI PR comment batches to prevent indefinite hangs when some jobs in a multi-agent batch get stuck. When the timeout expires, available results are posted and remaining jobs are canceled. See [CI Options Reference](/integrations/github/#ci-options-reference).

**Acknowledgements**

- Thanks to [Aaron Jacobs](https://github.com/atheriel) for downstream systemd unit files, `$XDG_RUNTIME_DIR` daemon socket handling, and systemd socket activation support.
- Thanks to [Stephan Hoyer](https://github.com/shoyer) for adding the iterative `roborev-refine` skill.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for adding human PR discussion to review prompts, preserving TUI selections and model provenance, and refactoring shared daemon polling, workflow resolution, clone, runtime argument, repo-root, HTTP loader, and config precedence helpers.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for preventing stale TUI status/fix-job updates, adding `auto_close_passing_reviews`, and subscribing the TUI to the daemon event stream.

---

## 0.49
<small>2026-03-24</small>

**New features**

- `roborev insights` command analyzes failing code reviews to identify recurring patterns, hotspot areas, noise candidates, and guideline gaps. Outputs actionable suggestions for improving `review_guidelines` in `.roborev.toml`. Runs as a daemon-backed job, queued and tracked like reviews. See [Commands](/commands/#insights).
- Unix domain socket support for CLI-to-daemon communication on Unix systems. Set `server_addr = "unix://"` in `~/.roborev/config.toml` to listen on `/tmp/roborev-{UID}/daemon.sock` instead of TCP loopback. Socket permissions (`0600`) enforce per-user access control. See [Configuration](/configuration/#unix-domain-socket).
- `ROBOREV_COLOR_MODE` environment variable to force `auto`, `dark`, `light`, or `none` color output across all TUI screens and CLI rendering. See [Configuration](/configuration/#color-mode).

**Improvements**

- Skill installation and status reporting use a shared multi-agent catalog. `roborev skills` now shows per-agent status (installed, outdated, not installed, no agent) for both Claude Code and Codex. Adding future agents requires a single catalog entry.
- Large Codex reviews are more reliable. Prompt budgeting is now configurable via `max_prompt_size` (per repo) and `default_max_prompt_size` (global), with smart fallback instructions that guide Codex to read diffs locally when they exceed the prompt budget. Diffs are read in bounded chunks with UTF-8-safe truncation.
- Pre-commit auto-fixes and lint hook management now use [prek](https://prek.j178.dev/) instead of a custom shell script (roborev development workflow only).

**Bug fixes**

- `NO_COLOR` is honored on TUI review and prompt detail screens. Previously, glamour markdown rendering defaulted to TrueColor regardless of `NO_COLOR`.
- `roborev refine` branch reviews now use the configured review agent instead of the fix agent.
- Reviews and hooks for commits made in git worktrees now run in the correct worktree directory. A `worktree_path` field is persisted per job so agents and hooks operate on the right branch.
- Copilot reviews no longer fail with permission denials in non-interactive (daemon) mode. The agent now uses `--allow-all-tools` with a deny-list for destructive operations in review mode.

**Acknowledgements**

- Thanks to [Sergey Trofimovsky](https://github.com/strofimovsky) for fixing `NO_COLOR` on TUI detail screens and adding `ROBOREV_COLOR_MODE`.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for making insights a daemon-owned job type, refactoring the shared multi-agent catalog and ACP/CLI runner flows, adopting `prek` for lint hooks, and cleaning up storage, verdict parsing, testenv, stream formatting, update, test helper, and version build-info internals.
- Thanks to [Ryan Mahoney](https://github.com/ryan-mahoney) for fixing review-agent and hook working directories for worktree commits.
- Thanks to [Thomas Maloney](https://github.com/tlmaloney) for fixing refine branch reviews that used the wrong agent.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for adding Unix domain socket daemon transport.

---

## 0.48
<small>2026-03-18</small>

**Improvements**

- Review agents now run in a read-only sandbox. Codex review jobs use `--sandbox read-only` instead of `--full-auto`, matching Claude Code's existing read-only tool restrictions. Agentic mode (fix, refine, `--agentic`) is unchanged. All agent subprocesses set `GIT_OPTIONAL_LOCKS=0` to avoid contending with the user's own git operations.
- `--open` and `--unaddressed` flags on `roborev fix` are deprecated. Open job discovery is now the default behavior when no positional job IDs are provided. The flags are hidden and silently ignored for backwards compatibility.
- `--branch <name>` flag added to `roborev fix` for cross-branch fixing without switching branches.
- Skip update notifications in development builds.

**Bug fixes**

- Avoid `.git/index.lock` contention during reviews by setting `GIT_OPTIONAL_LOCKS=0` in agent subprocess environments, reducing conflicts with concurrent git operations.
- Fix `--all-branches` and `--branch` filtering when running `roborev fix` from a git worktree. The branch override was not being threaded through to `filterReachableJobs`, causing it to filter by the worktree's branch instead of the requested branch.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for refactoring worktree helper flows.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for skipping update notifications on dev builds and speeding up the test suite.

---

## 0.47
<small>2026-03-17</small>

**New features**

- `roborev summary` command shows aggregate review statistics: pass/fail trends, per-agent effectiveness, review duration percentiles, fix resolution rates, and per-repo breakdowns. Scopes to the current repo by default; use `--all` for cross-repo summary. Supports `--since`, `--branch`, `--repo`, and `--json` flags. See [Commands](/commands/#review-statistics).
- TUI control socket for programmatic interaction with running TUI instances. External tools can query state and trigger mutations (filter, select, close, cancel, rerun, quit) over a Unix domain socket using a newline-delimited JSON protocol. Runtime metadata is written to `~/.roborev/tui.{PID}.json` for discoverability. See [TUI Control Socket](/integrations/tui/#control-socket).
- `--no-quit` flag on `roborev tui` suppresses keyboard quit (`q`) in queue and tasks views, allowing external controllers to manage the TUI lifecycle. The `quit` control command still works regardless.
- Token usage tracking: agent token consumption (peak context tokens and total output tokens) is automatically recorded after each job completes and displayed in the TUI review header and `roborev show` output. Requires `agentsview` to be installed. `roborev backfill-tokens` retroactively fetches token data for completed jobs that have session IDs but no stored usage.
- `opencode_cmd` config key to override the OpenCode executable path, matching the existing pattern for other `*_cmd` overrides.

**Improvements**

- Common lockfiles and generated files (package-lock.json, yarn.lock, go.sum, Cargo.lock, uv.lock, and others) are excluded from review diffs by default. Add custom patterns via `exclude_patterns` in global or per-repo config. Security reviews skip repo-level exclude patterns to prevent suppression of sensitive files. See [Configuration](/configuration/#exclude-patterns).
- `maximum` reasoning level (aliases: `max`, `xhigh`) maps to Codex's xhigh reasoning effort. For agents without an xhigh equivalent, it maps to thorough. See [Reasoning Levels](/configuration/#reasoning-levels).
- Session ID column in the TUI queue view.
- Column checkboxes in the TUI options menu respond to mouse clicks.
- Long comment text word-wraps in the TUI review pane.
- The TUI elapsed timer updates every second instead of only on data refreshes.
- Skill names switched from colon syntax (`roborev:fix`) to hyphenated syntax (`roborev-fix`) for compatibility with GitHub Copilot CLI. Run `roborev skills update` to apply the new names. Both Claude Code and Codex skills are updated.

**Bug fixes**

- Fix `GetCurrentBranch` returning a `heads/`-prefixed branch name when git refs are ambiguous.
- Update Gemini defaults and fall back cleanly when the configured model is unavailable.

**Acknowledgements**

- Thanks to [Phillip Cloud](https://github.com/cpcloud) for isolating tests from global git config, fixing a session-stream test agent leak, adding the TUI elapsed-time tick, and ignoring `.claude/worktrees`.
- Thanks to [Sergey Trofimovsky](https://github.com/strofimovsky) for adding the `opencode_cmd` executable override.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for issue 478 refactors, maximum Codex reasoning support, TUI session-id and column-option improvements, and Copilot-compatible skill names.

---

## 0.46
<small>2026-03-11</small>

**Improvements**

- Agent availability checks now honor `*_cmd` config overrides (`claude_code_cmd`, `codex_cmd`, `cursor_cmd`, `pi_cmd`). Previously, custom agent commands were ignored during availability detection, so an agent could appear unavailable even when the configured binary was in PATH. See [Agent Command Overrides](/configuration/#agent-command-overrides).
- The TUI review screen now displays the branch name stored with the review instead of resolving it dynamically via `git name-rev`. In worktree setups where the same SHA is reachable from multiple refs, the old behavior could display the wrong branch.

**Bug fixes**

- Fix the post-commit hook sending the worktree path instead of the main repository root to the daemon when running inside a linked git worktree. This caused commits to be registered under a phantom repo entry.
- Fix `roborev fix --open`, `--list`, and `--batch` discovering reviews from other worktrees. Jobs are now filtered to only those reachable from the current worktree's HEAD or matching its branch name.
- Fix the post-commit hook not firing in linked git worktrees when `core.hooksPath` is set to a relative path. Relative paths are now resolved against the main repository root instead of the worktree root. `init`, `install-hook`, and `uninstall-hook` also normalize the hooks path and fail early if it cannot be resolved.
- Add JSONL post-commit hook logging to `~/.roborev/post-commit.log` so that silent hook failures leave an audit trail with timestamps, repo paths, and failure reasons. See [Troubleshooting](/guides/troubleshooting/#post-commit-hook-log).

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for fixing post-commit hooks in git worktrees and migrating tests to testify.

---

## 0.45
<small>2026-03-08</small>

**New features**

- `--min-severity` flag for `roborev fix` and `roborev refine` to limit fixes to findings at or above a chosen severity (`low`, `medium`, `high`, `critical`). Also configurable per repo via `fix_min_severity` and `refine_min_severity` in `.roborev.toml`. When all findings in a review fall below the threshold, `refine` automatically closes the review instead of treating it as a fix failure.
- Experimental: `reuse_review_session` config option (global and per repo) to resume prior agent sessions on the same branch, reducing token usage and review latency on active branches. See [Session Reuse](/guides/reviewing-code/#session-reuse).

**Improvements**

- `roborev show` now displays comments after the review output, matching the TUI review detail view.
- Copied reviews (TUI `y` key) now include review comments, giving the fix agent more context when you paste into an agent session.
- Agent tool-call narration (text the agent emits before tool calls) is stripped from persisted review output across all agents.
- Daemon status details are hidden from the review detail view in the TUI; the queue view is unchanged.
- Review prompts now instruct agents not to build projects, run tests, or execute code during review.
- `roborev config set` and `roborev init` now produce commented TOML output with inline descriptions for each field.

**Bug fixes**

- Fix `roborev compact` using the wrong branch inside git worktrees. It was resolving the main checkout's branch instead of the worktree's branch.
- Fix workflow model fallback so it uses the selected agent's actual default model instead of the global default.
- Job log files are no longer permanently lost when the initial file open fails under resource pressure. The new log writer retries and buffers output until disk logging recovers.
- Timed-out review jobs now unwind reliably instead of appearing to run past the configured `job_timeout`. Timeout errors are recorded as `agent timeout after <duration>` for clearer reporting in the TUI and hooks.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for documenting auto-written TOML config files and safely reusing review sessions on the current branch.

---

## 0.44
<small>2026-03-07</small>

**New features**

- `mouse_enabled` global config flag (and TUI options menu toggle) to disable mouse interactions in the TUI.
- `roborev post-commit` command and `post_commit_review` repo config to control post-commit review behavior, including branch review workflows.
- Webhook review hooks (`type = "webhook"`) for external integrations.
- `excluded_commit_patterns` repo config for skipping reviews based on commit message substrings.
- `auto_filter_branch` global config to automatically filter the TUI to the current branch or worktree on startup.

**Improvements**

- Tighter review prompts and more consistent verdict parsing.
- Daemon/client mismatch warnings surfaced through daemon status output.

**Bug fixes**

- Retry `fix` daemon calls automatically after a daemon restart.
- Fix daemon startup restart loops.
- Fix Enter key handling in the TUI inside embedded terminals.
- Fix cases where the TUI fails to close cleanly.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the mouse-disable config flag, webhook review hook support, and job session ID capture.
- Thanks to [Darren Haas](https://github.com/darrenhaas) for adding the post-commit command and branch review hook configuration.

---

## 0.43
<small>2026-03-06</small>

**New features**

- `default_backup_model` config option to control the fallback model used by agent workflows when the primary model is unavailable.
- `advanced.tasks_enabled` config flag to opt in to the TUI background tasks workflow (fix jobs, patch application, and rebasing). This workflow was previously enabled by default and has been moved behind a flag to avoid confusion about the primary review workflow.

**Improvements**

- `Ctrl-D` quits the TUI as an additional shortcut alongside `q`.
- Improved built-in agent skill definitions for more reliable matching, and expanded agent configuration documentation.

**Bug fixes**

- Agent resolution for `review`, `analyze`, `fix`, and `refine` commands now selects the intended agent more reliably.
- CLI `--agent` overrides no longer inherit the wrong `default_model` from configuration.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for Ctrl-D TUI quit handling and gating the advanced TUI tasks workflow behind configuration.

---

## 0.42
<small>2026-03-05</small>

**New features**

- Multi-repo workspace support: `roborev list` looks in immediate child subfolders for repos, and `roborev review` suggests repo-level review commands.
- Cursor agent support.
- Pi coding agent support.
- Save generated patch files to disk from the TUI Tasks view.

**Improvements**

- Skip review throttling when a new push supersedes an in-progress review. The old review is canceled and the new one starts immediately.
- Validate configured agent names and reject unknown agents earlier.

**Bug fixes**

- Improve Claude review failure reporting so agent errors are captured and surfaced correctly.

**Acknowledgements**

- Thanks to [Miki Tebeka](https://github.com/tebeka) for saving patch files from the TUI.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for adding Pi coding agent support.
- Thanks to [Darren Haas](https://github.com/darrenhaas) for multi-repo workspace support.

---

## 0.41
<small>2026-03-04</small>

**Bug fixes**

- Restore a separate `P/F` verdict column in the TUI queue so review outcomes are easier to scan.

---

## 0.40
<small>2026-03-03</small>

**New features**

- ACP (Agent Client Protocol) support: run reviews through any ACP-compatible agent via the `[acp]` config section.
- Kiro agent integration via `kiro-cli`.
- Configurable PR comment upsert: update existing roborev PR comments instead of posting duplicates (`ci.upsert_comments`).

**Improvements**

- Renamed review status terminology to `closed`/`open` across CLI, TUI, and API. `roborev close` and `roborev fix --open` replace the legacy command/flag aliases (which are still accepted).
- Combined the separate Status and P/F columns in the TUI queue into a single Status column with color-coded states (Queued, Running, Pass, Fail, Error, Canceled).
- Column customization in the TUI: press `o` to reorder or toggle column visibility. New `column_borders`, `column_order`, and `task_column_order` config options.
- Mouse copy/paste in TUI content views; long stderr lines wrap in log views.
- Visual polish across TUI queue, review, and task screens: tighter column spacing, box-drawing separators, right-aligned elapsed column.
- Deprecated the `/roborev:address` skill in favor of `/roborev:fix`.

**Bug fixes**

- Fixed UTF-8 truncation when composing PR comments.
- Fixed command/footer parsing by trimming trailing blank lines and enforcing `--` separators.

**Acknowledgements**

- Thanks to [Danny Steenman](https://github.com/dannysteenman) for configurable PR comment upserts and UTF-8 truncation fixes.
- Thanks to [Veit Sanner](https://github.com/VeitSanner) for Agent Client Protocol support.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for TUI mouse copy/paste support, log stderr word wrapping, and spacing polish.

---

## 0.39
<small>2026-02-28</small>

**New features**

- Compact mode for better usability on short terminal windows.
- Distraction-free toggle in the TUI for a cleaner review experience.

**Bug fixes**

- Custom fix instructions now include full review context during fix generation.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for compact mode on short terminals, the distraction-free toggle, and review context for custom fix instructions.

---

## 0.38
<small>2026-02-26</small>

**New features**

- Kilo agent support via the `kilo` CLI.
- `roborev wait` accepts multiple job IDs in a single command.

**Improvements**

- TUI task view supports mouse interactions (click to select, double-click to view).
- `roborev update` manages the daemon lifecycle for smoother upgrades.

**Bug fixes**

- Use `ANTHROPIC_API_KEY` for the OpenCode agent in GitHub Actions workflows.
- `roborev fix` skips reviews that already have a `PASS` verdict.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for multiple job IDs in `roborev wait`, mouse improvements for TUI tasks, and Kilo agent support.

---

## 0.37
<small>2026-02-25</small>

**Improvements**

- TUI help bar restyled with two-tone key hints and aligned columns for easier shortcut scanning.
- Unified stream output formatting across CLI and TUI views for more consistent display.

**Bug fixes**

- Show the correct `roborev` version when installed via `go install`.

**Acknowledgements**

- Thanks to [Miki Tebeka](https://github.com/tebeka) for reporting the correct version when roborev is installed with `go install`.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for restyling the TUI help bar with aligned two-tone keys.

---

## 0.36
<small>2026-02-24</small>

**New features**

- `roborev tui --repo` and `--branch` flags to launch the TUI pre-filtered to a specific repository or branch. Without a value, each flag resolves to the current repo/branch. With `=` syntax (e.g. `--repo=/path/to/repo`, `--branch=feature-x`), the value is used directly. When set via flags, the filter is locked and cannot be changed in the TUI.
- Inline fix panel in the TUI review view: press `F` while viewing a review to open a fix prompt at the bottom of the screen instead of a full-screen modal. `Tab` toggles focus between the review content and the fix input. `Enter` submits, `Esc` cancels.
- Shell completions for `--agent` and `--reasoning` flags across all commands that accept them (`init`, `review`, `run`, `fix`, `analyze`, `refine`).
- OpenCode JSON stream support: the OpenCode agent now uses `--format json` for structured JSONL output, integrated into the unified stream formatter for consistent progress rendering.
- CI repository matching with wildcard patterns and exclusion lists. `ci.repos` entries now support glob patterns (e.g. `"myorg/*"`, `"myorg/api-*"`) using `path.Match` syntax. New `exclude_repos` field filters out matching repos, and `max_repos` (default: 100) caps the total expanded count. Wildcard results are cached for one hour.

**Improvements**

- TUI help bar uses table-based rendering for consistent column alignment across all views.

**Bug fixes**

- `--all-branches` now implies `--open` on `roborev fix` and `roborev refine`, removing the need to pass both flags.
- Patch application in git worktrees resolves the correct worktree path via `git worktree list`, fixing failures when the branch is checked out in a non-default worktree location.
- Temporary command execution uses explicit file sync and retry with exponential backoff to prevent intermittent `text file busy` (ETXTBSY) races on Linux.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for TUI repo/branch flags, help bar alignment, temp-command race fixes, shell completions, worktree patch-path handling, and the inline fix panel.
- Thanks to [Danny Steenman](https://github.com/dannysteenman) for CI repo wildcard patterns and exclusion lists.

---

## 0.35
<small>2026-02-23</small>

**New features**

- Shell completion for `roborev analyze` command types: tab-complete analysis type names (e.g. `roborev analyze <TAB>` suggests `refactor`, `complexity`, etc.).
- Persistent job logs: agent output is written to `~/.roborev/logs/jobs/` as NDJSON so review activity survives daemon restarts.
- Unified log viewer: `roborev log <job-id>` renders stored job output on the CLI, and pressing `l` in the TUI opens a scrollable log viewer with live polling for running jobs. `roborev log clean` removes old log files.

**Improvements**

- Test and production runtime data are isolated so `go test` runs do not pollute `~/.roborev/` logs or interfere with the production daemon.
- CLI and TUI streaming output uses gutter-grouped tool calls, markdown text wrapping, and Codex reasoning item rendering for clearer review progress.

**Bug fixes**

- Handle empty Git refs when fixing compact review jobs to prevent fix-flow failures. The server resolves a usable ref from the parent job's branch or falls back to HEAD, and the TUI shows a confirmation modal when no ref is available.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for analyze command shell completions and compact-review fix handling for empty git refs.

---

## 0.34
<small>2026-02-22</small>

**New features**

- `roborev ci review`: daemon-free batch reviews for CI pipelines with auto-detection of GitHub Actions environment variables (`GITHUB_REPOSITORY`, `GITHUB_REF`, `GITHUB_EVENT_PATH`).
- `roborev init gh-action`: generates a GitHub Actions workflow file with SHA256-verified roborev installation and agent setup.
- TUI fix jobs: press `F` on a completed review to launch a background fix in an isolated worktree. New Tasks view (`T`) for managing fix jobs and applying patches.
- CI poller auto-clone: repos in `ci.repos` no longer require a local `roborev init` checkout. The poller clones them automatically to `~/.roborev/clones/`.
- Quota-aware agent cooldown: agents that hit hard quota limits enter a timed cooldown (default 30 min) with automatic failover to backup agents. CI comments show "skipped (quota)" instead of "failed".
- Daemon activity logging for better operational visibility.

**Improvements**

- Review verdicts are stored for reuse in later review workflows.

**Bug fixes**

- Fix jobs now create worktrees at the reviewed commit instead of HEAD, preventing patches against the wrong revision.
- Database migration no longer crashes on databases with quoted table names from prior ALTER TABLE migrations.
- Missing git origin remote treated as confirmed mismatch for auto-clone instead of a transient error.
- Fixed a data race between `WorkerPool.Start` and `WorkerPool.Stop`.

**Acknowledgements**

- Thanks to [Hugh Brown](https://github.com/hughdbrown) for storing verdicts for later use.
- Thanks to [Alejandro Saucedo](https://github.com/axsaucedo) for adding `roborev ci review` and the `init gh-action` workflow generator.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for TUI-triggered fixes through background worktrees.

---

## 0.33
<small>2026-02-17</small>

**New features**

- `roborev compact` command to verify and consolidate open review findings, reducing false positives and merging related findings from multiple reviews into a single consolidated review.
- Backup-agent failover: automatically retry failed jobs with a secondary agent when the primary fails (e.g. fall back to Claude Code when Codex rate-limits).
- GitHub commit status checks: the CI poller posts pending/success/failure statuses on PR commits when GitHub App auth is configured.
- Ref-aware configuration: the CI poller reads `.roborev.toml` from the PR branch's git ref, so configuration can vary by branch.
- `--label` flag on `roborev run` for custom labels displayed in the TUI.

**Improvements**

- Consolidated review guidelines for more consistent review output across commands.
- Hardened CI and hook workflows for more reliable automated runs.

**Bug fixes**

- Post-rewrite hook preserves review history across rebases by remapping commit SHAs when patch content is unchanged.
- Skip hook upgrade checks in CI mode to avoid CI interruptions.

**Acknowledgements**

- Thanks to [Nick Strayer](https://github.com/nstrayer) for backup agent failover for review jobs.
- Thanks to [Hugh Brown](https://github.com/hughdbrown) for the `compact` command for verifying and consolidating reviews.

---

## 0.32
<small>2026-02-16</small>

**New features**

- `roborev wait` command to block until a review job completes, improving scripting and CI flows.
- Refine targeting flags so you can run `roborev refine` against specific findings.
- Unified TUI tree filter with lazy branch loading, search, and current-directory prioritization.

**Improvements**

- Improved TUI hint bar to make available actions clearer.
- Removed the hardcoded OpenCode model so model selection follows your configuration.

**Bug fixes**

- Fixed TUI Cursor cancel behavior and corrected closed/open stats display.
- Fixed agent prompt handling on Windows to avoid the 32KB command-line limit.
- Fixed refine loops so git hook failures no longer break execution.
- Stripped `CLAUDECODE` when spawning the `claude-code` agent to prevent environment leakage.

**Acknowledgements**

- Thanks to [Jeremy Jordan](https://github.com/jeremyjordan) for adding `roborev wait`.
- Thanks to [Nick Strayer](https://github.com/nstrayer) for the unified tree filter with lazy branch loading, search, and cwd prioritization.

---

## 0.31
<small>2026-02-11</small>

**New features**

- `roborev config` subcommands (`get`, `set`, `list`) for viewing and managing configuration from the CLI.
- `--branch <name>` flag on `roborev analyze` and explicit branch names in `roborev review --branch`.

**Improvements**

- Refreshed built-in Claude and Codex skill guides for review/refine/respond/fix workflows.

**Acknowledgements**

- Thanks to [Hugh Brown](https://github.com/hughdbrown) for skill tuneups.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the `roborev config` get/set/list subcommand.

---

## 0.30
<small>2026-02-11</small>

**New features**

- TUI renders Markdown in review output for clearer formatting.

**Improvements**

- TUI output is sanitized and escaped to prevent control sequences from breaking terminal rendering.

---

## 0.29
<small>2026-02-10</small>

**New features**

- `review` and `review-branch` skills for Codex and Claude to run code reviews from agent skills.
- `design-review-branch` skills for Codex and Claude.

**Improvements**

- Normalized skill invocation patterns for more consistent matching.
- Improved Codex stream handling with stronger merge guarding.

**Bug fixes**

- Fixed cases where the Codex agent produces no visible CLI output.
- Fixed range reviews that fail when the start point is the repository root commit.

---

## 0.28
<small>2026-02-10</small>

**New features**

- Server-side filtering for review/job lists.
- Automatic TUI filtering to narrow visible reviews and jobs.
- Filter metrics to show what filtering matches.

**Improvements**

- Clearer TUI command display and improved prompt navigation.

**Bug fixes**

- Prevented the daemon from inheriting `GIT_DIR` from Git hook environments.

---

## 0.27
<small>2026-02-09</small>

**New features**

- `--type` flag for `design` and `security` reviews from the CLI.
- Jump-to-top shortcut (`g`) in the TUI.
- Built-in design review skill templates for Codex and Claude.

**Bug fixes**

- Fixed review-type consistency so selected modes are applied reliably across commands.

**Acknowledgements**

- Thanks to [Benn Stancil](https://github.com/bstancil) for the TUI jump-to-top shortcut.
- Thanks to [Hugh Brown](https://github.com/hughdbrown) for the `--type` flag for design and security review types.

---

## 0.26
<small>2026-02-08</small>

**New features**

- CI poller that detects GitHub pull requests and queues reviews automatically.
- GitHub App integration for authenticated PR review workflows.
- Persistent CI review tracking that survives daemon restarts.
- `hide_closed_by_default` config option for the TUI.

**Improvements**

- Expanded configuration to support CI polling and GitHub App settings.
- Default Gemini model set so Gemini works without explicit model configuration.
- Hardened agent integrations for improved reliability.

**Acknowledgements**

- Thanks to [Aseem Bansal](https://github.com/anshbansal) for `hide_addressed_by_default`, the Gemini default model, and agent/test hardening.

---

## 0.25
<small>2026-02-04</small>

**New features**

- `roborev list` command for viewing stored reviews.
- `--json` flag on `roborev show` for machine-readable output.
- Color-coded Closed column in the TUI.

**Improvements**

- TUI queue view displays `JobID` instead of `ID` for clearer identification.

**Bug fixes**

- Fixed verdict detection for `Severity: Level` format.
- Fixed hook v1 to v2 upgrade by stripping `&` and documenting `install-hook --force`.

---

## 0.24
<small>2026-02-03</small>

**Improvements**

- Show available fixes in the `roborev fix` list.
- Fail fast when task jobs are missing a prompt.

**Bug fixes**

- Prevented wrong agent selection and duplicate reviews from the post-commit hook.

**Acknowledgements**

- Thanks to [Hugh Brown](https://github.com/hughdbrown) for the fix-list fixes-available feature and adding `bin/` to `.gitignore`.

---

## 0.23
<small>2026-02-02</small>

**New features**

- `/roborev:fix` skill to address multiple review findings in one pass.
- `{findings}` template variable for hook commands.

**Improvements**

- Show skill status in `roborev skills` output.
- Upgrade post-commit hook on init to keep tooling up to date.

**Bug fixes**

- Fixed post-commit hook backgrounding to avoid blocking or hangups.

**Acknowledgements**

- Thanks to [John Zila](https://github.com/jzila) for the `{findings}` hook command template variable.

---

## 0.22
<small>2026-01-31</small>

**New features**

- Review hooks system to run shell commands when reviews complete or fail.
- `--batch` flag on `roborev fix` for batch operation.

**Improvements**

- Rewritten README documenting the coding agent workflow.

**Bug fixes**

- Fixed hook tests for portability across environments.

---

## 0.21
<small>2026-01-30</small>

**New features**

- Cursor agent support.
- `check-agents` command to list and smoke-test available agents.
- `--open` flag on `fix` for batch fixing.
- `show --prompt` to display the prompt sent to the agent.

**Improvements**

- Improved daemon resilience and overall UX.
- Include current UTC date in review prompts for temporal context.

**Bug fixes**

- Fixed shell wildcard expansion in `analyze` when run from subdirectories.
- Prevented duplicate review jobs when enqueueing.
- Fixed branchless jobs not included when running fix.

---

## 0.20
<small>2026-01-29</small>

**New features**

- `roborev analyze` for built-in code analysis workflows.
- `roborev fix` to apply guided fixes from analysis results.

**Bug fixes**

- Fixed cosmetic issues in repo stats display.
- Fixed zero "Created" date in `roborev repo show`.

---

## 0.19
<small>2026-01-27</small>

**New features**

- Workflow-specific configuration keys and `--fast` shorthand flag.
- Branch column in the TUI with filtering support.
- `--local` flag to run reviews without starting the daemon.

**Improvements**

- Improved TUI row selection styling.

**Bug fixes**

- Fixed branch filter returning no results when fetch is limited.
- Fixed false negative verdicts when severity labels are present.
- Fixed `make install` to avoid using `go install`.

---

## 0.18
<small>2026-01-26</small>

**New features**

- `tail` command to view streaming agent output.
- Support for multiple clones running concurrently.
- Automatic terminal color adaptation for light/dark themes.

**Improvements**

- Show model name and reorganized Review screen layout.

**Bug fixes**

- Fixed `address` API and CLI to use `job_id` correctly.

---

## 0.17
<small>2026-01-25</small>

**New features**

- Configurable model selection for all agents.
- Gemini-specific preamble support for run tasks.
- TUI commit viewer, help modal, and clearer navigation feedback.

---

## 0.16
<small>2026-01-24</small>

**New features**

- Layered Escape key behavior to clear filters one level at a time.
- Gemini-specific review template with upfront summary requirement.

**Improvements**

- Renamed `prompt` command to `run` for clearer CLI usage.
- Improved daemon lifecycle management for safer start/stop.

**Bug fixes**

- Fixed TUI flickering when the queue is empty with filters applied.
- Fixed edge cases in daemon shutdown.

---

## 0.15
<small>2026-01-23</small>

**New features**

- Config hot-reload for the daemon.
- Factory Droid agent support.
- `y` hotkey to copy review content to the clipboard.
- Review metadata header in clipboard yank content.
- PowerShell installer and ARM64 builds for Windows.

**Improvements**

- Flash notifications for incomplete jobs in the TUI.
- Homebrew tap integration for easier installation.

**Bug fixes**

- Fixed multi-byte character handling in TUI text input.
- Fixed Codex agent stdin handling on Windows.

**Acknowledgements**

- Thanks to [Arthur Gerigk](https://github.com/gerigk) for Factory Droid agent support.

---

## 0.14
<small>2026-01-21</small>

**New features**

- TUI respond modal to capture review responses and include them in future prompts.

**Bug fixes**

- Fixed TUI rendering artifacts when scrolling with page up/down.

---

## 0.13
<small>2026-01-20</small>

**New features**

- PostgreSQL sync to share reviews across multiple machines.

**Improvements**

- Simplified `install.sh` and moved docs screenshots to the documentation site.
- Instruct reviewers to skip commit message review.

**Bug fixes**

- Fixed race condition that caused closed items to briefly reappear.
- Fixed markdown formatting in verdict parsing.
- Fixed `sync now` to connect automatically when the daemon is not yet connected.

---

## 0.12
<small>2026-01-19</small>

**New features**

- `--since` option on `roborev review` to scope reviews to recent changes.
- Gemini support for `roborev refine`.
- Copilot and OpenCode agent support.

**Improvements**

- Default `allow_unsafe_agents` to true for refine when using Claude.
- Improved TUI rendering and presentation.

**Bug fixes**

- Fixed TUI rendering glitches and layout issues.

---

## 0.11
<small>2026-01-18</small>

**New features**

- `roborev prompt` command for custom agent tasks.
- `roborev repo` command for managing tracked repositories.
- Nix flake app entry for roborev.

**Improvements**

- Claude Code compatibility for `roborev refine`.
- Expanded daemon API to support repo and prompt operations.

**Acknowledgements**

- Thanks to [Hussain Sultan](https://github.com/hussainsultan) for adding the Nix roborev app.

---

## 0.10
<small>2026-01-16</small>

**New features**

- `roborev skills install` command to install bundled agent skills.
- Bundled skills for Claude Code and Codex (address/respond workflows).

---

## 0.9
<small>2026-01-14</small>

**New features**

- `refine` command for automated review fixing.

**Improvements**

- Allow `roborev refine` on main with `--since`, waiting for in-progress reviews.
- Use configured `display_name` in the filter modal.

**Bug fixes**

- Fixed queue cursor behavior when hide-closed is active and closing from the review screen.

---

## 0.8
<small>2026-01-13</small>

**New features**

- Renamed `enqueue` to `review` with a cleaner CLI interface.
- `--dirty` flag to review uncommitted changes.
- `--wait` flag to keep the CLI open until review completes.

---

## 0.7
<small>2026-01-11</small>

**New features**

- `r` hotkey to rerun failed/canceled jobs or start a new review.
- `roborev stream` command for JSONL event streaming.
- `excluded_branches` and `display_name` config options.
- Nix flake for building and development.

**Improvements**

- Full commit message bodies included in review prompts.
- Clearer new release notifications.

**Bug fixes**

- Fixed git worktrees being treated as separate repositories.
- Fixed false positive "failed" reviews.

**Acknowledgements**

- Thanks to [John Zila](https://github.com/jzila) for JSONL event streaming, git worktree repository detection fixes, and the Nix flake.

---

## 0.6
<small>2026-01-10</small>

**New features**

- `h` hotkey to hide closed reviews.
- Branch display in the TUI review view.
- Distinct `[CLOSED]` color styling in review view.

**Improvements**

- Improved verdict parsing.
- Improved TUI height sizing and review ID display.

**Bug fixes**

- Fixed TUI height sizing display issues.

---

## 0.5
<small>2026-01-09</small>

**New features**

- Filter-by-repo modal in the TUI.
- `ROBOREV_DATA_DIR` env var to override the data directory.
- Configurable job timeout.
- TUI pagination for large review lists.
- Keyboard navigation between reviews without returning to the list.
- P/F (Pass/Fail) verdict column in the TUI queue.

**Improvements**

- TUI views fit terminal width dynamically.
- More robust executable path handling for hooks.

**Acknowledgements**

- Thanks to [Andy Hadjigeorgiou](https://github.com/andyxhadji) for configurable job timeouts and responsive TUI widths.

---

## 0.4
<small>2026-01-08</small>

**New features**

- `roborev update` command to check for and install updates.
- TUI notification when a new version is available.
- Husky git hook manager support.

**Improvements**

- Automatic `.git/hooks` directory creation.
- Respect `core.hooksPath` for git operations.
- Refactored post-commit hook for improved security and silent operation.
- Detect rebase state and skip reviews during rebase.

**Bug fixes**

- Fixed version comparison for dev builds.
- Fixed Windows path detection for hook locations.

**Acknowledgements**

- Thanks to [Tenzin Wangdhen](https://github.com/sinzin91) for Husky git hook manager support and automatic `.git/hooks` directory creation.

---

## 0.3
<small>2026-01-07</small>

**New features**

- Job cancellation with `x` key in the TUI (terminates agent subprocess).
- `uninstall-hook` command.

**Bug fixes**

- Fixed TUI selection highlight to cover the full line.
- Fixed job cancellation persistence and race conditions.
- Fixed migration handling for foreign keys and ALTER TABLE ordering.

---

## 0.2
<small>2026-01-06</small>

**New features**

- Project-specific review guidelines in `.roborev.toml`.
- Closed status tracking with a dedicated closed column and toggle.
- Prompt inspection in the TUI.
- Page up/down navigation in the TUI.
- Daemon version tracking with auto-restart on upgrade.
- Gemini CLI and Copilot CLI agent support.
- OpenCode agent support.
- Automatic retry for failed reviews (up to 3 attempts).

**Improvements**

- Optimistic updates for close toggle.
- Compact timestamp format in the TUI queue.
- Daemon version displayed in the TUI and CLI.

**Bug fixes**

- Fixed TUI queue edge cases for empty queues and navigation.
- Fixed daemon stop behavior and restart reliability.
- Fixed SQLite datetime parsing for TUI timestamps.
- Fixed retry job atomicity.

**Acknowledgements**

- Thanks to [Jonathan](https://github.com/etothexipi) for OpenCode agent support.

---

## 0.1
<small>2026-01-05</small>

Initial release.

- Pure-Go SQLite driver for static binaries.
- `--addr` normalization to add `http://` prefix if missing.
