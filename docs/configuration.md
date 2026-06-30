---
title: Configuration
description: Configure roborev behavior globally and per-repository
---

roborev uses a layered configuration system. Settings are resolved in this order (highest to lowest priority):

1. **CLI flags** (`--agent`, `--model`, `--reasoning`)
2. **Per-repo** `.roborev.toml` in your repository root
3. **Global** `~/.roborev/config.toml`
4. **Defaults** (auto-detect agent, thorough reasoning for reviews)

## The `config` Command

The `roborev config` command lets you inspect and modify configuration from the command line, similar to `git config`. It works with both global and per-repo config files.

### Get a value

```bash
roborev config get default_agent              # merged: tries local, then global
roborev config get default_agent --global     # global config only
roborev config get review_agent --local       # repo config only
roborev config get sync.enabled               # nested keys use dot notation
```

Without `--global` or `--local`, `get` uses merged scope: it checks the repo config first, then falls back to global. The raw value is printed to stdout for easy piping.

### Set a value

```bash
roborev config set default_agent codex --global
roborev config set max_workers 8 --global
roborev config set review_agent claude-code    # defaults to --local
roborev config set sync.enabled true --global
roborev config set ci.repos "org/repo1,org/repo2" --global
```

Without `--global` or `--local`, `set` defaults to writing the repo config (`.roborev.toml`). You must be inside a git repository for local writes.

Values are automatically coerced to the correct type:

| Config type | Input format | Example |
|-------------|-------------|---------|
| string | as-is | `codex` |
| integer | decimal number | `8` |
| boolean | true/false, 1/0 | `true` |
| string array | comma-separated | `"org/repo1,org/repo2"` |

Writes are atomic (temp file + rename) and preserve file permissions: `0600` for global config (which may contain secrets) and `0644` for repo config.

!!! note
    Some keys are scoped to one file. For example, `server_addr` is global-only and cannot be set with `--local`. The command will tell you if a key doesn't belong in the scope you chose.

### List all values

```bash
roborev config list                    # merged config (effective values)
roborev config list --global           # global config only
roborev config list --local            # repo config only
roborev config list --show-origin      # show where each value comes from
```

The `--show-origin` flag adds a column showing whether each value comes from `global`, `local`, or `default`:

```
global  default_agent   codex
local   review_agent    claude-code
default max_workers     4
```

Sensitive values (API keys, database URLs) are automatically masked in list output, showing only the last 4 characters.

## Per-Repository Configuration

Create `.roborev.toml` in your repository root to customize behavior for that project.

```toml
agent = "gemini"                  # AI agent to use
model = "gemini-3-flash-preview"  # Model override for this repo
review_context_count = 5   # Recent reviews to include as context
display_name = "backend"   # Custom name shown in TUI (optional)
excluded_branches = ["wip", "scratch"]  # Branches to skip reviews on

# Reasoning levels: thorough, standard, fast
review_reasoning = "thorough"  # For code reviews (default: thorough)
refine_reasoning = "standard"  # For refine command (default: standard)

# Severity filtering
review_min_severity = "medium"  # Skip low-severity findings in reviews
fix_min_severity = "medium"     # Skip low-severity findings in fix
refine_min_severity = "medium"  # Skip low-severity findings in refine

# Session reuse (experimental)
reuse_review_session = true     # Resume prior agent sessions on same branch

# Auto-close passing reviews
auto_close_passing_reviews = true

# Project-specific review guidelines
review_guidelines = """
No database migrations needed - no production databases yet.
Prefer composition over inheritance.
All public APIs must have documentation comments.
"""

[kata_context]
mode = "current"   # off (default), current, or open
max_chars = 50000
```

### Per-Repository Options

