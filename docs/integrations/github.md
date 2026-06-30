---
title: GitHub Integration
description: Automatically review GitHub PRs and post results as bot comments
---

roborev can poll GitHub for open pull requests, run code reviews on each one, and post the results as PR comments. In 0.57 and later, daemon CI reviews run through the [subagent review panel](/advanced/subagent-review-panels/) system: each PR HEAD gets one panel run and one synthesis parent review.

## How It Works

The CI poller runs inside the roborev daemon. On each interval it:

1. Lists open PRs for each configured repo via `gh pr list`
2. Skips PRs that have already been reviewed at their current HEAD SHA, subject to throttling and deferred retry state
3. Fetches the PR head commit (including fork-based PRs)
4. Computes the frozen merge-base range (`base..head`) and includes human PR discussion from trusted collaborators in the review prompt
5. Loads `.roborev.toml` from the repo's default branch, then resolves either `[ci] panel` or the compatible CI matrix (`agents`, `review_types`, or `[ci.reviews]`)
6. Enqueues panel member jobs plus one gated synthesis parent job
7. Posts the synthesized result as a PR comment after the parent finishes, but only after rechecking that the PR is still open, the HEAD SHA is unchanged, and the repo identity still matches

For multi-agent or multi-type configurations, member results are synthesized into a single combined PR comment. When only one member produces output, roborev can pass that output through without an extra synthesis agent call.

## What to Expect

Before enabling the CI poller, understand the following:

