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

Or use `go install ./cmd/...` for quick iteration (version shows commit hash only).

## Project Structure

```
roborev/
├── cmd/roborev/         # CLI entry point
├── internal/
│   ├── daemon/          # HTTP API server and worker pool
│   ├── storage/         # SQLite operations
│   ├── agent/           # Agent interface and implementations
│   └── config/          # Configuration loading
├── scripts/             # Install and utility scripts
└── docs/                # This documentation site
```

## Architecture

```
CLI (roborev) -> HTTP API -> Daemon (roborev daemon run) -> Worker Pool -> Agents
                                |
                            SQLite DB
```

- **Daemon**: HTTP server on port 7373 (auto-finds available port if busy)
- **Workers**: Pool of 4 (configurable) parallel review workers
- **Storage**: SQLite at `~/.roborev/reviews.db` with WAL mode

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
2. Implement the `Agent` interface:

```go
type Agent interface {
    Name() string
    Review(ctx context.Context, repoPath, commitSHA, prompt string) (string, error)
}
```

3. Call `Register()` in `init()`

## Database Schema

Tables: `repos`, `commits`, `review_jobs`, `reviews`, `responses`

Job states: `queued` -> `running` -> `done`/`failed`

## Conventions

- **HTTP over gRPC**: Simple HTTP/JSON for the daemon API
- **No CGO in releases**: Build with `CGO_ENABLED=0` for static binaries (except sqlite which needs CGO locally)
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
requests do not invoke roborev, while an explicit `$roborev-review-branch`
invocation does. It requires `codex-cli` 0.144.1 or newer, authenticated plus
network and model access, and it incurs model usage:

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
Codex authentication file into that disposable home, installs the in-tree
skills there, and runs each case in a temporary git repository. Every Codex
subprocess sets `allow_login_shell=false`; isolated `.zshenv`, `BASH_ENV`, and
`ENV` startup files plus a safe `PATH` ensure global login profiles cannot
reorder command resolution. A non-login `-c` preflight proves each available
supported shell resolves the harmless roborev stub before Codex starts. Codex
runs with `workspace-write` against the disposable repository plus a separate
disposable evidence directory; the user checkout and normal agent state remain
outside those configured writable roots. The stub writes a fresh per-case
sentinel before printing its marker, so redirected or indirect execution remains
detectable without contacting the roborev daemon. The evaluation never reads or
writes the normal daemon database or review state.

## Building

```bash
go build ./...             # Build all
make install               # Install with version info
```

## Documentation

The public docs site lives in `docs/` and is built with Zensical. The docs
source is tracked in this repository; image media is hydrated from two orphan
branches before builds:

- `docs-assets` contains curated static media such as logos, favicons, diagrams,
  Open Graph images, and integration screenshots.
- `docs-generated-assets` contains generated CLI and TUI screenshots.

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

The Vercel project should be linked from the repository root with `docs/` as
its root directory. Use these project settings:

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
