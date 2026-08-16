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
required. The Nix source build likewise provides the CLI and terminal UI only.

### Browser application

The browser workspace uses Bun 1.3.14. Install the pinned dependency graph and
run its complete checks from the repository root:

```bash
bun install --frozen-lockfile
bun run web:check
bun run web:test
bun run web:test:e2e
make api-check
make web-release-check
```

`make api-check` verifies that browser types match the canonical OpenAPI
document. `make web-release-check` builds the SPA, validates its Vite manifest,
temporarily stages it for Go embedding, tests the embedded release, and always
restores the tracked compilation stub.

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

## License

MIT
