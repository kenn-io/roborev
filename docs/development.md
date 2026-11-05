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

### Package builds

Package maintainers can mark a build as owned by their package manager, so users
upgrade through it instead of `roborev update`. Set
`internal/version.Distribution` to the package manager's name with a linker
flag:

```bash
go build \
  -ldflags="-X go.kenn.io/roborev/internal/version.Distribution=homebrew" \
  -o bin/roborev ./cmd/roborev
```

- `roborev update`, including `--force`, refuses to replace these builds. For
    `homebrew`, the message tells users to run `brew upgrade roborev`. Other
    names get a message naming the package.
- `roborev update --check` still reports newer releases and says to upgrade
    through the package manager.
- The terminal interface skips update checks and update notifications.
- Builds with an empty `Distribution`, the default, keep the normal update
    behavior. So do official direct downloads, which set `Distribution` to
    `kenn`.

Homebrew formulas can use the `homebrew` make target. It runs the normal build
with cgo enabled and `Distribution` set to `homebrew`:

```bash
make homebrew VERSION=1.2.3 BINDIR=/path/to/bin
```

Other `make` and GoReleaser builds read `Distribution` from the `DISTRIBUTION`
variable, which is empty by default. The same value also sets kit's telemetry
distribution.

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
- **Workers**: Pool of 4 (configurable) parallel review workers. The daemon
    wakes idle workers when a job is queued, and they poll every 2 seconds as a
    fallback
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
runners. Test jobs restore a daily Go module and build cache saved by main's
test legs for each OS. Other jobs use the default `actions/setup-go` cache. The
lint job also restores main's custom golangci-lint binary and analysis cache.

Runner selection lives in `.github/workflows/ci.yml` and
`.github/workflows/windows-migration.yml`. The PR dispatchers call the workflow
files from the same commit, so workflow changes run before merging. Run
`make check-actions` locally, then check the PR's jobs on GitHub. Changes to the
Windows migration workflow also run its migration test.

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

### Paired review quality evaluation

The opt-in Go harness in `internal/review/quality_eval*_test.go` exercises
`prompt.Builder`, `RunAgentReview`, and `RunSynthesisAgent`. It uses two
synthetic Go mechanisms: cross-tenant invoice access and an exclusive cursor
that repeats its boundary record. Each has vulnerable, fixed, and safe
implementations. Offline behavior checks execute those exact fixture bytes,
including unchanged and harmless-comment controls. Compact controls are measured
separately; a missing finding is not proof of a compact disposition.

Ordinary tests make no model calls. Run the offline fixture, scoring, and
transport checks with:

```bash
GOMAXPROCS=2 GOFLAGS=-p=2 go test -tags=qualityeval ./internal/review \
  -run Quality -count=1
```

Model-visible checkouts use opaque temporary identifiers beneath a neutral
top-level root. Expected condition labels remain in host artifacts; neither
checkout paths nor the real review/synthesis prompt receive case IDs.

Before observing model output, freeze the harness commit and
`internal/review/testdata/quality/rubric.json`. The live entrypoint loads all
six fixture sources and the rubric from that exact commit, rejects changed or
missing checkout bytes, and prepares cases from the committed in-memory
snapshot. Later worktree changes cannot alter model inputs or recorded hashes.
Its mechanism descriptions, admissible matches, exclusions, and ambiguity rules
define scoring. Location alone never earns credit. Each externally adjudicated
finding can match at most one mechanism; subsequent reports count as duplicates.
Unmatched or ambiguous findings remain unjudged. Record the adjudicator and
method honestly, including when an external agent reviewed the source. The
harness never starts a semantic judge.

A live arm runs six reviews and two synthesis controls per repeat: true
duplicates and two distinct bugs. Three repeats plan 24 invocations per arm, 48
for a baseline/candidate comparison. This incurs model usage. The initial
adapter is Pi with `zai/glm-5.3-flash` and native `max` reasoning. Select an
installed schema-extension entrypoint and a Pi `auth.json` explicitly. Only its
`zai` entry is copied into a disposable home. The consuming child excludes
inherited `CODEX_HOME`, `CLAUDE_CONFIG_DIR`, `PI_CODING_AGENT_DIR`, unrelated
credentials, skills, context files, and discovered extensions. It enables the
explicit schema extension and the adapter's read-only tools. No normal daemon
state is used.

