---
last_edited: 2026-09-17
title: Review History Search
description: Search completed reviews by keyword, meaning, repository, branch, time, verdict, and state
---

Roborev can search completed review history across every repository in the local
review database. Search is available through the CLI, the daemon HTTP API, and
the read-only MCP server.

Lexical search works without an embedding provider or a network request.
Semantic and hybrid search are optional. They use a local derived index and an
OpenAI-compatible embeddings endpoint.

## Search from the CLI

```bash
roborev search "retry provider failures" --agent
roborev search retry provider failures
roborev search "database is locked" --lexical
roborev search "concurrency problems during shutdown" --semantic
roborev search "stale cache" --repo roborev --branch main --open
roborev search "authentication" --since 720h --verdict fail --limit 10
roborev search "deadlock" --json
```

Search is global by default. Roborev does not infer a repository from the
current directory. Use `--repo` with a registered repository path, name, or
identity when you want a narrower scope.

The available filters are:

| Filter | Meaning |
|--------|---------|
| `--repo <path-or-name>` | One registered repository |
| `--branch <name>` | Exact branch name |
| `--since <value>` | Positive Go duration such as `72h`, or an RFC3339 timestamp |
| `--verdict pass\|fail` | Parsed review verdict |
| `--open` | Open reviews only |
| `--closed` | Closed reviews only |
| `--limit <n>` | At most 1 to 100 grouped results; default 20 |

Open and closed reviews are both searched when neither state flag is present.
The mode flags `--lexical`, `--hybrid`, and `--semantic` are mutually exclusive.
With no mode flag, the CLI requests `auto` mode. Quoted and unquoted query words
behave the same: `roborev search retry failures` and
`roborev search "retry failures"` search for the same text.

Use `--agent` for concise agent-readable output: an `OK search` header reports
the count, query, effective mode, and coverage warnings, followed by one
`- key=value` row per result. Values containing whitespace are quoted and
escaped. The flag changes formatting only; it does not select a search mode. Use
`--json` for the complete response. These two output flags are mutually
exclusive.

Search results identify the exact job and review that matched. Use
`roborev show <job-id>` to read the full review and its responses.

## Search modes

| Mode | Behavior |
|------|----------|
| `auto` | Uses hybrid search when embeddings are configured and ready, or ordinary lexical search when embeddings are not configured. A temporarily unavailable semantic path falls back to lexical with a degradation reason. |
| `lexical` | Uses local SQLite full-text search only. It never calls the embedding provider. |
| `hybrid` | Requires lexical and semantic search, then combines their ranked results. |
| `semantic` | Requires the embedding provider and a ready vector generation. |

Explicit `hybrid` and `semantic` requests fail when their semantic leg is not
available. An unconfigured provider is a bad request; a configured provider or
vector index that is temporarily unavailable is a service-unavailable error.
`auto` remains usable and reports its effective mode as `lexical` instead.

Lexical input is treated as literal text rather than raw FTS5 syntax. Roborev
also searches local identifiers such as repository, branch, ref, job UUID,
review UUID, review type, agent, and full commit SHA. Identifier matches rank
ahead of ordinary text matches. A hexadecimal query from 7 through 40 characters
also matches commit SHA prefixes case-insensitively. An ambiguous prefix can
return more than one result within the requested limit.

### Panel results

A multi-agent panel appears once in a result page, even when several panel
members matched. Roborev groups panel members and synthesis reviews by panel
run, combines the best lexical and semantic matches for that group, and returns
the job ID, metadata, and excerpt of the member selected by ranking.

Filters apply to each matching member before grouping. For example, an open
panel member can satisfy `--open` even when the panel synthesis review is
closed. The returned job ID still identifies the matching member.

## Coverage and result state

Human-readable output and JSON responses report the effective mode and current
index coverage. Three flags describe different conditions:

| Field | Meaning |
|-------|---------|
| `degraded` | `auto` could not use the complete semantic path and returned the available lexical or bounded hybrid result. `degraded_reason` explains why. |
| `partial` | The initial canonical mirror is incomplete, a vector generation needed by the effective mode is incomplete, or semantic retrieval reached its candidate ceiling. Some eligible history may not be represented. |
| `bounded` | Semantic retrieval exhausted its fixed candidate ceiling before it could prove the requested page complete. `bounded_reason` is `semantic candidate ceiling exhausted`. |

`partial` describes coverage, while `degraded` describes how the requested mode
ran. They can be set independently. Explicit semantic and hybrid searches return
a service-unavailable error when the candidate ceiling prevents a complete page.
In `auto` mode, Roborev returns the reachable result, sets all applicable
fields, and keeps the limit bounded.

The `coverage` object reports whether the canonical mirror scan is complete, its
known backlog, whether embeddings are configured, vector state, embedding
backlog, and skipped documents. Vector state is one of `disabled`, `building`,
`active`, `replacing`, or `unavailable`. Mirror backlog is omitted while its
exact size is not yet known.

## Freshness and backfill

