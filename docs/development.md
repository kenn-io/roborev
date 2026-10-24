---
title: Development
description: Contributing to roborev
---

## Getting Started

```bash
git clone https://github.com/kenn-io/roborev
cd roborev
go test ./...
make install    # Installs with version info (e.g., v0.7.0-5-gabcdef)
```

Or use `go install ./cmd/...` for quick iteration (version shows commit hash
only).

## Project Structure

```
roborev/
├── cmd/roborev/         # CLI entry point
├── web/                  # Native Svelte browser application
├── packages/             # Source-shipping browser component packages
├── internal/
│   ├── daemon/          # HTTP API server and worker pool
│   ├── web/             # Embedded, validated browser distribution
│   ├── storage/         # SQLite operations
│   ├── agent/           # Agent interface and implementations
│   └── config/          # Configuration loading
├── scripts/             # Install and utility scripts
└── docs/                # This documentation site
```

## Architecture

```
CLI / Agent hooks -> HTTP API -> Daemon (roborev daemon run) -> Worker Pool -> Agents
                                     |
                                     +-> SQLite DB
                                     +-> Agent Hook JSON state
```

- **Daemon**: HTTP server on port 7373 (auto-finds available port if busy)
- **Workers**: Pool of 4 (configurable) parallel review workers
- **Storage**: SQLite at `~/.roborev/reviews.db` with WAL mode
- **Agent Hook state**: JSON at `~/.roborev/agent-hook/state.json`

## Key Files

| Path | Purpose |
|------|---------|
| `cmd/roborev/main.go` | CLI entry point, all commands |
| `internal/daemon/server.go` | HTTP API handlers |
| `internal/daemon/worker.go` | Worker pool, job processing |
| `internal/storage/` | SQLite operations |
| `internal/agent/` | Agent interface + implementations |
| `internal/config/config.go` | Config loading, agent resolution |

## Adding a New Agent

1. Create `internal/agent/newagent.go`
1. Implement the `Agent` interface:

```go
type Agent interface {
    Name() string
    Review(ctx context.Context, repoPath, commitSHA, prompt string) (string, error)
}
```

1. Call `Register()` in `init()`

## Database Schema

Tables: `repos`, `commits`, `review_jobs`, `reviews`, `responses`

Job states: `queued` -> `running` -> `done`/`failed`

## Conventions

- **HTTP over gRPC**: Simple HTTP/JSON for the daemon API
- **No CGO in releases**: Build with `CGO_ENABLED=0` for static binaries (except
    sqlite which needs CGO locally)
- **Test agent**: Use `agent = "test"` for testing without calling real AI
- **Isolated tests**: All tests use `t.TempDir()` for temp directories

## Testing

```bash
go test ./...              # Run all tests
go test ./internal/agent/  # Test specific package
```

Ordinary unit tests are deterministic and offline. They do not authenticate to
Codex, contact a model, or run the live skill conformance evaluation.

### CI runners

CI uses GitHub-hosted `ubuntu-latest`, `macos-latest`, and `windows-2025`
runners. Go modules and build outputs use the default `actions/setup-go` cache.

Runner selection lives in `.github/workflows/ci.yml` and
`.github/workflows/windows-migration.yml`. The PR dispatchers call the workflow
files from the same commit, so workflow changes run before merging. Run
`make check-actions` locally, then check the PR's jobs on GitHub. Changes to the
Windows migration workflow also run its migration test.

### Domain review hook proof

The deterministic integration fixture builds a temporary CLI, installs the real
Git hook in a disposable repository, and makes five native Git commits. A
loopback daemon receives one two-member custom panel for the complete range.
Schema-capable test agents return findings so real synthesis runs. Assertions
cover committed PDF and rendition bytes, portable stored prompts, complete
oversized prompts, strict empty grants, member replacement, retry and cancel
cleanup. Production `TestAgent` capabilities are unchanged.

```bash
go test -tags integration ./internal/daemon -run '^TestDomainReview' -count=1
go test ./internal/prompt -run '^TestGoldenPrompt' -count=1
go test -tags integration ./internal/daemon -run '^$' \
  -fuzz '^FuzzDomainNativeReadPathIdentity$' -fuzztime=30s
```

The native case skips unless `ROBOREV_RUN_DOMAIN_REVIEWS=1`. Run it on an
approved authenticated host at the exact candidate commit. Set
`ROBOREV_DOMAIN_REVIEW_ARTIFACTS` to a private absolute output directory outside
the checkout:

```bash
ROBOREV_RUN_DOMAIN_REVIEWS=1 \
ROBOREV_DOMAIN_REVIEW_ARTIFACTS="$HOME/private-domain-proof" \
go test -tags integration ./internal/daemon \
  -run '^TestDomainReviewHookPanelNative$' -count=1 -v
```