For a single committed source tree, set these variables and preflight before
spending:

```bash
export ROBOREV_QUALITY_HARNESS_DIR="$PWD"
export ROBOREV_QUALITY_HARNESS_HEAD="$(git rev-parse HEAD)"
export ROBOREV_QUALITY_PRODUCT_HEAD="$(git rev-parse HEAD)"
export ROBOREV_QUALITY_OUT=/tmp/quality-plan
GOMAXPROCS=2 GOFLAGS=-p=2 ROBOREV_QUALITY_PLAN_ONLY=1 \
  make test-review-quality-eval

export ROBOREV_QUALITY_PI_AUTH=/path/to/selected/pi/auth.json
export ROBOREV_QUALITY_PI_EXTENSION=/path/to/pi-json-schema/index.ts
export ROBOREV_QUALITY_OUT=/tmp/quality-live
GOMAXPROCS=2 GOFLAGS=-p=2 make test-review-quality-eval
```

Source preflight uses Go's selected dependency graph, including linked local and
module dependencies, resolved embedded inputs, and selected module metadata.
Linked local inputs must be committed; unrelated packages and Hegel/cache files
are allowed. Dependency versions and complete input hashes are recorded. The
launcher captures the selected source, embed, and module metadata bytes, then
compiles from a temporary snapshot of local inputs and the selected module-cache
files. It embeds that source inventory hash in the child and rejects a declared
harness directory with different bytes. Overlay use must also set
`ROBOREV_QUALITY_OVERLAY` and `ROBOREV_QUALITY_MODFILE` to the exact compiler
inputs shown below; only virtual quality-test replacements are allowed.

The entrypoint accepts exactly the `qualityeval` compiler tag and rejects extra
tags, compiler/assembly overrides, race/sanitizer/coverage instrumentation, and
nondefault cgo/experiment settings. It builds one temporary evaluation child
with the captured overlay, module file, and module-cache inputs. The binary
records both the selection fingerprint and selected-source inventory hash. The
child compares those bindings with its runtime inputs before invoking a model.
It uses the compiler's recorded platform and cgo selection for dependency
enumeration. Setting the child environment marker alone cannot bypass the
controlled build. The artifact records the selection fingerprint and compiled
build settings.

Every run requires a new output directory. The parent verifies that its child
wrote a manifest for the intended source heads and invocation count. `run.json`
records product and harness heads separately, source/fixture/rubric and
effective prompt hashes, model settings, planned/completed/failed/pending and
unjudged counts, per-repeat target sets and explicit
judged/unjudged/failed/planned states, timing, and observed usage. Raw streams,
complete prompts, and settled documents are saved alongside it. There are no
harness retries. Missing provider usage or cost remains unknown.

For a comparison, run the **same frozen test files** against two independent
product trees. Do not run one binary twice with different labels. Go's overlay
can add the test files without changing either tree. For each target, create an
overlay with explicit absolute paths:

```bash
export QUALITY_HARNESS=/path/to/frozen/harness
export QUALITY_SOURCE=/path/to/baseline-or-candidate
python3 - <<'PY'
import json
import os
from pathlib import Path
harness = Path(os.environ["QUALITY_HARNESS"])
source = Path(os.environ["QUALITY_SOURCE"])
files = (harness / "internal/review").glob("quality_eval*_test.go")
mapping = {str(source / p.relative_to(harness)): str(p) for p in files}
Path("/tmp/quality-overlay.json").write_text(json.dumps({"Replace": mapping}))
PY

cd "$QUALITY_SOURCE"
export ROBOREV_QUALITY_HARNESS_DIR="$QUALITY_HARNESS"
export ROBOREV_QUALITY_HARNESS_HEAD="$(git -C "$QUALITY_HARNESS" rev-parse HEAD)"
export ROBOREV_QUALITY_PRODUCT_HEAD="$(git rev-parse HEAD)"
export ROBOREV_QUALITY_OVERLAY=/tmp/quality-overlay.json
export ROBOREV_QUALITY_MODFILE="$QUALITY_HARNESS/go.mod"
export ROBOREV_QUALITY_OUT=/tmp/quality-baseline-plan
GOMAXPROCS=2 GOFLAGS=-p=2 ROBOREV_RUN_QUALITY_EVAL=1 \
  ROBOREV_QUALITY_PLAN_ONLY=1 go test \
  -modfile="$QUALITY_HARNESS/go.mod" -overlay=/tmp/quality-overlay.json \
  -tags=qualityeval ./internal/review -run '^TestReviewQualityEvaluation$' \
  -count=1 -v -timeout=0
```