The daemon reconciles search data in the background at startup, after review and
response changes, after rows are pulled from PostgreSQL sync, and during a
periodic safety sweep. Existing completed reviews are included in the first
mirror scan. Reviews synchronized from PostgreSQL are indexed locally after
normal sync convergence; PostgreSQL does not store search vectors.

Lexical rows become available as mirror pages commit. A first embedding
generation is not activated until all current documents have been handled.
During this initial backfill, `auto` uses lexical search and the health and
coverage fields report progress.

Changing the model, dimensions, input type mode, content recipe, or
`fingerprint_salt` starts a replacement generation. Roborev keeps the old
generation on disk during the build but does not query vectors from an
incompatible generation. `auto` therefore degrades to lexical search until the
replacement activates. Explicit semantic and hybrid requests remain unavailable
during that window.

Search rehydrates every candidate from the canonical review database before
returning it. If a review changed after the sidecar was updated, Roborev drops
the stale hit and wakes reconciliation instead of serving stale content.

Use `roborev daemon status` or its `roborev status` alias to watch search
health. The Search section reports indexed lexical documents, mirror state and
backlog, vector state, embedded and pending counts, skipped documents, rate,
ETA, and the latest sanitized provider error when available. The same data is
available in the `search` object from `GET /api/health`.

## Configure Voyage embeddings

Embeddings are configured only in the global `~/.roborev/config.toml`.
Repository configuration cannot select or redirect an embedding provider.

Export the API key into the daemon's environment, then add this configuration:

```bash
export VOYAGE_API_KEY="..."
```

```toml
[search.embeddings]
base_url = "https://api.voyageai.com/v1"
model = "voyage-4-large"
dims = 1024
api_key_env = "VOYAGE_API_KEY"
input_type_mode = "retrieval"
batch_size = 64
timeout_seconds = 30
```

Restart the daemon after changing embedding settings. In retrieval mode, Roborev
sends `input_type = "document"` while indexing and `input_type = "query"` while
searching.

This release supports Voyage's default 1,024-dimensional output for
`voyage-4-large`. Roborev validates the configured dimension but does not send
Voyage's provider-specific `output_dimension` parameter. Non-default Voyage
dimensions are outside this release.

`api_key_env` is preferred to an inline `api_key`. The two settings are mutually
exclusive. If the configured environment variable is absent or empty, daemon
startup fails without making an unauthenticated provider call.

An HTTP endpoint carrying a bearer token is rejected by default. Set
`trust_private_network = true` only for an HTTP service on a private network
whose transport boundary you trust. HTTPS endpoints need no override.

## Privacy boundary

Enabling hosted embeddings sends the rendered review documents to the configured
provider. Those documents include commit subjects, review type and verdict,
structured summaries, findings, suggested fixes, legacy review output, and human
responses.

Raw prompts, diffs, patches, command lines, job logs, token data, repository
paths, remote identities, and outputs from ineligible job types are excluded
from embedding input. Repository names, branches, refs, UUIDs, agent names, and
commit SHAs are indexed locally as lexical identifiers and are not added to
embedding input by that identifier index.

The included review and response fields are free-form prose. They can themselves
quote code, paths, URLs, logs, or other sensitive text. Roborev does not redact
those quotations before sending them to the provider. Review the provider's
retention and privacy terms before enabling hosted embeddings.

Lexical-only operation keeps search local and makes no embedding network call.

## Storage and recovery

Search data is derived local state in `reviews.search.db`, next to the canonical
`reviews.db`. It contains the text mirror, FTS index, and local vector
generations. It is not synchronized to PostgreSQL and is never the source of
truth for review content or liveness.

On a schema mismatch or structural corruption, the daemon removes and rebuilds
the search sidecar and its SQLite journal files. The canonical `reviews.db` is
not removed or rewritten by this recovery. Missing FTS5 or vector runtime
support is reported as a binary capability error because rebuilding cannot fix
it.

## HTTP and MCP

The daemon exposes read-only search at `GET /api/search` on its existing local
API boundary:

```bash
curl --get http://127.0.0.1:7373/api/search \
  --data-urlencode 'q=retry provider failures' \
  --data-urlencode 'mode=auto' \
  --data-urlencode 'state=open' \
  --data-urlencode 'limit=20'
```

Query parameters are `q`, `mode`, `repo`, `branch`, `since`, `verdict`, `state`,
and `limit`. The response includes query and mode state, coverage, and grouped
hits with canonical job and review IDs, repository/ref metadata, score, matching
legs, and a bounded excerpt. Interpret `score` together with `mode`; lexical,
semantic, and hybrid modes use different ranking scales.

The MCP tool `roborev_search_reviews` accepts the same inputs and returns the
same response fields. Use `auto` for ordinary discovery, `lexical` for exact
paths, identifiers, or quoted errors, and `semantic` when wording-independent
retrieval is specifically needed. Then call `roborev_get_review` with the
returned `job_id` and fetch responses only when needed.
