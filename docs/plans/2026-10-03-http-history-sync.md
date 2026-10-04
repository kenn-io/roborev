# Proposed HTTP completed-history synchronization

This is a design follow-up, not implemented behavior. Native request signing
protects [restricted history reads](../advanced/request-signing.md); it does not
replicate local history. Existing synchronization still uses PostgreSQL directly.

## Purpose and boundary

Let a source daemon publish completed history to a private history service over
HTTPS, without exposing PostgreSQL or executing reviews remotely. The source
keeps SQLite as primary storage and continues running reviews locally. A display
service consumes history without a worker pool, git checkout, hooks, or provider
credentials. Building that display-only runtime is a separate prerequisite.

Do not extend the existing restricted listener with a generic mutation grant.
A sync endpoint needs separate scopes, payload validation, and daemon-owned
transactions. A signed request is still subject to authorization.

## Proposed operations and payloads

| Operation | Scope | Contract |
| --- | --- | --- |
| `POST /api/history/v1/batches` | `history:publish` | Transactionally ingest an idempotent batch for the authenticated source |
| `GET /api/history/v1/changes?cursor=...` | `history:pull` | Page changes for the credential's source/repository grants |
| `GET /api/history/v1/capabilities` | Sync credential | Advertise supported schema and entity versions; never weaken signing policy |

Each publish envelope has `schema_version`, `source_machine_uuid`,
`batch_uuid`, `previous_ack_cursor`, and `entities`. Authenticate the source
identity from its scoped credential; reject a conflicting source in the body.
Bind a reused `batch_uuid` to the exact envelope digest. Repeating the same
batch returns the original acknowledgement; different content returns 409.

Repository records carry a stable repository identity and display name. Machine
records carry a UUID and display label. Jobs carry `uuid`, `repo_uuid`,
`source_machine_uuid`, terminal `status`, immutable enqueue/completion times,
agent/model metadata, git ref, branch, job type, review type, analysis metadata,
and panel UUID relationships. Only done, failed, canceled, and deliberately
supported skipped records are publishable. Reviews reference `job_uuid` and carry
structured review JSON, output, closed state, and update revision. Responses
carry their own UUID, owning job/commit UUID, text, author label, and revision.

Numeric SQLite IDs never identify entities across machines. Reuse the current
sync UUIDs rather than allocating unrelated HTTP identities. Do not publish
local execution paths, credentials, worker IDs, command lines, or scheduling
state. Carry informational repository identity separately from local root-path
mapping; an imported path must never authorize filesystem access or execution.

A response carries `batch_uuid`, durable acknowledgement, and a resumable
change cursor. A pull page contains `database_uuid`, `cursor_version`, ordered
change sequence, entity versions, and `next_cursor`. Cursors are opaque and
bound to the database, credential scopes, schema, and ordering. Reject an old
cursor from another database or changed scope; do not silently restart midway.
Page-size limits need measured payload evidence or an authoritative ingress
contract before implementation. Never silently clip review content.

## Conflicts and deletions

Source-owned immutable job content is accepted only from its enrolled source.
Reject a second source trying to overwrite that UUID. Validate references and
panel ownership before committing a batch; no partial batch acknowledgement.
Imported numeric IDs are allocated locally and UUID references resolved within
the transaction.

For mutable review closed state and responses, use a service-assigned revision
and compare-and-swap `expected_revision`. On 409 return the current revision and
value; the client reconciles visibly instead of choosing a wall-clock winner.
Repeated identical updates are idempotent. Device clocks never decide conflicts.

Represent deletion explicitly with typed UUID tombstones and revisions. Define
retention based on the longest supported offline interval. A client older than
retained tombstones must perform an explicit snapshot rebase; it cannot resurrect
removed rows merely by replaying an old batch. Preserve source provenance and
local unacknowledged changes during that rebase.

## Retry and recovery

Write a local outbox transaction together with each eligible history change.
Persist batch identity and canonical payload before upload. Retry network errors,
429, and retryable 5xx with bounded exponential backoff and jitter; honor
`Retry-After`. Each HTTP attempt receives a fresh signature nonce while retaining
the batch UUID. Auth/signature failures stop retrying and require operator action.

The receiver commits entities, acknowledgement, idempotency record, and change
sequence atomically. A dropped response after commit is recovered by repeating
the same batch. The source marks its outbox acknowledged only after verifying the
returned batch and database identities. Pull entities and advance the local
cursor in one transaction. Cancellation or a crash leaves the previous cursor
available. Restarting must not lose or duplicate unacknowledged changes.

## Parity and acceptance evidence

Before enabling HTTP sync, compare the same synthetic dataset through both
transports: completed jobs, reviews, open/closed changes, responses, panel
relationships, analysis metadata, timestamps, source identities, Unicode/NUL
handling, local path remapping, and optional search vectors. Review fields that
PostgreSQL intentionally normalizes must have an explicit HTTP rule too.
Queued/running work remains local. Search vectors require separate version and
provider compatibility; unsupported vectors must be reported rather than dropped.

Test duplicate and reordered batches, concurrent publishers, CAS conflicts,
clock skew, failed acknowledgements, interrupted uploads, receiver restart,
source restart, tombstone expiry, cursor reset, schema mismatch, and key
revocation. Verify least-privilege publish/pull scopes and a display-only runtime
that cannot start workers or invoke agents. Migration requires an explicit
cursor/bootstrap plan; neither signing nor read-only export is a substitute.
