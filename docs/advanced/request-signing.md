---
title: Native Request Signing
description: Signed, restricted HTTPS access to daemon history
---

Native clients can read daemon history over an explicitly configured HTTPS
endpoint. An optional restricted listener requires both the daemon bearer key
and an independent HTTP signing key. It exposes no execution or administrative
operations. Existing local unsigned clients and browser sessions retain their
existing behavior.

## Supported workflows

| Workflow | Remote listener |
| --- | --- |
| List jobs and their scoped statistics | Supported |
| Read a review or comments by job ID | Supported, including server-resolved legacy comments |
| Watch job events | Requires a separate `history:events` grant |
| CLI `list`, `show --job`, `wait`, `stream` | Supported |
| TUI | Use a local daemon; remote TUI is not supported |
| Review submission, fix, refine, hooks, repository registration | Local only |
| Configuration, provider credentials, shutdown, MCP, profiling | Denied |
| Completed-history synchronization | Still uses [PostgreSQL](postgres-sync.md) directly |

A signature authenticates request bytes. Operation grants and repository grants
still decide what a key may read. A valid signature cannot enable a denied
route. Remote repository paths never cause the server to inspect git or the
filesystem. The remote listener is a read surface on an existing daemon; it does
not start a second worker pool or change where reviews execute. It does not
provide a worker-free display daemon or copy another machine's SQLite history.

## Create private signing files

Generate a fresh key and replay state without printing the secret:

```bash
roborev signing-init --secret-file /run/secrets/review-reader.key \
  --replay-file /var/lib/roborev/request-replay.json
```

Both destinations must be new, distinct files in existing private directories.
Initialization syncs each file and its parent directory before reporting
success. If it fails, it removes only files created by that invocation. The
secret file contains 128 lowercase hex characters representing 64 random bytes.
Keep it owner-only. A named environment variable containing the same hex format
can replace `secret_file` with `secret_env`; never put the secret in command
arguments, URLs, source files, or browser storage.

The key is independent of `auth_key`. This listener requires a nonempty valid
`auth_key` as well as signing. Configure clients with both credentials.

## Configure the restricted listener

Settings are captured at startup and require a daemon restart. Disabled is the
default. The default listener address, when enabled, is `127.0.0.1:7375`. No
network route is created automatically.

```toml
[remote]
enabled = true
listen = "127.0.0.1:7375"
external_url = "https://reviews.example.com/history"
trusted_proxy = true
replay_file = "/var/lib/roborev/request-replay.json"

# Operator-selected ingress budgets; these are examples, not defaults.
max_body_bytes = 1048576
max_header_bytes = 16384
max_concurrent = 4
replay_capacity = 10000
read_header_timeout = "5s"
read_timeout = "30s"
request_timeout = "1m"

[[remote.keys]]
id = "reader-2026"
secret_file = "/run/secrets/review-reader.key"
grants = ["history:read"]
repo_ids = [1, 2]
```

Choose budgets from the actual ingress contract and workload. Body and header
budgets are bytes; concurrency is simultaneous admitted requests, including
streams; replay capacity bounds active nonces and retained key identities. Time
budgets are Go durations. Every budget is required and positive. Saturated
request slots return 503; full replay state fails closed. Bodies are streamed to
private temporary files, checked completely, and removed after the request.
Oversized content is rejected, never truncated. Read deadlines bound body
verification and are cleared once the body is verified. Write deadlines and
`request_timeout` bound execution and streams. Streams end at `request_timeout`
and must reconnect with a fresh signature. These budgets apply only to the new
restricted listener.

Use repository numeric IDs from the local daemon's repository listing. Every key
needs explicit `repo_ids`, or `all_repos = true` instead. Unknown repository IDs
prevent startup. Empty grants never become access to every repository.
`history:read` permits `/api/jobs`, `/api/review`, and `/api/comments`; ping is
available to authenticated keys. `history:events` separately permits
`/api/stream/events`. No mutation grant exists.