| Option | Type | Description |
|--------|------|-------------|
| `agent` | string | AI agent to use for this repo |
| `model` | string | Model to use (overrides global `default_model`) |
| `display_name` | string | Custom name shown in TUI |
| `review_context_count` | int | Number of recent reviews to include as context |
| `excluded_branches` | array | Branches to skip automatic reviews on |
| `excluded_commit_patterns` | array | Commit message substrings to skip reviews on (case-insensitive) |
| `exclude_patterns` | array | Filenames or glob patterns to exclude from review diffs for this repo |
| `post_commit_review` | string | Post-commit hook behavior: `"commit"` (default) or `"branch"` |
| `hook_timeout_seconds` | int | Override the post-commit hook request timeout for this repo, in seconds. Useful for large repos where the daemon's enqueue git calls are slow. Read filesystem-only from this checkout's `.roborev.toml` (a linked worktree without its own file does not inherit the main checkout's value). Zero or negative values inherit the global / platform default |
| `auto_close_passing_reviews` | bool | Automatically close reviews that pass with no findings |
| `review_reasoning` | string | Reasoning level for reviews: thorough, standard, fast |
| `refine_reasoning` | string | Reasoning level for refine: thorough, standard, fast |
| `review_min_severity` | string | Minimum severity for reviews: `critical`, `high`, `medium`, or `low`. Cascades: CLI flag > repo config > global config |
| `fix_min_severity` | string | Minimum severity for `fix`: `critical`, `high`, `medium`, or `low` |
| `refine_min_severity` | string | Minimum severity for `refine`: `critical`, `high`, `medium`, or `low` |
| `reuse_review_session` | bool | (Experimental) Resume prior agent sessions on the same branch. See [Session Reuse](/guides/reviewing-code/#session-reuse) |
| `reuse_review_session_lookback` | int | Max recent session candidates to consider (default: unlimited). See [Session Reuse](/guides/reviewing-code/#session-reuse) |
| `review_agent_<level>` | string | Agent to use for reviews at specific reasoning level |
| `review_model_<level>` | string | Model to use for reviews at specific reasoning level |
| `refine_agent_<level>` | string | Agent to use for refine at specific reasoning level |
| `refine_model_<level>` | string | Model to use for refine at specific reasoning level |
| `fix_agent` | string | Agent to use for fix / analyze --fix |
| `fix_agent_<level>` | string | Agent to use for fix at specific reasoning level |
| `fix_model` | string | Model to use for fix / analyze --fix |
| `fix_model_<level>` | string | Model to use for fix at specific reasoning level |
| `fix_commit_author` | string | Author for fix-like commits, formatted as `Name <email>`. Applied directly to roborev-owned commits; prompt-only for foreground agent commits |
| `fix_commit_co_authored_by` | array | `Co-authored-by` trailers for fix-like commits, each formatted as `Name <email>`. Applied directly to roborev-owned commits; prompt-only for foreground agent commits |
| `security_agent` | string | Agent to use for `--type security` reviews |
| `security_agent_<level>` | string | Agent for security reviews at specific reasoning level |
| `security_model` | string | Model for security reviews |
| `security_model_<level>` | string | Model for security reviews at specific reasoning level |
| `design_agent` | string | Agent to use for `--type design` reviews |
| `design_agent_<level>` | string | Agent for design reviews at specific reasoning level |
| `design_model` | string | Model for design reviews |
| `design_model_<level>` | string | Model for design reviews at specific reasoning level |
| `backup_agent` | string | Fallback agent if primary is unavailable or fails |
| `review_backup_agent` | string | Fallback agent for reviews |
| `refine_backup_agent` | string | Fallback agent for refine |
| `fix_backup_agent` | string | Fallback agent for fix |
| `security_backup_agent` | string | Fallback agent for security reviews |
| `design_backup_agent` | string | Fallback agent for design reviews |
| `review_guidelines` | string | Project-specific guidelines for the reviewer |
| `review_guidelines_supersede_global` | bool | Use repo guidelines instead of appending global `review_guidelines` |
| `kata_context.mode` | string | Kata task context in review prompts: `off`, `current`, or `open`. See [Kata Integration](#kata-integration) |
| `kata_context.max_chars` | int | Maximum bytes of Kata issue context to include (default: `50000`) |
| `max_prompt_size` | int | Maximum prompt size in bytes for this repo (default: 200000) |
| `snapshot_dir` | string | Repo-relative directory for oversized diff snapshots (default: `.roborev`). See [Prompt Size Budget](#prompt-size-budget) |

### Fix Commit Metadata

Use `fix_commit_author` and `fix_commit_co_authored_by` to set author metadata on commits produced by fix-like workflows:

```toml
# .roborev.toml or ~/.roborev/config.toml
fix_commit_author = "Your Name <you@example.com>"
fix_commit_co_authored_by = [
  "Pair Reviewer <pair@example.com>",
]
```

Both keys are accepted in global and per-repo config. Repo config overrides global config independently per field, so a repo can override only the author and still inherit global co-authors. Set `fix_commit_author = ""` or `fix_commit_co_authored_by = []` in `.roborev.toml` to suppress a global value for that repo.

The identity format must be `Name <email>`. roborev validates this before invoking Git so bare names fail with a config error instead of Git interpreting them as commit search patterns.

roborev applies these values directly when it owns the commit:

- `roborev refine` commits
- background fix patches applied from the TUI

For foreground `roborev fix`, `roborev analyze --fix`, and batch fix flows, the agent creates the commit. roborev adds the configured author and trailer request to the agent prompt, but this is best-effort. It cannot remove trailers that the agent adds from its own Git configuration, so duplicate or unexpected `Co-authored-by` lines can still appear in agent-authored commits. Use `refine` or TUI-applied background fixes when deterministic trailers matter.

Only the commit author is overridden. The committer remains the user and environment running `roborev`.

`fix_commit_co_authored_by` uses `git commit --trailer`, which requires Git 2.32 or newer. `fix_commit_author` uses Git's long-standing `--author` option and is not gated on trailer support.

### Review Guidelines

Use `review_guidelines` to give the AI reviewer persistent context:
suppress irrelevant warnings, enforce conventions, or describe trust
boundaries and architecture so the reviewer doesn't flag non-issues.
Guidelines can be global, per-repo, or both:

```toml
# ~/.roborev/config.toml
review_guidelines = """
These rules apply to every repository on this machine.
Prefer clear error messages and avoid speculative findings.
"""
```

```toml
# .roborev.toml
review_guidelines = """
This is a local CLI tool. The daemon runs on localhost and all
displayed data originates from the user's own filesystem and git
repos. Do not flag injection or sanitization for data that never
crosses a trust boundary.

Performance is critical - flag any O(n^2) or worse algorithms.
All error messages must be user-friendly.
"""

# Optional: replace global review_guidelines for this repo instead of appending.
review_guidelines_supersede_global = false
```

When both scopes are configured, global guidelines are rendered first and
repo guidelines are appended after them. Set
`review_guidelines_supersede_global = true` in `.roborev.toml` when a repo
needs to replace the global rules entirely.

Guidelines are included in the review prompt for local and daemon review jobs,
so they shape what the reviewer flags and what it ignores. Common uses:

- **Trust boundaries**: Describe where untrusted data enters the
  system so the reviewer doesn't flag sanitization for trusted paths.
- **Architecture constraints**: Note that the daemon and client evolve
  in lockstep, that backward compatibility isn't required, etc.
- **Suppress noise**: Tell the reviewer to skip narrow-terminal
  overflow, localhost rate-limiting, or other non-issues for your
  project.

### Make roborev always flag something

Use `review_guidelines` in `.roborev.toml` to inject standing instructions into
every review for the repo:

```toml
review_guidelines = """
Every change to UI components must include or update a Playwright e2e test.
Flag any PR that changes UI without a corresponding e2e test.
"""
```

Because empty hooks are omitted from the generated config, you can add a
`[[hooks]]` block directly without removing anything:

```toml
[[hooks]]
event = "review.*"
type = "kata"
project = "myproj"
```

### Kata Integration

If your repo is bound to a [Kata](https://github.com/kenn-io/kata) project with a committed `.kata.toml`, roborev can include Kata task context in review prompts. For an overview of both directions of the integration, see [Kata](/integrations/kata/).

```toml
# .roborev.toml or ~/.roborev/config.toml
[kata_context]
mode = "current"   # off (default), current, or open
max_chars = 50000  # cap on Kata context bytes in the prompt
```

| Mode | Behavior |
|------|----------|
| `off` | Do not include Kata context (default) |
| `current` | Include only Kata issues referenced in the reviewed commit messages, such as `Closes: kata#abc4` or `<project>#abc4` |
| `open` | Include all open Kata issues from the bound project, excluding issues filed by roborev itself |

`current` mode frames referenced issues as authoritative task intent. `open` mode frames the open backlog as background context so the reviewer does not treat every open issue as part of the current change. Dirty reviews have no commit messages to inspect, so `current` mode includes no Kata context for them; use `open` if you want dirty reviews to see the backlog.

Kata context applies to local review prompts only (single commit, branch ranges, and dirty reviews). Daemon CI pull-request reviews never include Kata context, since PR head content is untrusted and backlog details could leak into public PR comments. Fix and task jobs do not receive Kata context either.

The `kata` CLI must be on `PATH`. If the CLI is missing or the repo is not bound to Kata, roborev skips the context without failing the review. If a binding exists but is broken, or a referenced issue cannot be loaded, the daemon logs the error and includes a note in the prompt when useful.

Kata context is optional prompt context. If the prompt exceeds `max_prompt_size`, roborev trims other optional context first and drops Kata context last.

To file failed reviews and review findings back into Kata, configure a `type = "kata"` review hook. See [Built-in: Kata Integration](/guides/hooks/#built-in-kata-integration).

### Workflow-Specific Agent and Model

Use different agents or models depending on the workflow and reasoning level:

```toml
# Use a faster model for quick reviews, thorough model for deep analysis
review_model = "claude"
review_model_fast = "claude-sonnet-4-5-20250929"
review_model_thorough = "claude-opus-4-5-20251101"

# Use different agents and models for refine
refine_agent_fast = "gemini"
refine_model_fast = "gemini-3-flash"
refine_agent_thorough = "codex"
refine_model_thorough = "gpt-5.5"
```

Base keys use the pattern `{workflow}_agent` and `{workflow}_model` (e.g. `review_model`, `refine_agent`, `fix_agent`, `security_agent`, `design_model`) to set the default for each workflow. Level-specific keys override for a given reasoning level:
- `{workflow}_model_{level}` or `{workflow}_agent_{level}`
- `workflow` is `review`, `refine`, `fix`, `security`, or `design`
- `level` is `thorough`, `standard`, or `fast`

The fallback hierarchy for each workflow is:
- **CLI flag** > **repo `{workflow}_agent_{level}`** > **repo `{workflow}_agent`** > **repo `agent`** > **global `{workflow}_agent_{level}`** > **global `{workflow}_agent`** > **global `default_agent`** > `codex`

#### Per-type `[analyze.<type>]` override

Some review types have no dedicated `{type}_agent` / `{type}_model` keys. Those can be pinned through a generic per-type block keyed by the type name, which sets `agent` and `model`. Today this applies to the `lookahead` review type:

```toml
[analyze.lookahead]
agent = "claude-code"
model = "sonnet"
```

For such a review type the fallback hierarchy is:
- **CLI flag** > **repo `[analyze.<type>]`** > **repo `agent` / `model`** > **global `[analyze.<type>]`** > **global `default_agent` / `default_model`** > `codex` for agent selection, or the agent's own default model

This block is consulted **only** for review types that lack dedicated fields. Types that have them — `review`, `refine`, `fix`, `security`, `design` — ignore `[analyze.<type>]` when running reviews and use their `{type}_agent` / `{type}_model` keys instead. So an `[analyze.security]` table written for `roborev analyze security` never changes `roborev review --type security`. Reasoning is unaffected by this block for reviews; it follows `review_reasoning` (the block's `reasoning` field applies only to `roborev analyze <type>` runs).

### Review Panels

Use `[review]` to configure subagent review panels. A panel fans one daemon review target out to named reviewers and stores one synthesis parent review:

```toml
[review]
default_panel = "branch_final"
hook_review_panel = "quick"

[review.subagents.bug]
agent = "codex"
review_type = "default"
instructions = "Focus on correctness, regressions, and missing tests."

[review.subagents.security]
agent = "claude-code"
review_type = "security"
allow_failure = true
timeout = "3m"

[review.panels.branch_final]
members = ["bug", "security"]
synthesis_agent = "codex"
```

`default_panel` applies to manual daemon reviews when `--panel` is not set. `hook_review_panel` applies to automatic post-commit reviews. CI reviews can select a named panel with `[ci] panel = "branch_final"`. Use `allow_failure = true` for flaky or best-effort subagents whose failure should not fail an otherwise successful panel.

Global and repo panel maps are merged by name, with repo entries overriding global entries. See [Subagent Review Panels](/advanced/subagent-review-panels/) for the full reference.

### Backup Agents

If the primary agent is unavailable (for example, its command is not visible to the daemon) or fails during execution (rate limits, network errors, crashes), roborev can use a configured backup agent. This is useful when your primary agent has usage caps. For example, Codex plans often hit rate limits during heavy review sessions, so falling back to Claude Code keeps reviews flowing.

```toml
# ~/.roborev/config.toml
default_agent = "codex"
default_backup_agent = "claude-code"  # Fallback for any workflow
default_backup_model = "claude-sonnet-4-20250514"  # Model for the backup agent
```

When a backup agent takes over, it uses the model specified by `default_backup_model`. Without this setting, the backup agent uses whatever model is configured for it normally. This is useful when your backup agent needs a different model than the one configured as `default_model` (which is typically chosen for the primary agent).

You can also set backup agents per workflow:

```toml
# ~/.roborev/config.toml
review_backup_agent = "claude-code"   # Fallback for reviews
refine_backup_agent = "codex"         # Fallback for refine
fix_backup_agent = "claude-code"      # Fallback for fix
```

Per-repo overrides work the same way in `.roborev.toml`:

```toml
# .roborev.toml
backup_agent = "claude-code"          # Repo-level fallback
review_backup_agent = "gemini"        # Workflow-specific override
```

When a workflow needs a backup agent, roborev resolves it in this order:

1. Repo-level workflow-specific backup (e.g. `review_backup_agent` in `.roborev.toml`)
2. Repo-level generic `backup_agent`
3. Global workflow-specific backup (e.g. `review_backup_agent` in `config.toml`)
4. Global `default_backup_agent`

If a backup agent is found and installed, roborev uses that agent instead. If no backup is configured or the backup agent isn't installed, the job fails normally. roborev does not choose unrelated installed agents from the built-in agent list for workflow-configured reviews or fixes.

### Agent Command Overrides

If an agent binary is installed under a non-standard name or path, use a `*_cmd` setting to tell roborev where to find it:

```toml
# ~/.roborev/config.toml
claude_code_cmd = "/opt/bin/claude"
codex_cmd = "codex-nightly"
gemini_cmd = "gemini"                 # Pin legacy Gemini CLI instead of auto-preferring agy
cursor_cmd = "/usr/local/bin/agent"
opencode_cmd = "/usr/local/bin/opencode-wrapper"
pi_cmd = "~/bin/pi"
```

| Option | Default command |
|--------|----------------|
| `claude_code_cmd` | `claude` |
| `codex_cmd` | `codex` |
| `gemini_cmd` | auto (`agy`, then `gemini`) |
| `cursor_cmd` | `agent` |
| `opencode_cmd` | `opencode` |
| `pi_cmd` | `pi` |

These overrides affect both agent execution and availability detection. Without them, roborev only checks for the default command name when deciding whether an agent is installed. Gemini is the exception: when `gemini_cmd` is unset, roborev auto-prefers `agy` before the legacy `gemini` command; set `gemini_cmd = "gemini"` to pin the legacy CLI, for example when using explicit Gemini model overrides.

### Agent Name Validation

Unknown agent names in configuration files and CLI flags are now validated and rejected early. If you specify an agent name that roborev doesn't recognize, you'll get a clear error message at startup or command invocation instead of a confusing failure later.

### Excluded Branches

Skip automatic reviews on work-in-progress branches:

```toml
excluded_branches = ["wip", "scratch", "experiment"]
```

Reviews triggered manually with `roborev review` still work on these branches.

### Excluded Commit Patterns

Skip reviews for commits whose messages contain specific substrings (case-insensitive matching):

```toml
excluded_commit_patterns = ["[skip review]", "wip:", "fixup!"]
```

When reviewing a range, the range is skipped only if every commit in the range matches. Manually triggered reviews with `roborev review` are not affected.

### Exclude Patterns

Common lockfiles and generated files are excluded from review diffs by default: `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `bun.lockb`, `bun.lock`, `uv.lock`, `poetry.lock`, `Pipfile.lock`, `pdm.lock`, `go.sum`, `Cargo.lock`, `Gemfile.lock`, `composer.lock`, `packages.lock.json`, `pubspec.lock`, `mix.lock`, `Package.resolved`, `Podfile.lock`, `flake.lock`, and the `.beads/`, `.gocache/`, and `.cache/` directories.

To exclude additional files, use `exclude_patterns` in either global or per-repo config:

```toml
# .roborev.toml
exclude_patterns = ["generated.go", "*.pb.go", "vendor/"]
```

```toml
# ~/.roborev/config.toml
exclude_patterns = ["*.min.js", "dist/"]
```

Patterns can be filenames, directory names (with trailing `/`), or glob patterns (including `**`). Both global and repo-level patterns are merged. User patterns are appended to the built-in exclusion list.

!!! note
    Security reviews (`--type security`) skip repo-level exclude patterns entirely. A compromised `.roborev.toml` on a branch cannot hide files from security review. Global-level patterns still apply.

### Prompt Size Budget

The `max_prompt_size` (per repo) and `default_max_prompt_size` (global) settings control the maximum size in bytes of the prompt sent to review agents. The per-repo value takes precedence over the global default. The default is 200,000 bytes (~200 KB).

```toml
# ~/.roborev/config.toml
default_max_prompt_size = 300000   # 300 KB global default

# .roborev.toml
max_prompt_size = 500000           # 500 KB for this repo only
```

This limit applies to all review commands (`review`, `compact`, `ci review`) and all agents. When a diff exceeds the budget, roborev writes the full diff to an external snapshot file in a per-snapshot directory and includes fallback instructions in the prompt pointing the agent at the snapshot path. Codex receives the snapshot directory via `--add-dir`. Optional context (prior reviews, guidelines) is trimmed first to preserve as much inline diff as possible. Final prompt size is checked before submission, and context-window failures fail or fail over rather than retrying the same oversized prompt.

By default, snapshots are written to `.roborev/` under the repo root so the agent sandbox does not need broader filesystem access. Override the location with `snapshot_dir` in `.roborev.toml`:

```toml
# .roborev.toml
snapshot_dir = ".cache/roborev"
```

`snapshot_dir` must be a relative path under the repo root, must not be inside `.git`, and must not contain control characters. `roborev init` ensures the configured directory is ignored in `.gitignore`; snapshot creation also writes a local `.git/info/exclude` fallback for existing checkouts whose ignore setup is stale.

### Post-Commit Review Mode

By default, the post-commit hook reviews the single commit at HEAD. Set `post_commit_review = "branch"` to review all commits since the branch diverged from the base branch instead:

```toml
post_commit_review = "branch"
```

When set to `"branch"`, each commit triggers a `merge-base..HEAD` range review covering the entire branch. On the base branch itself (e.g. `main`), detached HEAD, or any error, the hook falls back to a single-commit review.

This setting only affects the post-commit hook. `roborev review` is not changed by this option.

### Auto-Close Passing Reviews

By default, all reviews remain open in the queue until you explicitly close them. Set `auto_close_passing_reviews = true` to automatically close reviews that pass with no findings:

```toml
auto_close_passing_reviews = true
```

When enabled, reviews with a passing verdict are closed immediately after the verdict is parsed. Failed reviews remain open for attention. This is useful if you only want to focus on reviews that found issues.

The option works in both global config (`~/.roborev/config.toml`) and per-repo config (`.roborev.toml`). Per-repo settings override the global value.

## Global Configuration

Create `~/.roborev/config.toml` to set system-wide defaults.

```toml
default_agent = "codex"
default_model = "gpt-5.5"  # Default LLM
default_backup_model = "claude-sonnet-4-20250514"  # Fallback model for backup agent
server_addr = "127.0.0.1:7373"
max_workers = 4
job_timeout_minutes = 30          # Per-job timeout in minutes
hook_timeout_seconds = 30         # Post-commit hook request timeout (0 = platform default: 3, 30 on Windows)
agent_quota_cooldown = "30m"      # Maximum quota cooldown after agent limits
review_guidelines = "Global review instructions for every repo."
hide_closed_by_default = true     # Start TUI with closed/failed/canceled hidden
auto_filter_repo = true           # Auto-filter TUI to current repo on startup
auto_filter_branch = true         # Auto-filter TUI to current branch/worktree on startup
mouse_enabled = true              # Enable mouse interactions in the TUI
tab_width = 4                     # Tab expansion width for code blocks in TUI (default: 2)
column_borders = true             # Show separators between TUI columns
```

### Global Options

| Option | Type | Default | Description | Hot-Reload |
|--------|------|---------|-------------|------------|
| `default_agent` | string | auto-detect | Default AI agent to use | Yes |
| `default_backup_agent` | string | - | Fallback agent when the primary is unavailable or fails | Yes |
| `default_backup_model` | string | - | Fallback model used when a backup agent runs | Yes |
| `default_model` | string | agent default | Model to use (format varies by agent) | Yes |
| `server_addr` | string | 127.0.0.1:7373 | Daemon listen address. Use `unix://` for Unix domain socket (see [Unix Domain Socket](#unix-domain-socket)) | No |
| `max_workers` | int | 4 | Number of parallel review workers | No |
| `job_timeout_minutes` | int | 30 | Per-job timeout in minutes | Yes |
| `hook_timeout_seconds` | int | `3` (`30` on Windows) | Post-commit hook request timeout, in seconds. Raise it on Windows or large repos where the daemon's enqueue git calls are slow. Zero or negative values are ignored and fall back to the platform default | Yes |
| `agent_quota_cooldown` | string | `30m0s` | Maximum daemon-wide cooldown after an agent quota or session-limit error, as a Go duration such as `10m`, `30m`, or `1h` | Yes |
| `allow_unsafe_agents` | bool | false | Enable agentic mode globally | Yes |
| `anthropic_api_key` | string | - | Anthropic API key for Claude Code | Yes |
| `review_context_count` | int | 3 | Recent reviews to include as context | Yes |
| `review_guidelines` | string | - | Global reviewer instructions included in review prompts for every repo | Yes |
| `reuse_review_session` | bool | false | (Experimental) Resume prior agent sessions on the same branch. See [Session Reuse](/guides/reviewing-code/#session-reuse) | Yes |
| `reuse_review_session_lookback` | int | 0 | Max recent session candidates to consider (0 = unlimited) | Yes |
| `auto_close_passing_reviews` | bool | false | Automatically close reviews that pass with no findings | Yes |
| `kata_context.mode` | string | `off` | Kata task context in review prompts: `off`, `current`, or `open` | Yes |
| `kata_context.max_chars` | int | `50000` | Maximum bytes of Kata issue context to include | Yes |
| `review_min_severity` | string | - | Default minimum severity for reviews: `critical`, `high`, `medium`, or `low` | Yes |
| `fix_min_severity` | string | - | Default minimum severity for `fix`: `critical`, `high`, `medium`, or `low` | Yes |
| `refine_min_severity` | string | - | Default minimum severity for `refine`: `critical`, `high`, `medium`, or `low` | Yes |
| `disable_codex_sandbox` | bool | false | Disable Codex `bwrap` sandboxing for systems where it is unavailable | Yes |
| `hide_closed_by_default` | bool | false | Start TUI with closed/failed/canceled reviews hidden | N/A |
| `auto_filter_repo` | bool | false | Auto-filter TUI to the current repo on startup | N/A |
| `auto_filter_branch` | bool | false | Auto-filter TUI to the current branch/worktree on startup | N/A |
| `mouse_enabled` | bool | true | Enable mouse interactions in the TUI (also togglable from the TUI options menu) | N/A |
| `tab_width` | int | 2 | Tab expansion width for code blocks in TUI (1-16) | N/A |
| `column_borders` | bool | false | Show `▕` separators between TUI columns | N/A |
| `column_order` | array | | Custom queue column display order | N/A |
| `task_column_order` | array | | Custom task column display order | N/A |
| `claude_code_cmd` | string | `claude` | Custom path or name for the Claude Code binary | Yes |
| `codex_cmd` | string | `codex` | Custom path or name for the Codex binary | Yes |
| `gemini_cmd` | string | unset | Custom path or name for the Gemini-compatible binary; unset auto-prefers `agy` then `gemini` | Yes |
| `cursor_cmd` | string | `agent` | Custom path or name for the Cursor binary | Yes |
| `opencode_cmd` | string | `opencode` | Custom path or name for the OpenCode binary | Yes |
| `pi_cmd` | string | `pi` | Custom path or name for the Pi binary | Yes |
| `exclude_patterns` | array | `[]` | Filenames or glob patterns to exclude from review diffs globally | Yes |
| `default_max_prompt_size` | int | 200000 | Default maximum prompt size in bytes for review prompts | Yes |

!!! note
    The previous config key `hide_addressed_by_default` is still read as a fallback. If you have it set and `hide_closed_by_default` is not present, the old value is used automatically. No action is required, but new configs should use the new name.

### Hot-Reload

The daemon automatically watches `~/.roborev/config.toml` for
changes. Most settings take effect immediately without restarting the
daemon.

**Settings that require daemon restart:** `server_addr`, `max_workers`, and the `[sync]` section.

### Data Directory

All roborev data is stored in `~/.roborev/` by default:

```
~/.roborev/
├── config.toml       # Global configuration
├── daemon.json       # Runtime state (port, PID)
├── post-commit.log   # JSONL log of post-commit hook invocations
├── reviews.db        # SQLite database
└── logs/jobs/        # Persistent job output logs
```

Override with the `ROBOREV_DATA_DIR` environment variable:

```bash
export ROBOREV_DATA_DIR=/custom/path
```

### Unix Domain Socket

On Unix systems, the daemon can listen on a Unix domain socket instead of TCP loopback. This provides filesystem-level access control: the socket is created with `0600` permissions and its parent directory with `0700`, so only the owning user can connect.

To enable Unix domain sockets, set `server_addr` to `unix://`:

```toml
# ~/.roborev/config.toml
server_addr = "unix://"
```

With `unix://` (no path), the socket is created at `$XDG_RUNTIME_DIR/roborev/daemon.sock` when `$XDG_RUNTIME_DIR` is set and points to an existing absolute directory. Otherwise, the socket is placed under the platform temp directory (e.g. `/tmp` on Linux, `/var/folders/.../T` on macOS) at `{tempdir}/roborev-{UID}/daemon.sock`, where `{UID}` is your numeric user ID. To use a specific path:

```toml
server_addr = "unix:///var/run/roborev/daemon.sock"
```

The CLI flag works the same way:

```bash
roborev --server unix:// tui
roborev daemon run --addr "unix:///custom/path.sock"
```

Stale socket files from previous daemon runs are cleaned up automatically on startup. The socket is removed on graceful shutdown.

!!! note
    Unix domain sockets are not supported on Windows. Socket path length is limited to 104 bytes on macOS and 108 bytes on Linux.

### Persistent Daemon

The daemon starts automatically when you run `roborev init` or any command that needs it, and stays running in the background. This is sufficient for most users. If you want the daemon to survive reboots, restart on failure, or be managed alongside other system services, set up a system service.

!!! warning "Use `--no-daemon` with system services"
    If you manage the daemon with systemd or launchd, use `roborev init --no-daemon` when registering repos. Otherwise, `roborev init` auto-starts a background daemon that conflicts with the service-managed one.

**macOS (launchd):**

```bash
# Resolve paths. Homebrew prefix differs between Intel and Apple Silicon
ROBOREV_BIN="$(command -v roborev)"
BREW_PREFIX="$(brew --prefix 2>/dev/null || echo /usr/local)"

mkdir -p ~/Library/Logs/roborev
cat > ~/Library/LaunchAgents/com.roborev.daemon.plist << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.roborev.daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>${ROBOREV_BIN}</string>
        <string>daemon</string>
        <string>run</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>${BREW_PREFIX}/bin:${BREW_PREFIX}/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>${HOME}/Library/Logs/roborev/daemon.log</string>
    <key>StandardErrorPath</key>
    <string>${HOME}/Library/Logs/roborev/daemon.log</string>
</dict>
</plist>
EOF

launchctl load ~/Library/LaunchAgents/com.roborev.daemon.plist
```

The heredoc expands `${ROBOREV_BIN}`, `${BREW_PREFIX}`, and `${HOME}` at generation time so the plist contains absolute paths. Launchd does not expand `~` or environment variables in plist values.

**Linux (systemd):**

roborev ships with `roborev.service` and `roborev.socket` unit files in its `packaging/systemd/` directory. If your package manager installed the unit files, enable the user-level service:

```bash
systemctl --user enable --now roborev
```

For on-demand startup via socket activation, enable the socket unit instead. The daemon starts automatically when a client connects and uses `Type=notify` to signal readiness back to systemd:

```bash
systemctl --user enable --now roborev.socket
```

If the unit files are not available (e.g., you used `go install` or the install script), create a service unit manually. This covers the always-on service mode; socket activation requires the bundled unit files.

```bash
# Resolve the actual binary path for ExecStart
ROBOREV_BIN="$(command -v roborev)"

mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/roborev.service << EOF
[Unit]
Description=roborev daemon
After=network.target

[Service]
ExecStart=${ROBOREV_BIN} daemon run
Restart=on-failure

[Install]
WantedBy=default.target
EOF

systemctl --user enable --now roborev
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `ROBOREV_DATA_DIR` | Override default data directory (`~/.roborev`) |
| `ROBOREV_COLOR_MODE` | Color theme: `auto` (default), `dark`, `light`, `none`. See [Color Mode](#color-mode) |
| `ROBOREV_TELEMETRY_ENABLED` | Set to `0` to disable anonymous daemon telemetry |
| `TELEMETRY_ENABLED` | Generic telemetry opt-out. Set to `0` to disable telemetry |
| `NO_COLOR` | Set to any value to disable all color output ([no-color.org](https://no-color.org)) |

### Telemetry

roborev sends limited anonymous telemetry when the daemon starts and once every 24 hours while it remains running. Events are `daemon_started` and `daemon_active`, with repo count, review count, whether sync is enabled, whether CI is enabled, whether auto design review is enabled, roborev version, OS, architecture, and an anonymous install ID stored in the local database.

Telemetry does not include repo names, paths, remotes, prompts, review output, provider tokens, usernames, or IP geolocation. Disable it with either environment variable:

```bash
ROBOREV_TELEMETRY_ENABLED=0 roborev daemon run
TELEMETRY_ENABLED=0 roborev daemon run
```

Telemetry is disabled in Go test processes.

### Color Mode

The TUI automatically adapts to light and dark terminals by default. Use `ROBOREV_COLOR_MODE` to override the auto-detection:

| Value | Behavior |
|-------|----------|
| `auto` | Detect terminal background color (default) |
| `dark` | Force dark color palette |
| `light` | Force light color palette |
| `none` | Strip all colors (equivalent to `NO_COLOR=1`) |

```bash
ROBOREV_COLOR_MODE=dark roborev tui
```

`NO_COLOR` takes precedence over `ROBOREV_COLOR_MODE` at all layers. When `NO_COLOR` is set, all ANSI color sequences are stripped regardless of `ROBOREV_COLOR_MODE`.

### Model Selection

The `default_model` setting specifies which model agents should use. The format varies by agent:

```toml
# OpenAI models (Codex, Copilot)
default_model = "gpt-5.5"

# Anthropic models (Claude Code)
default_model = "claude-opus-4-8"

# OpenCode (provider/model format)
default_model = "anthropic/claude-opus-4-8"
```

### Cost Usage Endpoint

By default, roborev looks up token usage and cost estimates through the local `agentsview` CLI. You can route lookup through an HTTP endpoint instead:

```toml
[cost]
endpoint = "https://usage.example.test/api/v1/sessions/{session_id}/usage"
timeout = "2s"
```

`endpoint` must include `{session_id}`, which roborev URL-escapes and replaces with the agent session ID. If `endpoint` is empty, roborev uses the `agentsview` CLI path. `timeout` defaults to `10s`; invalid, zero, or negative values also fall back to `10s`.

The endpoint should return JSON compatible with `agentsview session usage --format json`:

```json
{
  "session_id": "session-123",
  "agent": "codex",
  "project": "myrepo",
  "total_output_tokens": 28800,
  "peak_context_tokens": 118000,
  "has_token_data": true,
  "cost_usd": 0.42,
  "has_cost": true
}
```

`has_token_data` and `has_cost` are required booleans. When `has_token_data` is true, token counts are required. When `has_cost` is true, `cost_usd` is required. A `404` response is treated as no usage data for that session.

### Agentic Mode

Enable agentic mode globally (allows agents to edit files and run commands):

```toml
allow_unsafe_agents = true
```

!!! warning
    This makes all review operations potentially write to your codebase. Use with caution. It's generally safer to enable agentic mode per-job with `--agentic`.

### Advanced Section

The `[advanced]` section controls opt-in features that are not part of the default workflow.

```toml
# ~/.roborev/config.toml
[advanced]
tasks_enabled = true   # Enable background tasks in the TUI
```

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `tasks_enabled` | bool | false | Enable the TUI background tasks workflow (fix jobs, patch application, rebasing) |

When enabled, the TUI exposes the `F` (fix) and `T` (tasks) shortcuts for launching fix jobs and managing patches. See [Background Tasks](/advanced/background-tasks/) for the full reference.

### Codex Review Options

The `[agent.codex]` section controls Codex-specific behavior for review jobs. Both options default to `true` so Codex reviews start from a clean state without skill instructions or user-level Codex config. Fix jobs are not affected.

```toml
# ~/.roborev/config.toml
[agent.codex]
disable_review_skills = true       # Suppress Codex skill instructions on review jobs
ignore_review_user_config = true   # Pass --ignore-user-config to Codex review jobs
```

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `disable_review_skills` | bool | true | Run Codex review jobs with `skills.include_instructions=false` so model-visible skill instructions are stripped from the prompt |
| `ignore_review_user_config` | bool | true | Pass `--ignore-user-config` to Codex review jobs so user-level Codex config is not loaded |

Set either to `false` to restore the prior behavior if you rely on Codex skill instructions or user-level Codex config during reviews.

#### Custom Codex Config (Model Providers)

`ignore_review_user_config` stops Codex review jobs from loading your
`~/.codex/config.toml`, which also means a custom `model_provider` and its
`[model_providers.*]` block defined there are not applied. To inject specific
Codex options without loading your full user config, define an
`[agent.codex.config]` table. Every key under it is passed to Codex as a
`-c key=value` override on **every** Codex job (review, fix, refine, analyze),
and these overrides apply even when `--ignore-user-config` is in effect.

The table mirrors Codex's own `config.toml`, so you can declare a custom
provider and select it:

```toml
# ~/.roborev/config.toml
[agent.codex]
ignore_review_user_config = true   # safe to leave enabled

[agent.codex.config]
model_provider = "my-custom"

[agent.codex.config.model_providers.my-custom]
name = "My Provider"
base_url = "https://api.example.com/v1"
env_key = "MY_API_KEY"
wire_api = "responses"
```

roborev flattens that into `-c model_provider="my-custom"`,
`-c model_providers.my-custom.base_url="https://api.example.com/v1"`, and so on,
one override per leaf key. Values keep their TOML type: strings stay quoted,
numbers and booleans stay bare, and arrays stay arrays. roborev's own review
controls (skill suppression, sandbox mode, reasoning effort) are applied after
your overrides, so they take precedence if a key collides.

### Pi Classifier Options

Pi can be used as the auto design-review classifier because roborev runs Pi with a JSON schema output extension. The default extension source is `npm:@nqbao/pi-json-schema@0.1.1`.

Override the extension source under `[agent.pi]` if you vendor or mirror the extension:

```toml
[agent.pi]
jsonschemaextension = "/opt/roborev/pi-json-schema/index.ts"
```

Install the default extension in Pi so classifier setup is visible to `pi list` and so locked-down environments do not need to fetch it at runtime:

```bash
pi install npm:@nqbao/pi-json-schema
```

## Hooks

Run shell commands when reviews complete or fail. See the [Review Hooks guide](/guides/hooks) for full details.

```toml
# In config.toml or .roborev.toml
[[hooks]]
event = "review.failed"          # or "review.completed", "review.*"
command = "notify-send 'Review failed for {repo_name}'"

[[hooks]]
event = "review.failed"
type = "beads"   # Built-in: creates a bd issue automatically

[[hooks]]
event = "review.*"
branches = ["main", "release/*"]
type = "kata"    # Built-in: creates Kata issues for failures/findings
```

### Hook Options

| Option | Type | Description |
|--------|------|-------------|
| `event` | string | Event pattern to match, such as `review.completed`, `review.failed`, or `review.*` |
| `branches` | array | Optional branch allowlist using `path.Match` globs. Empty means all branches |
| `command` | string | Shell command with `{var}` template interpolation |
| `type` | string | Built-in hook type (`beads`, `kata`, `webhook`), or omit for custom command |
| `url` | string | Webhook destination URL (required when `type = "webhook"`) |
| `project` | string | Kata project override for `type = "kata"` |
| `labels` | array | Extra Kata labels for `type = "kata"`; `roborev` is always added |
| `priority` | int | Kata issue priority for `type = "kata"` |

Template variables: `{job_id}`, `{repo}`, `{repo_name}`, `{sha}`, `{agent}`, `{verdict}`, `{findings}`, `{error}`

## Auto Design Review

Off by default. When enabled, roborev decides per commit whether to dispatch a `--type design` review on top of the normal code review. The router uses cheap heuristics first (path globs, diff size, file count, commit-subject regexes) and falls back to a JSON-schema-constrained classifier for ambiguous cases. With `enabled = true`, the post-commit, `roborev review`, range, and dirty paths all consult the router, as does the CI poller when `design` is not already in the configured panel or review matrix. When the router decides not to run, a skipped row is recorded with a short reason and rendered dimmed in the TUI; PR synthesis includes a one-line `Auto-design-review skipped: <reason>` section.

By default, the TUI queue hides the auto-design-router's classifier jobs (`job_type = classify`) and skipped design rows (`status = skipped`, scoped to `source = auto_design`) so per-commit routing decisions do not crowd out review rows. Decisions are still recorded and counted on the daemon status endpoint. Press `s` in the TUI to toggle visibility for the current session, or set `show_classify_jobs = true` in `~/.roborev/config.toml` to make them visible globally; per-repo `.roborev.toml` can override with a nullable `show_classify_jobs` field (omit to inherit). When viewing a hidden classifier or skipped row, press `l` to see the classifier verdict and `skip_reason` rendered above the (typically empty) log.

### Enabling

Turn it on globally in `~/.roborev/config.toml`:

```toml
[auto_design_review]
enabled = true
```

To run the router only for automatic post-commit hook reviews, use `hook_enabled` instead:

```toml
[auto_design_review]
hook_enabled = true
```

`hook_enabled = true` does not affect manual `roborev review`, dirty/range reviews, or CI. Use `enabled = true` when those entry points should consult the router too.

A per-repo `[auto_design_review]` block in `.roborev.toml` overrides the global values. The per-repo `enabled` and `hook_enabled` fields are tri-state: omitting a field inherits the global setting, `false` explicitly opts a repo out of a globally-enabled default, and `true` opts a repo in when the global default is off.

```toml
# .roborev.toml
[auto_design_review]
enabled = false   # opt this repo out of a globally-enabled router
hook_enabled = true  # still allow post-commit hook routing for this repo
```

### Heuristics

The router checks rules in fixed order: trigger paths, large diff, large file count, trigger message, then skip rules (trivial diff, all-files-skip, skip message). The first match wins; ambiguous commits go to the classifier.

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `enabled` | bool | false | Enable auto-design routing for post-commit hooks, manual reviews, dirty/range reviews, and CI |
| `hook_enabled` | bool | false | Enable auto-design routing only for post-commit hook reviews |
| `min_diff_lines` | int | 10 | Diffs below this changed-line count are skipped automatically |
| `large_diff_lines` | int | 500 | Diffs at or above this line count trigger a design review automatically |
| `large_file_count` | int | 10 | Commits touching at least this many files trigger automatically |
| `trigger_paths` | array | see below | Doublestar globs; any changed file matching triggers a design review |
| `skip_paths` | array | see below | Doublestar globs; if every changed file matches, the commit is skipped |
| `trigger_message_patterns` | array | see below | Regexes over the commit subject; a match triggers a design review |
| `skip_message_patterns` | array | see below | Regexes over the commit subject; a match skips the design review |
| `classifier_timeout_seconds` | int | 60 | Per-classify-job timeout |
| `classifier_max_prompt_size` | int | 20480 | Cap on classifier prompt size in bytes |

Default `trigger_paths`:

```
**/migrations/**
**/schema/**
**/*.sql
docs/superpowers/specs/**
docs/design/**
docs/plans/**
**/*-design.md
**/*-plan.md
```

Default `skip_paths`:

```
**/*.md
**/*_test.go
**/*.spec.*
**/testdata/**
```

Default `trigger_message_patterns`:

```
\b(refactor|redesign|rewrite|architect|breaking)\b
```

Default `skip_message_patterns`:

```
^(docs|test|style|chore)(\(.+\))?:
```

List fields replace defaults wholesale. Setting an empty list (e.g. `trigger_paths = []`) disables that family of heuristics for the repo. Unset list fields inherit the next layer's value.

Invalid globs and uncompilable regexes fail validation at startup so config typos surface loudly instead of silently suppressing every dispatch.

### Classifier

When heuristics are inconclusive, the router enqueues a `classify` job that runs the configured classifier agent against an embedded JSON schema. The agent returns a yes/no decision plus a short reason, and the router promotes the row to a design-review job or marks it skipped accordingly.

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `classify_agent` | string | `claude-code` | Agent for the routing classifier. Must implement structured-output (`SchemaAgent`) capability |
| `classify_model` | string | agent default | Model for the classifier agent |
| `classify_reasoning` | string | `fast` | Reasoning level: `fast`, `standard`, `medium`, `thorough`, or `maximum` |
| `classify_backup_agent` | string | - | Fallback classifier agent on quota exhaustion or failure |
| `classify_backup_model` | string | - | Fallback classifier model |

Currently `claude-code` (via `--json-schema`) and `pi` (via the configured JSON schema extension) implement the structured-output capability. Other agents are rejected at config-resolve time with a list of valid choices.

These keys live at the top level of the config file (not inside `[auto_design_review]`), since they describe the classifier agent the same way `review_agent` and `fix_agent` describe their workflows:

```toml
# ~/.roborev/config.toml
classify_agent = "claude-code"
classify_model = "claude-opus-4-8"
classify_reasoning = "fast"

[auto_design_review]
enabled = true
```

### CI integration

When the CI poller picks up a PR, it runs the auto design-review router on the head SHA when `design` is not already in the configured panel or review matrix. If heuristic inputs (diff, changed files, commit message) fail to assemble, the commit degrades to the classifier instead of being silently skipped.

The daemon `/api/status` endpoint exposes a `SkippedJobs` aggregate count, plus an `auto_design` subobject with five per-outcome counters (`triggered`, `skipped_heuristic`, `triggered_classifier`, `skipped_classifier`, `errored`) when the feature is enabled anywhere. The subobject is omitted from the JSON when the feature is disabled across all repos.

## Reasoning Levels

Reasoning levels control how deeply the AI analyzes code.

| Level | Description | Best For |
|-------|-------------|----------|
| `maximum` | Deepest analysis; maps to Codex `xhigh` reasoning | Complex reviews requiring maximum depth |
| `thorough` | Deep analysis with extended thinking | Code reviews (default) |
| `standard` | Balanced analysis | Refine command (default) |
| `fast` | Quick responses | Rapid feedback |

`maximum` is accepted as `max` or `xhigh` on the command line. For agents without an xhigh equivalent (Droid, Kilo, Pi), it maps to their highest available level (same as thorough).

Set per-command with `--reasoning`, or per-repo in `.roborev.toml`:

```bash
roborev review --reasoning fast      # Quick review
roborev refine --reasoning thorough  # Careful fixes
```

## Authentication

### Claude Code

Claude Code uses your Claude subscription by default. roborev
deliberately ignores `ANTHROPIC_API_KEY` from the environment to avoid
unexpected API charges.

To use Anthropic API credits instead of your subscription:

```toml
# ~/.roborev/config.toml
anthropic_api_key = "sk-ant-..."
```

roborev automatically sets `CLAUDE_NO_SOUND=1` when running Claude
agents to suppress notification and completion sounds.

To route Claude Code through a local or remote proxy (Ollama, LiteLLM,
LM Studio, etc.) instead of Anthropic's API, use the
`<model>@<base_url>` model spec. See
[Routing Claude Code to a Proxy](/agents/#routing-claude-code-to-a-proxy).

### Other Agents

- **Codex**: Uses authentication from `codex auth`
- **Gemini**: Uses authentication from Gemini CLI
- **Copilot**: Uses GitHub Copilot subscription via GitHub CLI
- **OpenCode**: Uses API keys configured in its own settings, set
    model to use in config TOML files

### Environment Variable Expansion

Sensitive values can reference environment variables:

```toml
# ~/.roborev/config.toml
anthropic_api_key = "${ANTHROPIC_API_KEY}"
```

This keeps secrets out of config files while still allowing roborev to use them.