- **The poller reviews ALL open PRs on first start.** It polls immediately on startup, not after the first interval. If you have 20 open PRs, all 20 will be enqueued for review right away. Subsequent polls only review PRs with new commits (tracked by HEAD SHA in the local database).
- **All open PRs are reviewed.** There is no filtering by draft status, labels, or author. Draft PRs, bot PRs, and stale PRs all get reviewed.
- **CI settings are global by default, with per-repo overrides.** The `panel`, `agents`, `review_types`, and `model` in the global `[ci]` section apply to every repo unless overridden. Individual repos can override panels, agents, review types, and reasoning level via the `[ci]` section in their `.roborev.toml` (see [Per-Repo Overrides](#per-repo-overrides)).
- **Reviews run with `max_workers` concurrency** (default: 4). Jobs are enqueued immediately but executed up to 4 at a time. A panel consumes normal worker capacity as its members run. A 2 type x 2 agent matrix creates 4 members plus 1 synthesis parent.
- **PR comments include panel metadata.** Comments include a footer with the panel name, synthesis job, member reviewers, statuses, runtimes, and optional costs. When only some panel jobs have reported cost, the footer marks the total as partial.
- **The daemon does not survive reboots.** Use `roborev daemon start` to run in the background, but you'll need a launchd agent (macOS) or systemd service (Linux) if you want it to start on boot.

!!! warning "First-run with many open PRs"
    If you're enabling the CI poller on a repo with many open PRs, consider temporarily setting `repos` to a test repo first, verifying it works, and then adding your real repos. This avoids an unexpected burst of review jobs and PR comments.

## Choose Your Authentication Method

roborev needs GitHub credentials to list PRs and post comments. There are two options:

| | GitHub App (Recommended) | Personal (`gh` CLI) |
|---|---|---|
| **Comments appear as** | `your-app-name[bot]` | Your personal GitHub account |
| **Setup effort** | Create an app, generate keys, install on repos | Minimal: just `gh auth login` |
| **Best for** | Teams, shared repos, production use | Quick testing, personal projects |
| **Permissions** | Scoped to specific repos and permissions | Whatever your account has access to |

!!! tip "Which should I pick?"
    **GitHub App** is the recommended approach: dedicated bot identity, scoped permissions, and clean separation from your personal account. Use **personal auth** if you just want to try roborev quickly without creating an app.

## Prerequisites

Before enabling the CI poller, you need:

1. **`gh` CLI** installed (roborev shells out to `gh` for PR listing and comment posting):
   ```bash
   # Install: https://cli.github.com/
   gh --version   # verify it's installed
   ```

   If you're using **GitHub App auth**, `gh` does not need to be separately authenticated. The app token is injected automatically. If you're using **personal auth**, you also need to log in:
   ```bash
   gh auth login
   gh auth status   # verify it worked
   ```

   If your organization uses SSO with personal auth, make sure your token is authorized for SSO access. The daemon runs long-lived. If your `gh` token expires while the daemon is running, the poller will log errors and skip repos until you re-authenticate.

2. **A local checkout** of each repo you want to poll (optional for CI-only setups):
   ```bash
   cd /path/to/myrepo
   roborev init              # starts daemon automatically
   roborev init --no-daemon  # if using systemd/launchd to manage the daemon
   ```
   The poller matches GitHub repos to local checkouts by git remote URL. If no local checkout is found, the poller automatically clones the repo to `~/.roborev/clones/{owner}/{repo}` and uses that clone for reviews. A missing git origin remote is treated as a confirmed mismatch (triggering auto-clone) rather than a transient error.

   If you do provide a local checkout, it must use `origin` as its remote name (the default). The poller runs `git fetch origin` and `git fetch origin pull/<number>/head` to retrieve PR commits, including those from contributor forks.

3. **At least one AI agent** installed. The poller auto-detects installed agents in this order: `codex`, `claude-code`, `gemini`, `copilot`, `opencode`, `cursor`, `kiro`, `kilo`, `droid`, `pi`. You can check what's available with:
   ```bash
   roborev check-agents             # smoke-test all installed agents
   roborev check-agents --agent codex  # test a specific agent
   ```
   Or set a specific agent in the `[ci]` config (see below).

## Setup with GitHub App (Recommended)

PR comments will appear as **`your-app-name[bot]`** with scoped permissions.

### 1. Create the GitHub App

Go to **GitHub Settings > Developer settings > GitHub Apps > New GitHub App**.

| Field | Value |
|-------|-------|
| **App name** | A globally unique name, e.g. `roborev-myorg` (this becomes the `[bot]` username) |
| **Homepage URL** | Your repo URL or any URL |
| **Webhook** | Uncheck "Active" (not needed -- roborev polls) |

Under **Repository permissions**, set:

- **Pull requests**: Read & write
- **Contents**: Read-only
- **Commit statuses**: Read & write

Leave everything else as "No access". Click **Create GitHub App**.

!!! important "If app permissions are currently empty"
    If you already created the app with empty permissions:

    1. Open app settings and go to **Permissions & events**.
    2. Set **Pull requests** to **Read and write**, **Contents** to **Read-only**, and **Commit statuses** to **Read and write**.
    3. Save the app settings.
    4. For each existing installation, open installation settings and **accept the updated permissions**.

    Until each installation accepts the new permissions, roborev may fail to list PR data, post PR comments, or publish commit status checks.

!!! tip
    Set the app visibility to **Any account** if you want others to install it on their repos. This is under the app's **Advanced** settings after creation.

### 2. Note the App ID

After creation, the **App ID** is shown near the top of the app settings page. You'll need this for `github_app_id`.

### 3. Generate a Private Key

On the app settings page, scroll to **Private keys** and click **Generate a private key**. Your browser downloads a `.pem` file. Store it securely:

```bash
mkdir -p ~/.roborev
# The downloaded file will be named something like your-app-name.2026-02-08.private-key.pem
mv ~/Downloads/your-app-name.*.private-key.pem ~/.roborev/roborev.pem
chmod 600 ~/.roborev/roborev.pem
```

### 4. Install the App on Your Repos

From the app settings page, click **Install App** in the left sidebar. Choose the account or organization that owns your repos, and select which repositories to grant access to.

After installing, note the **installation ID** from the URL:

```
https://github.com/settings/installations/12345678
                                           ^^^^^^^^
                                           this is your installation ID
```

If you have repos across multiple organizations or user accounts, install the app on each one. Each installation gets its own installation ID. You'll need these for the [multi-installation config](#multiple-installations) below.

### 5. Add CI config

Add to `~/.roborev/config.toml`:

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/myrepo"]
agents = ["codex"]
review_types = ["security"]

# GitHub App authentication
github_app_id = 123456
github_app_private_key = "~/.roborev/roborev.pem"
github_app_installation_id = 12345678
```

To use a named panel for CI, define `[review.subagents]` and `[review.panels]`, then replace `agents` and `review_types` with `panel = "ci"`. See [Named CI Panels](#named-ci-panels).

The `github_app_private_key` field accepts:

- A file path: `~/.roborev/roborev.pem` (tilde is expanded)
- An environment variable: `${ROBOREV_APP_KEY}` (expands to a path or inline PEM content)
- Inline PEM content (starting with `-----BEGIN`)

!!! warning "Avoid inline PEM in config files"
    Prefer a file path or `${ENV_VAR}` reference over inline PEM content. Inline keys are easy to commit to version control accidentally. Make sure your config file is excluded from VCS (the global config at `~/.roborev/config.toml` is outside your repo by default).

App auth requires `github_app_id`, `github_app_private_key`, and at least one installation ID (either `github_app_installation_id` or entries in `github_app_installations`). If none are configured, the poller falls back to your personal `gh` auth.

#### Multiple Installations

If your repos span multiple GitHub organizations or user accounts, each one has its own app installation with a separate installation ID. Use the `[ci.github_app_installations]` table to map each owner to its installation ID:

```toml
[ci]
enabled = true
repos = ["wesm/my-project", "roborev-dev/core"]

github_app_id = 123456
github_app_private_key = "~/.roborev/roborev.pem"

[ci.github_app_installations]
wesm = 111111
roborev-dev = 222222
```

The poller extracts the owner from each repo (the part before `/`) and looks up the matching installation ID. Owner matching is case-insensitive, so `wesm` matches repos listed as `Wesm/repo` or `WESM/repo`.

You can also mix the map with the singular `github_app_installation_id` as a fallback for owners not in the map:

```toml
[ci]
github_app_id = 123456
github_app_private_key = "~/.roborev/roborev.pem"
github_app_installation_id = 111111   # fallback for unlisted owners

[ci.github_app_installations]
roborev-dev = 222222                  # this org uses a different installation
```

Each installation gets its own cached access token, so there is no performance penalty for multiple installations.

### 6. Start the daemon and verify

```bash
roborev daemon start        # background mode
roborev daemon run          # or foreground mode to watch logs
```

Look for the log line:

```
CI poller: GitHub App authentication enabled (app_id=123456)
```

PR comments will now appear as **`your-app-name[bot]`**.

## Setup with Personal Auth

If you don't want to create a GitHub App, you can use your personal `gh` CLI login instead. PR comments will appear as your GitHub account.

### 1. Add CI config

Add to `~/.roborev/config.toml` (make sure you've already run `roborev init` in your local checkout per [Prerequisites](#prerequisites)):

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/myrepo"]
agents = ["codex"]              # which agents to use (or omit for auto-detect)
review_types = ["security"]     # "security", "design", "lookahead", or "default"
```

> `"default"` runs the standard code review (bugs, security, testing gaps, regressions, code quality), the same as `roborev review` without `--type`. The aliases `"review"` and `"general"` are also accepted. Note that the CI config defaults to `["security"]` if `review_types` is not set.

To use a named panel instead of the matrix, set `panel = "ci"` and define the panel under `[review.panels.ci]`. See [Named CI Panels](#named-ci-panels).

No `github_app_*` fields needed. The daemon posts comments using whatever account `gh auth` is logged in as.

### 2. Start the daemon

```bash
roborev daemon start        # background mode
roborev daemon run          # or foreground mode to watch logs
```

## Verifying It Works

On startup you should see:

```
CI poller started (interval: 5m0s, repos: [myorg/myrepo])
```

The poller checks for open PRs immediately, then on each interval. When a review completes, you'll see:

```
CI poller: posted review comment on myorg/myrepo#42 (job 123, verdict=P)
```

Use `roborev status` to check the daemon and queue state at any time.

!!! note "CI reasoning level"
    The CI poller defaults to `reasoning = "thorough"` for all review jobs. You can override this per-repo with a `[ci]` section in the repo's `.roborev.toml` (see [Per-Repo Overrides](#per-repo-overrides)).

## Commit Status Checks

When GitHub App authentication is configured, the CI poller posts commit status checks on each PR's head commit. These appear as check entries on the PR and in the commit status list on GitHub.

The status context is `roborev` and progresses through these states:

| State | When |
|-------|------|
| `pending` | A panel is queued, running, throttled, or deferred for retry |
| `success` | The review process completed and a comment was posted, including comments that contain findings |
| `failure` | At least one member failed to run for a genuine reason while another member still produced usable review output |
| `error` | No reviewer produced usable output because of no available agent, repeated genuine failures, or all member jobs failing |

Status checks require the **Commit statuses: Read and write** permission on your GitHub App. The setup guide above already includes it. If your app predates that permission, add it:

1. Open your GitHub App settings and go to **Permissions & events**
2. Under **Repository permissions**, set **Commit statuses** to **Read and write**
3. Save and accept the updated permissions on each installation

If no GitHub App is configured, or the app lacks the commit statuses permission, status checks are silently skipped. PR comments are still posted regardless.

!!! note
    Status checks are posted per commit, not per member job. The status reflects whether the review infrastructure completed, not whether the reviewer found code issues. Findings are reported in the PR comment.

## Keeping the Daemon Running

`roborev daemon start` runs the daemon in the background, but it won't survive a reboot. See [Persistent Daemon](/configuration/#persistent-daemon) for launchd (macOS) and systemd (Linux) setup.

## Wildcard Repository Patterns

Instead of listing every repository individually, you can use glob patterns in `ci.repos` to match multiple repos under an owner. The owner part (before the `/`) must be literal.

```toml
[ci]
enabled = true
repos = [
  "myorg/*",           # All repos under myorg
  "myorg/api-*",       # Only repos starting with "api-"
  "other/specific",    # Exact repo still works
]

# Exclude repos matching these patterns
exclude_repos = ["myorg/archived-*", "myorg/internal-*"]

# Safety cap on total expanded repos (default: 100)
max_repos = 50
```

Patterns use Go's `path.Match` syntax (`*` matches any sequence of characters, `?` matches a single character, `[...]` matches character classes). Matching is case-insensitive.

Wildcard expansion calls the GitHub API (`gh repo list`) and caches results for one hour. Archived repos are automatically excluded from the API results. Explicit (non-wildcard) repos always take priority when `max_repos` is reached.

Exclusion patterns in `exclude_repos` apply to both exact entries and wildcard-expanded entries.

## Named CI Panels

Named panels give the CI poller explicit reviewer roles instead of a pure agent x review type matrix. This is the most flexible CI setup in 0.57 and later.

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/myrepo"]
panel = "ci"

[review.subagents.bug]
agent = "codex"
review_type = "default"
instructions = "Focus on correctness, regressions, and tests."

[review.subagents.security]
agent = "claude-code"
review_type = "security"
instructions = "Focus on authn/authz, injection, secrets, and unsafe file access."

[review.subagents.design]
agent = "codex"
review_type = "design"
allow_failure = true

[review.panels.ci]
members = ["bug", "security", "design"]
synthesis_agent = "codex"
```

When `panel` is set, the CI poller ignores `agents`, `review_types`, and `[ci.reviews]` for that repo. The named panel is resolved from global config plus the repo's `.roborev.toml` as loaded from the repo's default branch. Repo definitions override global definitions by name.

Use `allow_failure = true` on a subagent when that reviewer is best-effort or depends on flaky infrastructure. Its successful output is still included, but a failure or cancellation will not block an otherwise usable CI panel comment.

See [Subagent Review Panels](/advanced/subagent-review-panels/) for the full panel configuration reference.

## Multi-Review Types and Agents

If `panel` is not set, you can still configure multiple review types and agents for each PR. The CI poller adapts the matrix into an implicit panel, with one member per review type and agent pair, then posts a single synthesized comment.

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/myrepo"]

# Run both security and standard code reviews
review_types = ["security", "default"]

# Use multiple agents
agents = ["codex", "gemini"]

# This creates 4 panel members per PR (2 types x 2 agents)
```

When the matrix is 1x1 (single review type, single agent), the synthesis parent can pass the member output through directly without an extra synthesis agent call.

### Granular Review Matrix

For finer control, use `[ci.reviews]` to assign specific review types to specific agents instead of the full cross-product:

```toml
[ci]
enabled = true
repos = ["myorg/backend"]

[ci.reviews]
codex = ["security"]
gemini = ["security", "default"]
```

This creates 3 panel members per PR (codex runs security, gemini runs security and default) rather than the 4 you'd get from a 2x2 cross-product. Map keys are sorted for deterministic job order.

When `[ci.reviews]` is set, `agents` and `review_types` are ignored. To disable reviews for a specific repo, set an empty `[ci.reviews]` table in the repo's `.roborev.toml`:

```toml
# .roborev.toml: disable CI reviews for this repo
[ci.reviews]
```

### Synthesis

When multiple members complete with findings, their outputs are combined by a synthesis step into a single well-formatted PR comment. The synthesis agent:

- Deduplicates findings reported by multiple agents
- Organizes findings by severity
- Preserves file and line references
- Produces a one-line summary verdict

For implicit matrix CI, customize synthesis with `[ci]` settings:

```toml
[ci]
synthesis_agent = "claude-code"                  # Agent to use for synthesis
synthesis_backup_agent = "gemini"                # Backup if primary fails
synthesis_model = "claude-opus-4-8"   # Model override for synthesis
```

If the primary synthesis agent fails because of quota or availability, roborev tries `synthesis_backup_agent` before falling back to raw formatting. Named panels use their own `synthesis_agent`, `synthesis_model`, `synthesis_backup_agent`, and `synthesis_backup_model` fields under `[review.panels.<name>]`.

## Comment Upsert

By default, each review run creates a new PR comment. When `upsert_comments` is enabled, roborev finds and updates its existing comment instead of posting a duplicate. This keeps PR threads clean when reviews run repeatedly on the same PR.

```toml
[ci]
upsert_comments = true
```

roborev embeds an invisible HTML marker in its PR comments to identify them. When upserting, it searches for the marker, patches the matching comment via the GitHub API, and falls back to creating a new comment if the existing one can't be updated (e.g., token mismatch between the original poster and the current auth).

Per-repo overrides in `.roborev.toml` can enable or disable upsert independently of the global setting:

```toml
# .roborev.toml
[ci]
upsert_comments = false   # Disable upsert for this repo even if globally enabled
```

## PR Throttling

When contributors push frequently to the same PR, the poller can generate excessive reviews. The `throttle_interval` config sets a minimum time between reviews of the same PR:

```toml
[ci]
throttle_interval = "1h"              # default; minimum time between reviews per PR
throttle_bypass_users = ["wesm"]      # these users bypass throttling
```

When a PR is pushed within the throttle window, the poller defers the review and posts a pending GitHub status showing the next eligible review time. Set `throttle_interval = "0"` to disable throttling entirely.

Throttling is bypassed when a new push supersedes an in-progress review: the old review is canceled and the new one starts immediately, so you always get feedback on the latest code.

Users listed in `throttle_bypass_users` get immediate reviews on every push regardless of the interval. Matching is case-insensitive.

## Safe CI Retries

The CI poller tracks retry state per repo, PR number, and HEAD SHA. If any panel member produces real review output, roborev posts the available review instead of discarding it. If no member produces output, the poller classifies the outcome:

| Outcome | Behavior |
|---------|----------|
| Transient provider outage or synthesis quota failure | Defers without a PR comment, keeps the status `pending`, and retries with exponential backoff. The first delay is 2 minutes, it doubles up to a 1 hour cap, and transient retries give up after 72 hours. |
| Genuine member failure | Retries up to 3 consecutive genuine attempts. After that, roborev posts an all failed note and sets an `error` status. |
| Quota or timeout skips only | Posts an all skipped summary and uses a nonblocking status. |

If panel members produced review output but the synthesis agent hits quota or a transient provider failure, roborev now retries the panel instead of posting the degraded raw-member fallback. Genuine synthesis failures still fall back to the available member output because retrying the same broken synthesis setup is unlikely to help.

Deferred retries are rechecked before they run. roborev retries only if the PR is still open and still points at the same HEAD SHA. Closed PRs, stale heads, and repo identity mismatches are retired without posting comments. If a new push arrives while an older panel is active, the older active panel is canceled and superseded by the new HEAD. Retry runs preserve the PR's configured agents and review panel; daemon cooldown state can skip an agent during execution, but it does not rewrite the retry's configured review plan.

## Per-Repo Overrides

Individual repos can override the global CI settings by adding a `[ci]` section to their `.roborev.toml` file. This lets you run different panels, agents, review types, or reasoning levels for different repos.

```toml
# .roborev.toml (in repo root)

agent = "codex"   # agent for post-commit reviews (unrelated to CI)

[ci]
panel = "ci"                         # run named [review.panels.ci] for this repo
# Or omit panel and use the compatible matrix:
# agents = ["gemini"]
# review_types = ["security", "default"]
reasoning = "standard"               # override reasoning level (thorough, standard, fast)
```

Per-repo overrides take priority over the global `[ci]` config. Any field not set in the repo's `[ci]` section falls back to the global config. If `panel` is set, it takes priority over the matrix fields for that repo.

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `panel` | string | global `panel` | Named `[review.panels.<name>]` panel for CI reviews of this repo |
| `agents` | array | global `agents` | Agents for CI reviews of this repo |
| `review_types` | array | global `review_types` | Review types for CI reviews of this repo |
| `reviews` | table | global `reviews` | Granular agent-to-review-type map (overrides `agents` and `review_types`; empty table disables reviews) |
| `reasoning` | string | `"thorough"` | Reasoning level: `thorough`, `standard`, or `fast` |
| `min_severity` | string | `"low"` | Minimum severity to include: `low`, `medium`, `high`, or `critical` |
| `upsert_comments` | bool | global `upsert_comments` | Override global comment upsert setting for this repo |
| `include_costs` | bool | global `include_costs` | Include token cost estimates in PR comment footers for this repo |

## CI Options Reference

### Core Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `enabled` | bool | `false` | Enable the CI poller |
| `poll_interval` | string | `"5m"` | How often to check for PRs (minimum 30s, invalid values default to 5m) |
| `repos` | array | `[]` | GitHub repos to poll in `"owner/repo"` format. Supports glob patterns (e.g. `"myorg/*"`, `"myorg/api-*"`). |
| `exclude_repos` | array | `[]` | Glob patterns to exclude from the resolved repo list |
| `max_repos` | int | `100` | Safety cap on total expanded repos (explicit repos have priority over wildcard-expanded ones) |
| `panel` | string | | Named `[review.panels.<name>]` panel for CI reviews. When set, overrides `agents`, `review_types`, and `reviews`. |
| `review_types` | array | `["security"]` | Review types to run for each PR: `security`, `design`, `lookahead`, or `default`. `"review"` and `"general"` are accepted as aliases for `"default"`. |
| `agents` | array | auto-detect | Agents to run for each PR (e.g., `["codex", "gemini"]`) |
| `reviews` | table | | Granular agent-to-review-type map. Overrides `agents` and `review_types` when set. See [Granular Review Matrix](#granular-review-matrix). |
| `model` | string | | Model override for CI reviews |
| `min_severity` | string | `"low"` | Minimum severity to include in output: `low`, `medium`, `high`, or `critical` |
| `throttle_interval` | string | `"1h"` | Minimum time between reviews of the same PR. Set `"0"` to disable. |
| `throttle_bypass_users` | array | `[]` | GitHub usernames that bypass throttling (case-insensitive) |
| `synthesis_agent` | string | | Agent for combining implicit matrix results |
| `synthesis_backup_agent` | string | | Backup agent for implicit matrix synthesis when the primary fails |
| `synthesis_model` | string | | Model override for implicit matrix synthesis |
| `upsert_comments` | bool | `false` | Update existing PR comments instead of creating new ones |
| `include_costs` | bool | `false` | Include token cost estimates in PR comment footers |
| `discord_webhook_url` | string | | Discord webhook URL for best-effort CI job failure notifications |
| `batch_timeout` | string | `"15m"` | Maximum time to wait for panel members before posting available results. Set `"0"` to disable. |

When `agents` is empty, the poller auto-detects the first available agent from: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi.

### GitHub App Options

| Option | Type | Description |
|--------|------|-------------|
| `github_app_id` | integer | App ID from the app settings page |
| `github_app_private_key` | string | Path to PEM file, `${ENV_VAR}`, or inline PEM |
| `github_app_installation_id` | integer | Installation ID (fallback for owners not in the installations map) |
| `github_app_installations` | table | Map of owner name to installation ID for multi-org setups (see [Multiple Installations](#multiple-installations)) |

App auth requires `github_app_id`, `github_app_private_key`, and at least one installation ID (either `github_app_installation_id` or entries in `github_app_installations`). If none are configured, the poller falls back to default `gh` authentication. For repos whose owner has no matching installation ID, the poller also falls back to default `gh` auth for that repo.

## Troubleshooting

The daemon logs to stdout (or to the log file if using a system service). Common issues:

**"no local repo found matching..."**
You need to run `roborev init` in a local checkout of the repo. The poller matches GitHub `owner/repo` to local repos by git remote URL.

**"gh pr list: ..."**
The `gh` CLI is not installed, not authenticated, or doesn't have access to the repo. Run `gh auth status` and `gh pr list --repo owner/repo` to debug. If your org uses SSO, re-authorize your token with `gh auth refresh`.

**"merge-base ... : ..."**
The PR's base or head commit isn't available locally. This usually means `git fetch` failed. Check that the local repo has the remote configured correctly.

**"GitHub App token failed, falling back to default gh auth"**
The GitHub App authentication failed. Check that your PEM file path is correct, the app is installed on the repo, and the installation ID matches. The daemon falls back to your `gh` CLI auth. If you're also logged in via `gh auth login`, PR operations will still work but comments will appear as your personal account. If you're not logged in, `gh` commands will fail.

**"no installation ID for owner ..., using default gh auth"**
The poller found no installation ID for this repo's owner. If you're using `[ci.github_app_installations]`, add an entry for the owner. If you're using the singular `github_app_installation_id`, make sure it's set. Owner names are matched case-insensitively, so `wesm` and `Wesm` are equivalent.

**No log output at all for CI**
Check that `[ci] enabled = true` is in `~/.roborev/config.toml`. CI config is hot-reloaded; starting or stopping the poller may still require normal daemon lifecycle if CI was never running.

**Reviews enqueue but never complete**
Check `roborev status` to see if jobs are queued/running. The agent may be failing -- check the daemon logs for error messages from the agent.

**Unexpected review burst on first start**
This is normal. The poller reviews all open PRs on first startup. After the initial run, only PRs with new commits (different HEAD SHA) trigger new reviews. The tracking is persistent across daemon restarts.

## Full Examples

### GitHub App -- Single Review

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/backend", "myorg/frontend"]
review_types = ["security"]
agents = ["claude-code"]
model = "claude-opus-4-8"

github_app_id = 123456
github_app_private_key = "~/.roborev/roborev.pem"
github_app_installation_id = 12345678
```

### GitHub App -- Named Panel

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/backend"]
panel = "ci"
include_costs = true

github_app_id = 123456
github_app_private_key = "~/.roborev/roborev.pem"
github_app_installation_id = 12345678

[review.subagents.bug]
agent = "codex"
review_type = "default"
instructions = "Focus on correctness, regressions, and test coverage."

[review.subagents.security]
agent = "claude-code"
review_type = "security"
instructions = "Focus on authn/authz, injection, secrets, and unsafe file access."

[review.panels.ci]
members = ["bug", "security"]
synthesis_agent = "codex"
```

### GitHub App -- Multi-Agent Matrix

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/backend"]

# 2x2 matrix = 4 members plus 1 synthesis parent per PR
review_types = ["security", "default"]
agents = ["codex", "gemini"]

# Synthesis settings
synthesis_agent = "claude-code"
synthesis_backup_agent = "gemini"
synthesis_model = "claude-opus-4-8"

github_app_id = 123456
github_app_private_key = "${ROBOREV_APP_KEY}"
github_app_installation_id = 12345678
```

### GitHub App -- Granular Review Matrix

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/backend"]
throttle_interval = "30m"
throttle_bypass_users = ["wesm"]
synthesis_agent = "claude-code"

github_app_id = 123456
github_app_private_key = "${ROBOREV_APP_KEY}"
github_app_installation_id = 12345678

# Assign specific review types per agent (3 jobs instead of 4)
[ci.reviews]
codex = ["security"]
gemini = ["security", "default"]
```

### GitHub App -- Multiple Installations

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["wesm/my-project", "roborev-dev/core", "roborev-dev/docs"]
review_types = ["security"]
agents = ["codex"]

github_app_id = 123456
github_app_private_key = "~/.roborev/roborev.pem"

# Each org/user has its own app installation
[ci.github_app_installations]
wesm = 111111
roborev-dev = 222222
```

### Per-Repo Overrides

Global config (`~/.roborev/config.toml`) sets defaults for all repos:

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/backend", "myorg/frontend"]
review_types = ["security"]
agents = ["codex"]

github_app_id = 123456
github_app_private_key = "~/.roborev/roborev.pem"
github_app_installation_id = 12345678
```

The backend repo wants deeper reviews with multiple agents. Add a `.roborev.toml` in the backend repo root:

```toml
# myorg/backend/.roborev.toml

[ci]
review_types = ["security", "default"]
agents = ["codex", "gemini"]
reasoning = "thorough"
```

The frontend repo is lower-risk and only needs a fast security scan:

```toml
# myorg/frontend/.roborev.toml

[ci]
review_types = ["security"]
agents = ["codex"]
reasoning = "fast"
```

Result: `backend` PRs get a 2x2 implicit panel (4 members plus 1 synthesis parent) with thorough reasoning, while `frontend` PRs get a single fast security review. Repos without a `.roborev.toml` `[ci]` section use the global defaults.

### Wildcard Repos with Exclusions

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/*"]
exclude_repos = ["myorg/archived-*", "myorg/docs"]
max_repos = 50
review_types = ["security"]
agents = ["codex"]

github_app_id = 123456
github_app_private_key = "~/.roborev/roborev.pem"
github_app_installation_id = 12345678
```

### Personal Auth -- Single Review

```toml
[ci]
enabled = true
poll_interval = "5m"
repos = ["myorg/backend", "myorg/frontend"]
review_types = ["security"]
agents = ["claude-code"]
model = "claude-opus-4-8"
```

## Discord Failure Notifications

Set `discord_webhook_url` in the global `[ci]` config when you want an out-of-band alert for CI poller jobs that fail before roborev can post useful PR feedback:

```toml
[ci]
discord_webhook_url = "https://discord.com/api/webhooks/..."
```

The setting is hot-reloaded. Set it to an empty value or remove the key to disable notifications without restarting the daemon.

Notifications are sent for terminal CI review job failures only. They are best-effort: a slow, unavailable, or rate-limited Discord webhook does not change job state, CI retry state, GitHub status updates, or PR comment posting.

Each message includes the repository, CI base branch, job ID, panel/member context when available, agent, review type, ref, retry count, failure class, and trimmed error text. Raw error text is length-bounded but not path-sanitized, so send these notifications only to trusted private channels.

Quota/cooldown failures are deduped globally per canonical agent for the configured `agent_quota_cooldown` window. This keeps one agent quota incident from flooding Discord when many PRs or panel members route to the same unavailable agent. For example, one `codex` quota failure can suppress additional `codex` quota messages from other repos until the cooldown window expires; the first message is the representative failure for that daemon-wide agent cooldown.

The webhook URL is a secret-bearing credential. roborev masks it in config output, but anyone with the raw URL can post to the Discord channel. Rotate the webhook in Discord if the URL is accidentally shared.

## Quota Handling

When an agent hits a hard rate or quota limit, roborev puts that agent into a timed cooldown instead of failing the review immediately.

| Setting | Default | Description |
|---------|---------|-------------|
| `agent_quota_cooldown` | `30m0s` | Maximum daemon-wide cooldown after an agent quota or session-limit error, as a Go duration such as `10m`, `30m`, or `1h` |

During cooldown:

- The agent is skipped for new jobs. CI comments show "skipped (quota)" for that agent instead of "failed".
- If a backup agent is configured (see [Backup Agents](/configuration/#backup-agents)) and is not also in cooldown, the job is retried with the backup agent automatically.
- Commit status is set to `success` when all panel members were skipped due to quota. This prevents quota exhaustion from blocking PRs.
- The cooldown timer resets each time the agent hits a quota error, but it is capped by `agent_quota_cooldown`. Provider reset hints can shorten the cooldown, not lengthen it beyond your configured cap.

No configuration is needed unless you want a different cap. Quota detection and cooldown are automatic. The daemon logs cooldown start and end events so you can monitor agent availability.

## CI Review (GitHub Actions)

The CI poller described above runs inside the roborev daemon as a background service. For teams that prefer a stateless, daemon-free approach, `roborev ci review` runs reviews directly inside a GitHub Actions workflow.

### When to Use Each Approach

| | Daemon CI Poller | `ci review` in GitHub Actions |
|---|---|---|
| **Runs on** | Your machine or server (daemon) | GitHub-hosted runners |
| **State** | SQLite database tracks reviewed SHAs | Stateless; runs on every trigger |
| **Setup** | `~/.roborev/config.toml` with `[ci]` | GitHub Actions workflow file |
| **Best for** | Continuous polling, centralized review | Per-PR checks, no infrastructure |

### Quickstart with `init gh-action`

The fastest way to set up CI reviews is with the workflow generator:

```bash
cd your-repo
roborev init gh-action --agent claude-code
```

This creates `.github/workflows/roborev.yml`. Commit and push it, then add your agent's API key as a repository secret.

### Required Repository Secrets

Each agent needs its API key as a repository secret:

| Agent | Secret Name |
|-------|-------------|
| Claude Code | `ANTHROPIC_API_KEY` |
| Codex | `OPENAI_API_KEY` |
| Gemini | `GOOGLE_API_KEY` |

Add secrets in your repository's **Settings > Secrets and variables > Actions**.

### How the Generated Workflow Works

The generated workflow triggers on `pull_request` events and:

1. Checks out the PR branch with full history
2. Downloads the pinned roborev binary and verifies its SHA256 checksum
3. Runs `roborev ci review --comment` with the configured agents
4. Posts review results as a PR comment

In GitHub Actions, `ci review` reads `GITHUB_REPOSITORY`, `GITHUB_REF`, and `GITHUB_EVENT_PATH` automatically, so no flags are needed beyond `--comment`.

### Customizing via `.roborev.toml`

The `ci review` command reads the repo's `.roborev.toml` for CI-specific settings:

```toml
# .roborev.toml
[ci]
review_types = ["security", "default"]
reasoning = "thorough"
min_severity = "medium"
```

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `review_types` | array | `["security"]` | Review types to run |
| `reasoning` | string | `"thorough"` | Reasoning level |
| `min_severity` | string | `"low"` | Minimum severity to include in output |

### Manual Workflow Setup

If you prefer full control over the workflow, create `.github/workflows/roborev.yml` manually:

```yaml
name: roborev
on:
  pull_request:
    types: [opened, synchronize]

permissions:
  contents: read
  pull-requests: write

jobs:
  review:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Install roborev
        run: |
          curl -fsSL https://roborev.io/install.sh | bash
          echo "$HOME/.roborev/bin" >> "$GITHUB_PATH"

      - name: Run review
        env:
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
        run: roborev ci review --comment --agent claude-code
```

Adjust the agent and secrets to match your setup. For multi-agent reviews, pass `--agent` multiple times or use `--review-types` to run different review types.

## See Also

- [Configuration](/configuration/): Global and per-repo settings
- [Event Streaming](/advanced/streaming/): Stream review events for custom integrations
