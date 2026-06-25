# roborev Onboarding Revamp — Design

Date: 2026-06-25
Status: Approved (pending spec review)

## Motivation

Feedback from a new-but-sophisticated user, plus a maintainer goal of making
roborev's automation legible to coding agents:

- Hooks / "the magic automation bits" were the first thing the user looked for
  and were hard to find. People want the hands-off path front and center.
- The generated `~/.roborev/config.toml` contains `hooks = []`. Adding a
  `[[hooks]]` array-of-tables block at the bottom then fails to parse because a
  key cannot be both a value array and an array-of-tables.
- Users want a known-good CLAUDE.md snippet to make their agent commit early and
  often (so roborev has frequent commits to review).
- Automation "kicks in" more reliably from the Claude Code CLI than Claude
  Desktop, and it is not documented why.
- Users want a clear answer to "how do I make roborev always flag X" (e.g.
  require e2e Playwright tests) via configuration.
- The TUI hero image does not convey how roborev works; a concept diagram would.
- Agents should be able to be told "run this / read this" and then understand
  roborev well enough to assist the user with configuration (.roborev.toml,
  CLAUDE.md / AGENTS.md).

## Goals

1. Ship a `roborev quickstart` command: a repo-aware, read-only, agent-oriented
   guide an agent reads to understand roborev and help configure the user's repo.
2. Fix the `hooks = []` collision and make `[[hooks]]` discoverable.
3. Restructure docs so the automation path (post-commit reviews + agent hook) is
   front and center, with precise terminology.
4. Add the missing guidance: CLAUDE.md commit-early snippet, review-tuning
   ("always flag X"), and a CLI-vs-Desktop explanation.
5. Add one canonical "how roborev works" concept SVG used on the docs homepage
   and the README.

## Non-Goals

- No changes to review logic, agents, daemon protocol, or storage.
- No interactive/agentic config writing inside `quickstart` — it prints; the
  calling agent makes edits.
- No regeneration of the TUI screenshot pipeline; the TUI shot is demoted, not
  removed.

## Phasing

1. **Phase 1 — CLI + config:** `hooks = []` fix and `roborev quickstart` (+ tests).
2. **Phase 2 — Docs:** nav restructure, new automation page, content additions.
3. **Phase 3 — Visual:** new concept SVG wired into homepage and README.

Each phase is independently committable and reviewable.

---

## Phase 1a — Fix `hooks = []` collision

### Problem

`Config.Hooks` (`internal/config/config.go:226`) and `RepoConfig.Hooks`
(`internal/config/config.go:421`) are tagged `toml:"hooks"` with no `omitempty`.
`SaveGlobalTo` / repo save marshal via `tomlv2.Marshal` (`config.go:1217`,
`:1259`), emitting `hooks = []` for the common empty case. A user who appends:

```toml
[[hooks]]
event = "review.*"
type = "kata"
```

then hits a TOML parse error: `hooks` is already a value array.

### Fix

1. Add `,omitempty` to both tags:
   - `config.go:226` → `toml:"hooks,omitempty"`
   - `config.go:421` → `toml:"hooks,omitempty"`
   Empty slices are then omitted from marshaled output, so no `hooks = []`.

2. **Discoverability, scoped correctly.** Do NOT append a commented example
   inside `SaveGlobalTo` — it runs on every rewrite and would resurrect comments
   a user deleted. Instead, append a commented `[[hooks]]` example block ONLY
   when creating the global config for the first time.

   Implementation: a dedicated first-creation writer that accepts the prepared
   config (so `roborev init --agent ...` keeps working — today init builds
   `DefaultConfig()`, sets `cfg.DefaultAgent = agent`, then saves):

   ```
   func WriteDefaultGlobalConfigTo(path string, cfg *Config) error
   ```

   It must preserve `SaveGlobalTo`'s durability guarantees: atomic write
   (temp file + rename) and `0600` file permissions, since the global config can
   later hold sensitive values (e.g. webhook URLs). It is used ONLY on
   first-time creation. `roborev init` creates `~/.roborev/config.toml` when
   absent (`init_cmd.go` step 3); route that creation through this writer,
   passing the already-prepared config (with `DefaultAgent` applied when
   `--agent` is given). Normal rewrites continue through `SaveGlobalTo`.

   The writer marshals the config (now without `hooks = []`) and appends a
   trailing commented block:

   ```toml
   # To run a command or built-in integration when reviews complete, add hooks:
   #
   # [[hooks]]
   # event = "review.failed"
   # command = "notify-send 'roborev: review failed for {repo_name}'"
   #
   # [[hooks]]
   # event = "review.*"
   # type = "kata"
   # project = "myproj"
   ```

   Normal saves (`SaveGlobalTo`) remain comment-free and stable.