Scoped job listings apply stable repository IDs before pagination and
statistics; renaming or reassigning a repository path cannot transfer a grant.
ID lookups recheck the owning repository after body verification and replay
admission. Legacy SHA comment joins use that repository too. Review and comment
reads require `job_id`; SHA and commit-ID lookups are excluded. Job panel
summaries are omitted because their member joins need an independent scope
audit. Stored command lines, adapter configuration, session IDs, and worktree
paths are omitted from remote job metadata. Event delivery verifies the owning
job's repository; global events without an owning job are excluded. Event
streams are best-effort notifications, not replication cursors.

### TLS and path rewriting

In `trusted_proxy` mode the listener must bind an explicit loopback IP. Only use
this mode behind a trusted TLS terminator on the same machine. It must strip
exactly the configured external path prefix and preserve query bytes, including
a terminal `?`. Do not expose this plaintext listener to the network.
`Forwarded` and `X-Forwarded-*` headers never choose the signed origin or
prefix. The verifier reconstructs the external URI from `external_url` plus the
received path and raw query. Keep the regular daemon API listener inaccessible
through that ingress: it has a different, unrestricted local policy.

For native TLS, omit `trusted_proxy` and configure `cert_file` and
`tls_key_file`. Use a certificate valid for `external_url`'s host. The native
listener also receives paths with the configured prefix stripped; use an empty
external prefix when clients connect directly. Explicit non-loopback IP binding
is allowed only with native TLS. This feature creates no public routing.

## Configure native clients

```toml
[remote_client]
external_url = "https://reviews.example.com/history"
key_id = "reader-2026"
secret_file = "/run/secrets/review-reader.key"
# Optional private CA bundle; system trust is used otherwise.
ca_file = "/run/secrets/review-ca.pem"
```

`external_url` binds both native credentials to that fixed origin and prefix.
Only a matching explicit HTTPS endpoint selects the signing transport. Other
endpoints retain local transport validation and runtime identity checks. A
configuration change cannot move an existing client's signing credential to a
different origin or prefix.

Select the endpoint explicitly:

```bash
roborev --server https://reviews.example.com/history list --json
roborev --server https://reviews.example.com/history show --job 42
roborev --server https://reviews.example.com/history stream
```

Remote `list` uses server grants instead of inferring a local repository or
branch. Explicit branch filters work; local path filters do not. Remote `show`
requires a numeric `--job`; remote `wait` also requires explicit `--job` IDs.
Remote `stream` rejects `--repo`; its repository scope comes from server grants.
It reconnects after a clean stream close with a fresh signature. Reconnects use
the native job-poll backoff from one to five seconds; received events reset the
delay. Authentication, connection, and read failures stop the command with an
error. Reconnect notices go to stderr; stdout remains JSONL. Events missed
between connections are not recovered. Remote failures never start or restart a
local daemon. The endpoint uses normal HTTPS certificate verification, with an
optional configured CA bundle. It requires no shared local runtime files and
does not weaken local process identity checks. Help, version output, and
shell-completion generation also work with `--server`; they do not contact the
daemon.

Signing occurs after bearer injection at the native HTTP transport boundary.
Generated calls, direct requests, polling, and stream reconnects share that
transport. Each application attempt gets a new random nonce. The client pins
both origin and prefix and follows no redirects, even to the same origin.
Nonrepeatable request bodies fail before sending. Post-send `net/http` retries
are disabled for HTTP/1 and HTTP/2, including bodyless GETs; retry explicitly to
create a fresh signed attempt. Pre-send unusable-connection retries cannot
consume replay admission. HTTP/2 and TLS verification remain enabled. The client
never falls back to unsigned access.

## Wire profile