Use a harness whose module file differs only by the intended test dependency
update. `-modfile` keeps that dependency identical while production code is
linked from the selected source tree. Inspect both plan manifests before
removing `ROBOREV_QUALITY_PLAN_ONLY=1` for live arms; use distinct output paths.
Require different intended product heads and identical harness, rubric,
fixtures, model, reasoning, repeats, and invocation plans. Declare the candidate
change and inspect prompt hashes; prompt changes should be visible there. Retain
negative results and operational failures.

Adjudication is offline. Supply JSON with `Actor`, `Method`, and `Entries` keyed
by invocation ID. Each entry needs the exact saved `OutputHash` and `Judgments`
referring to 1-based settled finding positions:

```json
{
  "Actor": "independent-reviewer",
  "Method": "external source adjudication under the frozen rubric",
  "Entries": {
    "invoice-vulnerable-1": {
      "OutputHash": "copy the hash from run.json",
      "Judgments": [
        {
          "Finding": 1,
          "Mechanisms": ["invoice"],
          "Reason": "The finding identifies the unchecked cross-tenant read."
        }
      ]
    }
  }
}
```

Alternatively use `Category` with `legitimate_extra`, `other_false_positive`, or
`unjudged` and a source-grounded `Reason`. `Mechanisms` and `Category` are
mutually exclusive. More than one proposed mechanism remains ambiguous and earns
no credit. Optional `Clarity` notes can record trigger, actual, expected,
preservation, and unsupported execution claims without affecting correctness.

```bash
ROBOREV_QUALITY_RESULTS=/tmp/quality-live/run.json \
  ROBOREV_QUALITY_JUDGMENTS=/tmp/quality-judgments.json \
  GOMAXPROCS=2 GOFLAGS=-p=2 go test -tags=qualityeval ./internal/review \
  -run '^TestReviewQualityAdjudication$' -count=1 -v
```

`adjudicated-v2.json` reports missed bugs, fixed and safe mechanism false
positives separately, duplicates, distinct synthesis loss, legitimate extras,
other false positives, and unjudged counts. Failed and pending runs stay in
planned denominators. Unknowns widen point bounds and 95% Wilson intervals.
These intervals describe variability conditional on two synthetic mechanisms;
repeats may be correlated. This small suite does not establish universal review
precision or prove an unexecuted reproduction.

## Building

```bash
go build ./...             # Build all
make install               # Install with version info
```

Plain `go build` and `go install` source builds contain the compilation stub and
therefore leave the browser listener disabled. `make build` and `make install`
run the validated web-asset transaction when an embedded browser application is
required.

### Shared build cache and debugging

`make` targets build and test Go code with `-trimpath`. Compiled code then
records module-relative source paths instead of absolute ones, so every clone
and git worktree shares one Go build cache. Without it, each checkout compiles
and caches its own copy of every package.

- To opt out for one command, run `make test GOFLAGS=-trimpath=false`.
- To get the same sharing from plain `go` commands outside `make`, run
    `go env -w GOFLAGS=-trimpath` once.

Trimmed binaries do not record where their source lives, so Delve needs help
finding it:

- `dlv debug` and `dlv test` inherit `GOFLAGS`. Pass
    `--build-flags=-trimpath=false` so Delve can show source. Debug builds turn
    off optimizations, so they never shared cache entries anyway.
- For `dlv exec` or `dlv attach` on a trimmed binary, start Delve from the
    repository root and run `config substitute-path -guess` (Delve 1.24 or
    newer). It maps the module path to your checkout, the standard library to
    `GOROOT`, and dependencies to the module cache.

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

Deploying needs a Vercel link in the `docs/` directory, because the deploy
uploads that directory:

```bash
vercel link --cwd docs
```

`docs/vercel.json` supplies the build settings. See
[Publishing](https://github.com/kenn-io/roborev/blob/main/docs/README.md#publishing)
in the docs maintainer guide for the project setup.

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

`make docs-deploy` runs `vercel deploy docs --prod`.

## License

MIT