### Tests (`internal/config/config_test.go`)

- Empty `Hooks` marshals without a `hooks` key (both global and repo configs).
- A config with a non-empty `[[hooks]]` round-trips (marshal → parse → equal).
- A config file containing both a hand-added `[[hooks]]` block and no leftover
  `hooks = []` parses cleanly (regression for the reported error).
- `WriteDefaultGlobalConfigTo` output parses cleanly and contains the commented
  example; `SaveGlobalTo` output does NOT contain the commented example.
- `WriteDefaultGlobalConfigTo` writes with `0600` permissions and replaces any
  existing file atomically.

---

## Phase 1b — `roborev quickstart` command

New file `cmd/roborev/quickstart_cmd.go`, registered in `main.go`.

### Contract

- **Read-only.** Detection must never mutate state. Specifically:
  - Use read-only daemon probing: `getDaemonEndpoint()` +
    `probeDaemonWithRetry()` (`cmd/roborev/daemon_lifecycle.go:94,145`),
    returning `*daemon.PingInfo`. Do NOT call `ensureDaemon()`
    (`daemon_lifecycle.go:225`, used by `status.go:31`), which can start/restart
    the daemon.
  - Resolve the hooks directory with the read-only `gitrepo.HooksPath` resolver
    (wrapped in `cmd/roborev/helpers.go`), which honors linked worktrees and
    `core.hooksPath`; then read `<hooksDir>/post-commit` and check for the
    roborev marker. Do NOT read `<repo>/.git/hooks/post-commit` directly (misses
    worktrees / `core.hooksPath`), and do NOT call the mutating
    `EnsureAbsoluteHooksPath` (referenced near `init_cmd.go:77`).
  - Read agent-hook installation from `~/.claude/settings.json` and
    `~/.codex/hooks.json` without installing.
  - Load config via the existing read-only `config.Load` path.
- **Prints only.** No edits to any file. The calling agent performs edits.

### Output: two parts

**Part 1 — Detected state** (repo-aware checklist). Whether the command is
inside a git repo is reported as the top-level `in_git_repo` field, not as a
check row (so it never appears as a `missing` check needing a command). Every
check row carries a runnable `fix_command` when `missing` — no bare
instructions. The eight checks, with stable IDs:

| Check `id` | Detection | `fix_command` when missing |
|------------|-----------|----------------------------|
| `daemon_running` | `probeDaemonWithRetry` ping | `roborev daemon start` |
| `post_commit_hook` | `gitrepo.HooksPath` → read `post-commit` marker | `roborev install-hook` |
| `repo_registered` | `GET /api/repos/resolve?path=<repoRoot>` (read-only) | `roborev init` |
| `repo_config` | stat `.roborev.toml` at repo root | `roborev init --agent <agent>` |
| `configured_agent` | parse `.roborev.toml` / global | `roborev config set --local agent <agent>` |
| `agent_hook_claude` | read `~/.claude/settings.json` | `roborev agent-hook install --agent claude` |
| `agent_hook_codex` | read `~/.codex/hooks.json` | `roborev agent-hook install --agent codex` |
| `skills_installed` | skills discovery (read-only) | `roborev skills install` |

The repo-dependent checks (`post_commit_hook`, `repo_registered`,
`repo_config`, `configured_agent`) report `status: "unknown"` when not inside a
git repo.

`fix_command` is always emitted fully substituted — never with a literal
`<agent>` placeholder (the JSON contract promises a runnable command). The
`<agent>` shown in the table above is resolved at render time to a concrete
agent value from repo/global config (`ResolveAgent`), falling back to `codex`
(the `DefaultConfig` default) when nothing is configured.