This fixed profile uses [RFC 9421](https://www.rfc-editor.org/rfc/rfc9421.html)
HTTP Message Signatures and
[RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html) Content-Digest. It
accepts only canonical serialization of this profile, not arbitrary signature
negotiation.

- Label: `sig1`.
- Ordered components: `@method`, `@target-uri`, `content-digest`,
    `content-type`, `authorization`.
- Content type: `application/json`, including bodyless requests.
- Digest: `sha-256=:BASE64:`, including the empty body digest.
- Required parameters: exactly `created`, `expires`, `nonce`, `keyid`, `alg`.
    Any canonical transmitted order is accepted and preserved in the MAC.
- `created` is Unix seconds; `expires` is exactly `created + 30`.
- Maximum future skew: 5 seconds.
- Nonce: 24 random bytes, encoded as 32 unpadded base64url characters.
- Key IDs: 1–128 characters from ASCII letters, digits, `_`, `.`, and `-`.
- Algorithm: `hmac-sha256`; the secret is 64 decoded bytes.

Duplicate covered headers, trailers, content encodings, ambiguous path
encodings, dot segments, and backslashes are rejected. Raw query ordering and
escaping are signed exactly; the restricted policy rejects duplicate query
parameters. Signature metadata is authenticated before body reads. Body digest,
freshness, and replay state must pass before dispatch. Signature errors do not
include credentials or request content.

## Replay, rotation, and recovery

The verifier stores hashed nonce identities and expiry times in a separate
owner-only journal. It holds an exclusive lock on a stable sibling `.lock` file.
Each admission atomically writes and fsyncs state, renames it, then fsyncs its
parent directory before executing the handler. A second process cannot open the
same replay state. Storage must support POSIX owner-only files and these
durability operations; unsupported filesystems fail closed. The verifier and
`signing-init` are not available on Windows because Go file modes cannot enforce
owner-only access there. Windows native clients can use `secret_env` with normal
HTTPS trust.

Pruning expired nonces and advancing the wall-clock highwater happen together.
Clock rollback below that durable watermark, missing or corrupt state, and
uncertain writes reject admission. In-process rollback compares Unix nanoseconds
under the admission lock and remains fenced until wall time catches up. Clock
and cancellation checks run again after hashing and durable publication. Never
delete or reset replay state while retaining signing keys. No startup quarantine
substitutes for durable nonce history. Active replicas must use disjoint signing
secrets and separate journals; sharing a secret between independently cached
replicas is unsupported.

To rotate a key:

1. Generate a new owner-only secret with a new key ID.
1. Add it with explicit grants and restart the verifier. Keep existing replay
    state; adding or removing keys never clears nonce records.
1. Switch clients to the new ID and secret.
1. Remove the old key and restart to revoke it. Restart terminates old streams.

Each key needs distinct secret material, including retained revoked identities.
A key ID is permanently bound to its secret fingerprint in the journal. Reusing
an ID with another secret fails startup. Retained key identities count against
configured replay capacity; select capacity for the expected rotation history.
After journal loss or corruption, generate fresh signing keys and fresh state,
revoke every old key, and update clients before enabling the listener again.

## Release and deployment prerequisites

Use a release containing this feature and the required local auth/TLS fixes.
Before routing traffic, verify scoped HTTPS reads, unsigned rejection, denied
mutations, exact prefix rewriting, replay rejection after restart, and
revocation from outside the private network. Configure only the restricted
listener for remote ingress. Keep PostgreSQL private and reviews executing
locally.

Rollback disables the remote listener and removes its ingress route. Revoke
exposed keys, stop existing streams, and retain replay state for retained keys.
Browser users continue to use browser sessions and CSRF controls; never inject
long-lived HMAC secrets into JavaScript, URLs, or localStorage.

HTTP history replication remains separate work. Existing HTTP export pages are
read-only and do not provide idempotent ingest, conflict resolution, deletions,
or bidirectional cursors. A future protocol must define those contracts before
request signing can support full cross-machine history synchronization.