This case invokes the real Claude adapter, selected synthetic MCP server and
real synthesis. It preserves the operator's authentication HOME and provider
routing, adds synthetic inherited session/tool overrides, and relies on adapter
scrubbing. Roborev data and loopback listeners are disposable. No production MCP
registration or sync is used. The installed CLI must support the strict startup
recipe.

The private artifacts contain raw native streams and frozen job records.
Verification requires successful native Read results correlated to the exact
offered PDF, verified text and complete prompt paths. On macOS the agent
checkout also crosses `/tmp`'s system symlink. Native init must show the strict
tool set, permission mode and selected server set. The granted call must reach
the server; same-server ungranted, prefix and newly advertised tools need native
permission denials. Removed write, shell, delegation, web and unselected-server
capabilities must be unavailable at init, with no successful forbidden calls or
changed file/hook sentinels. Model statements are not native denial evidence.

Run the separate strict adapter qualification as well. Neither deterministic
flags tests nor an earlier native run qualify a later candidate. Keep raw bridge
artifacts private; they may contain local paths and provider responses.

### Codex skill conformance evaluation

The opt-in live evaluation checks that ordinary natural-language review and fix
requests use Codex's native behavior without invoking roborev. It also covers
pasted findings, historical transcripts, and quoted skill mentions as native
requests, while verifying that an explicit `$roborev-fix` invocation or direct
Agent Hook instruction activates `roborev-fix` and that the exact explicit
`$roborev-review-branch` workflow activates the branch-review skill. It requires
`codex-cli` 0.144.1 or newer, authenticated plus network and model access, and
it incurs model usage:

```bash
make test-codex-skill-eval
```

Override the default model with a comma-separated comparison set:

```bash
make test-codex-skill-eval CODEX_SKILL_EVAL_MODELS='gpt-5.5,gpt-5.6-sol'
```

The live target currently requires POSIX shell startup behavior. Native Windows
skips the live evaluation before any model call. The tagged helper suite runs
without model usage; on native Windows, only its POSIX shell-resolution
preflight test skips, while the parser, execution-oracle, and process helpers
still run. Exercise the tagged offline helpers with:

```bash
go test -tags=codexeval ./internal/skills
```

To verify that the complete tagged package remains Windows-cross-compilable:

```bash
GOOS=windows GOARCH=amd64 go test -c -tags=codexeval \
  -o /tmp/roborev-skills-windows.test.exe ./internal/skills
```

The harness creates an isolated `HOME` and `CODEX_HOME`, copies the existing
Codex authentication file into that disposable home, installs the in-tree skills
there, and runs each case in a temporary git repository. Every Codex subprocess
sets `allow_login_shell=false`; isolated `.zshenv`, `BASH_ENV`, and `ENV`
startup files plus a safe `PATH` ensure global login profiles cannot reorder
command resolution. A non-login `-c` preflight proves each available supported
shell resolves the harmless roborev stub before Codex starts. Codex runs with
`workspace-write` against the disposable repository plus a separate disposable
evidence directory; the user checkout and normal agent state remain outside
those configured writable roots. The stub writes a fresh per-case sentinel
before printing its marker, so redirected or indirect execution remains
detectable without contacting the roborev daemon. The evaluation never reads or
writes the normal daemon database or review state.

## Building

```bash
go build ./...             # Build all
make install               # Install with version info
```

Plain `go build` and `go install` source builds contain the compilation stub and
therefore leave the browser listener disabled. `make build` and `make install`
run the validated web-asset transaction when an embedded browser application is
required.

### Browser application

The browser workspace uses Bun 1.4.4. Install the pinned dependency graph and
run its complete checks from the repository root:

```bash
bun install --frozen-lockfile
bun run web:check
bun run web:test
bun run web:test:e2e
make api-check
make web-release-check
```

`pkg/client/openapi.yaml` is the canonical OpenAPI document. Run
`make api-generate` after changing daemon routes to regenerate the document, the
DoorDash oapi-codegen Go client, and the Orval TypeScript clients.
`make api-check` verifies that these generated files match the daemon routes. Go
callers use `pkg/client` to configure the HTTP transport. Its generated raw
methods return response bodies without buffering; callers must close them.

`make web-release-check` builds the SPA, validates its Vite manifest,
temporarily stages it for Go embedding, tests the embedded release, and always
restores the tracked compilation stub.

The daemon's typed HTTP API and production JSON handling use Go's JSON v2
encoder. The Huma configuration registers this encoder in its JSON format map.
Ordinary slice fields are non-nullable collections in OpenAPI and serialize as
`[]` when empty. Use an explicit nullable representation only when `null` is
part of the API contract. Use explicit JSON field tags for case-sensitive
decoding and `omitzero` when zero-valued numbers must be omitted. JSON used for
experiment fingerprints uses deterministic encoding so map key order does not
change stored hashes.

To exercise a checkout exactly like an installed release, including the embedded
application and the normal Roborev SQLite database and configuration, build the
release-shaped binary and launch the UI through that binary:

```bash
bun install --frozen-lockfile
make build
./bin/roborev ui
```

This is intentionally different from `make web-dev`: it uses the normal Roborev
data directory, starts or restarts the normal daemon as needed, and opens the
reviews stored in the user's database. The binary remains under `bin/` and does
not replace an installed `roborev`. Because this path runs the checkout against
real state, use it only when that is the intended test.

`bun run web:test:e2e` builds and embeds the production SPA into a scratch Go
binary, seeds a synthetic SQLite database, starts a token-authenticated daemon
with no workers, and runs the review and browser-security scenarios in Chromium.
The runner uses a disposable home and data directory, restores the compilation
stub, and removes all temporary state on success, failure, or interruption.

Use `make web-dev` for full-stack development. It starts Vite and a branch-built
daemon with a disposable data directory, SQLite database, and configuration; it
never connects to the normal Roborev daemon or data directory. The runner
allocates a Vite port first and supplies that exact loopback origin to the
disposable daemon. There is no automatic development-origin relaxation. Stop the
command to terminate both processes and remove the temporary data.

## Documentation

The public docs site lives in `docs/` and is built with Zensical. The docs
source is tracked in this repository; image media is hydrated from two orphan
branches before builds:

- `docs-assets` contains curated static media such as logos, favicons, diagrams,
    Open Graph images, and integration screenshots.
- `docs-generated-assets` contains generated browser, CLI, and TUI screenshots.

The screenshot pipeline reads the normal review database only through a
read-only SQLite connection, extracts completed clean reviews from the canonical
public Roborev repository, sanitizes copied content, and runs the branch binary
against that reduced database inside Docker. See the
[screenshot pipeline guide](https://github.com/kenn-io/roborev/blob/main/docs/screenshots/README.md)
for the capture and asset publication workflow.

Install the docs toolchain and build the site:

```bash
make docs-install
make docs-build
```

Preview locally:

```bash
make docs-serve
```

Run the docs validation checks:

```bash
make docs-check
```

The Vercel project should be linked from the repository root with `docs/` as its
root directory. Use these project settings:

| Setting | Value |
| --- | --- |
| Framework preset | `Other` |
| Root directory | `docs` |
| Install command | `uv sync --frozen --no-dev` |
| Build command | `uv run --frozen bash ./vercel-build.sh` |
| Output directory | `site` |

The build wrapper also copies `index.md` and every nav-listed Markdown document
into `site/`. That keeps source-form docs available from the same deployment as
the rendered page: for example, `/changelog.md` serves the Markdown source that
generated `/changelog/`.

Deploy committed docs changes with:

```bash
scripts/update-docs.sh
```

The helper updates and pushes `docs-generated-assets`, hydrates both asset
branches, builds the docs, runs `make docs-check`, and deploys through Vercel.
For the direct Vercel deploy step only, use:

```bash
make docs-deploy
```

`make docs-deploy` runs `vercel deploy --prod` from the repository root.

## Native strict Claude qualification

Strict tool policies need a live test of the installed Claude CLI as well as
unit tests. Use an authenticated execution host with `claude`, `python3`, and
the project's Go toolchain. Run the candidate commit from inside a Claude
session; keep its native authentication and provider routing environment. Choose
a private absolute artifact directory outside the checkout:

```bash
ROBOREV_RUN_STRICT_CLAUDE=1 \
ROBOREV_STRICT_CLAUDE_ARTIFACTS=/tmp/strict-review-proof \
go test -tags integration ./internal/agent \
  -run '^TestClaudeStrictNativeQualification$' -count=1 -v
```

The test invokes the actual Go adapter for ordinary reviews, strict reviews with
empty grants, and strict reviews with one exact MCP grant, in plain and schema
modes. It adds synthetic inherited session/tool overrides. Ordinary cases also
enable coordinator mode in project settings to exercise the adapter's narrow
environment and invocation-level settings overrides. Do not substitute a copied
CLI invocation or a wrapper that clears the whole environment. Normal CI skips
this live test.

Inspect each case's `native-stream.jsonl`, `adapter-output.json`, server call
log, and checkout sentinels. Successful native Read, Glob and Grep results are
required; model-authored claims do not count. Strict granted calls also need a
successful lookup and native permission denials for same-server ungranted,
prefix-lookalike and newly advertised tools. Unselected/ambient servers must
never start. Unexpected built-ins, missing startup evidence or failed/late
selected servers fail the invocation even if the final review looks valid.

Strict cases include permissive project settings and a hook sentinel: hooks must
stay inactive and checkout files must remain unchanged. Ordinary hardening
restricts available tools and MCP configuration; it does not promise strict
settings or hook isolation. The synthetic MCP server permits every test tool, so
its call log independently exposes any ungranted call that escapes the native
permission layer. Raw host streams may contain session or machine details; keep
them private and attach only a sanitized result summary to a public review.

## License

MIT