**Part 2 — How roborev works + configuration playbook** (static embedded
markdown the agent reads to assist the user). Covers the four selected topics:

1. **Automation setup (two layers).**
   - Layer 1 — *Post-commit reviews*: the git post-commit hook enqueues a
     background review of every commit. Works with any editor/agent.
   - Layer 2 — *Agent hook*: watches the coding-agent session (turns, commits,
     failed reviews) and, when work piles up, returns one instruction telling the
     agent to run `/roborev-fix` before the session goes cold. Requires a CLI
     harness (see CLI-vs-Desktop note).
2. **Review tuning — "always flag X".** Use `review_guidelines` in
   `.roborev.toml` to inject standing instructions into every review prompt
   (worked example: "Every PR that changes UI must include or update a Playwright
   e2e test; flag changes that don't."). Plus correct `[[hooks]]` syntax with the
   `hooks = []` gotcha called out explicitly.
3. **Agent's own CLAUDE.md / AGENTS.md.** A ready-to-paste "commit early and
   often" snippet with rationale (roborev reviews per commit; frequent small
   commits → tighter, more useful feedback).
4. **Agent / model selection.** `agent`, `model`, per-workflow routing
   (`review_agent_*`, `fix_model_*`, etc.), and reasoning levels
   (fast/standard/thorough).

### Content authoring (Approach A)

Static explainer ships as a `//go:embed`'d markdown file
(`cmd/roborev/quickstart.md` or `internal/.../quickstart.md`). Detected state is
computed at runtime by a `detectState()` function and rendered above the
explainer. Website docs and this agent-facing prose are intentionally NOT a
shared source — different audiences and cadences.

### `--json` flag

First-class, stable schema for the detected-state portion only (omit the static
explainer). Top-level: `in_git_repo` (bool), `daemon_running` (bool, mirrors the
`daemon_running` check for convenience), and `checks` (array). Each check:

```json
{
  "in_git_repo": true,
  "daemon_running": true,
  "checks": [
    {
      "id": "post_commit_hook",
      "status": "ok",
      "details": "human-readable detail string",
      "fix_command": "roborev install-hook"
    }
  ]
}
```

Stable contract:
- `id` is one of exactly eight values: `daemon_running`, `post_commit_hook`,
  `repo_registered`, `repo_config`, `configured_agent`, `agent_hook_claude`,
  `agent_hook_codex`, `skills_installed`. The set and ordering are stable.
- `status` is a closed enum: `"ok" | "missing" | "unknown"`.
- `details` is a human-readable string (may be empty).
- `fix_command` is a runnable command string, present and non-empty whenever
  `status` is `"missing"`; it may be omitted/empty for `"ok"` or `"unknown"`.

Agents branch on `status` and run `fix_command`.

### Outside a git repo

Behavior is explicit and differs by output mode:
- **Human output:** exit non-zero with a short "run this inside a git repo /
  `roborev init`" setup message.
- **`--json`:** exit `0` with `in_git_repo: false`; repo-dependent checks
  (`post_commit_hook`, `repo_registered`, `repo_config`, `configured_agent`)
  report `status: "unknown"`; global checks (`daemon_running`,
  `agent_hook_*`, `skills_installed`) report their real state.

### Tests (`cmd/roborev/quickstart_cmd_test.go`)

- Detection is read-only: running `quickstart` against a repo with no daemon does
  NOT start one (assert no daemon process / no runtime record created), and does
  NOT modify `.git/hooks/post-commit` or any config file.
- `--json` emits the documented schema; `id` only ever takes the eight allowed
  values; `status` only ever takes allowed enum values; every `missing` check
  has a non-empty runnable `fix_command`.
- Static explainer is present in human output and absent from `--json`.
- Outside a git repo: human output exits non-zero with a setup message; `--json`
  exits `0` with `in_git_repo: false` and repo-dependent checks `unknown` while
  global checks report real state. Also covers the no-global-config case.

---

## Phase 2 — Docs restructure & content

### Nav (`docs/zensical.toml:42`)

Introduce a top-level **Automation** group, ordered to lead with the hands-off
path, using precise terminology that distinguishes the three distinct mechanisms:

```
{"Automation" = [
  {"Post-Commit Reviews" = "automation/post-commit-reviews.md"},
  {"Agent Hook"          = "agent-hook.md"},
  {"Review Event Hooks"  = "guides/hooks.md"},
]},
```

Rationale for naming:
- **Post-Commit Reviews** — the git post-commit trigger that reviews each commit.
- **Agent Hook** — the harness-level session nudge (existing `agent-hook.md`).
- **Review Event Hooks** — `guides/hooks.md`, which is about `review.*` event
  hooks (kata/beads/webhook/command), NOT the post-commit trigger. The current
  label "Review Hooks" conflates these; rename to remove ambiguity.

The Automation group sits high in the nav (just after Quick Start /
Installation), not buried near the bottom as today.

### New page `docs/automation/post-commit-reviews.md`

The single "make it run hands-off" page:
- The two-layer model (post-commit reviews + agent hook) with the new diagram.
- How to verify automation is live (`roborev status`, where reviews show up).
- The CLI-vs-Desktop note (below).
- Pointer to `roborev quickstart` for agent-assisted setup.

### Homepage (`docs/index.md`)

Add a "How roborev works" section near the top with the new concept SVG and the
two-layer automation pitch, placed above the agent matrix. TUI hero demoted to a
secondary "what it looks like" position.

### README.md

Mirror the two-layer automation framing near the top; embed the new diagram via
an absolute URL (see Phase 3).

### Content additions

- **CLAUDE.md commit-early snippet** — in `post-commit-reviews.md` and embedded
  in the `quickstart` explainer (single canonical wording, duplicated
  intentionally across human docs and agent output per Approach A).
- **Review-tuning guide** — extend `docs/configuration.md` and/or
  `docs/guides/reviewing-code.md` with the "always flag X" pattern
  (`review_guidelines`, Playwright example) and correct `[[hooks]]` usage.
- **CLI-vs-Desktop FAQ** — short entry (in `post-commit-reviews.md` and/or
  `docs/guides/troubleshooting.md`): the agent hook relies on harness hooks
  (`PreToolUse` / `PostToolUse` / `Stop`) that the Claude Code CLI and Codex
  expose but Claude Desktop does not. The post-commit review layer works
  everywhere; the agent-side automation loop needs a CLI harness.

### Tests / validation

- Docs build succeeds with the new nav and pages (the existing site build /
  link-check used in CI).
- No dangling nav entries; new page paths resolve.

---

## Phase 3 — "How roborev works" concept SVG

A single canonical, hand-authored SVG (not mermaid) matching the style of the
existing `architecture.svg` / `federation.svg`. Mermaid is fine for
maintainability elsewhere, but this is a product-facing concept diagram that must
also render in the GitHub README and visually match existing static assets.

Content: the loop —
**you + agent write code → commit → roborev reviews in background across agents
→ findings → agent hook nudges the agent → `/roborev-fix` → repeat.**

### Asset placement & wiring

- The SVG lives with the other static assets (`/assets/static/...`, served from
  the `docs-assets` orphan branch per `docs/README.md`).
- Add `how-it-works.svg` to BOTH asset manifests, or
  hydration/update validation will fail even with the SVG on the orphan branch:
  - `docs/assets/update-static-assets-branch.sh` `expected_assets` array
    (`:12`) — note this script errors on any file not in the list (`:104`).
  - `docs/assets/hydrate-assets.sh` `static_assets` array (`:15`).
- Docs reference it relatively: `/assets/static/how-it-works.svg`.
- README references it with an absolute URL so GitHub renders it:
  `https://roborev.io/assets/static/how-it-works.svg` (existing README images
  already use absolute URLs).

---

## Risks / Open Questions

- Adding `omitempty` could remove `hooks` from configs that a downstream test or
  tool expects to see; the round-trip tests cover this, but check for any code
  that asserts the literal presence of the key.
- Read-only detection depends on `probeDaemonWithRetry` semantics; confirm it
  performs no side effects (it only issues a `/ping` GET).
- The `docs-assets` orphan-branch workflow means the new SVG is added out-of-band
  from `main`; implementation must follow `docs/README.md` asset instructions.
