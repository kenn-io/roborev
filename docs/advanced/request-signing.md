---
title: Native Request Signing
description: Signed, restricted HTTPS access to daemon history
---

Native clients can read daemon history over an explicitly configured HTTPS
endpoint. An optional restricted listener accepts requests signed with a
per-reader signing key. It exposes no execution or administrative operations.
Remote readers never receive the daemon `auth_key`. Existing local clients and
browser sessions keep their existing behavior.

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

## Create a signing key

Generate a fresh key without printing the secret:

```bash
roborev signing-init --secret-file /run/secrets/review-reader.key
```

The destination must be a new file in an existing private directory.
Initialization syncs the file and its parent directory before reporting success.
If it fails, it removes only the file it created. The secret file contains 128
lowercase hex characters representing 64 random bytes. Keep it owner-only. A
named environment variable containing the same hex format can replace
`secret_file` with `secret_env`; never put the secret in command arguments,
URLs, source files, or browser storage.

The signing key is the only credential a remote reader needs. The listener
ignores the daemon `auth_key`, and readers never hold it, so a reader cannot use
it against the local API, MCP, or browser login. Give each reader its own key so
you can scope and revoke readers independently.

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

# Operator-selected ingress budgets; these are examples, not defaults.
max_header_bytes = 16384
max_concurrent = 4
read_header_timeout = "5s"
read_timeout = "30s"
request_timeout = "1m"

[[remote.keys]]
id = "reader-2026"
secret_file = "/run/secrets/review-reader.key"
grants = ["history:read"]
repo_ids = [1, 2]
```

Choose budgets from the actual ingress contract and workload. The header budget
is bytes; concurrency is simultaneous admitted requests, including streams. Time
budgets are Go durations. Every budget is required and positive. Saturated
request slots return 503. The listener serves reads only, so any request body is
rejected with 413 instead of being read or stored. The read deadline bounds the
empty-body check and is cleared afterward. `request_timeout` bounds execution
and stream lifetime. Each stream write, including the final response frame, also
gets up to `request_timeout` to complete. Idle streams have no socket write
deadline, so planned expiry can close the response cleanly. Clients must
reconnect with a fresh signature. These budgets apply only to the new restricted
listener.

Use repository numeric IDs from the local daemon's repository listing. Every key
needs explicit `repo_ids`, or `all_repos = true` instead. Unknown repository IDs
prevent startup. Empty grants never become access to every repository.
`history:read` permits `/api/jobs`, `/api/review`, and `/api/comments`; ping is
available to authenticated keys. `history:events` separately permits
`/api/stream/events`. No mutation grant exists.

Scoped job listings apply stable repository IDs before pagination and
statistics. Repository IDs are never reused after deletion; renaming or
reassigning a repository path cannot transfer a grant either. ID lookups recheck
the owning repository after body verification and replay admission. Legacy SHA
comment joins use that repository too, and apply only to single-commit reviews.
Dirty reviews and stored-prompt jobs show their own comments. Review and comment
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
`tls_key_file`. Use a certificate valid for `external_url`'s host. Clients
connect directly, so `external_url` must not include a path prefix; the daemon
rejects that configuration at startup. Explicit non-loopback IP binding is
allowed only with native TLS. The certificate is loaded at startup, so restart
the daemon after renewing it. This feature creates no public routing.

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
requires a numeric `--job` and fetches the review and its comments before
printing output. A failed comment read returns an error rather than a partial
review. The server includes eligible legacy comments in that response; the
client does not fetch them separately. Remote `wait` also requires explicit
`--job` IDs.

Remote `stream` rejects `--repo`; its repository scope comes from server grants.
It reconnects after a clean stream close with a fresh signature. Reconnects use
the native job-poll backoff from one to five seconds; received events reset the
delay. Authentication, connection, and read failures stop the command with an
error. A single reconnect notice goes to stderr; stdout remains JSONL. Events
missed between connections are not recovered.

Remote failures never start or restart a local daemon. The endpoint uses normal
HTTPS certificate verification, with an optional configured CA bundle. It
requires no shared local runtime files and does not weaken local process
identity checks. Help, version output, and shell-completion generation also work
with `--server`; they do not contact the daemon.

Signing occurs at the native HTTP transport boundary. The client sends no daemon
bearer token. Generated calls, direct requests, polling, and stream reconnects
share that transport. Each application attempt gets a new random nonce. The
client pins both origin and prefix and follows no redirects, even to the same
origin. Nonrepeatable request bodies fail before sending. Post-send `net/http`
retries are disabled for HTTP/1 and HTTP/2, including bodyless GETs; retry
explicitly to create a fresh signed attempt. Pre-send unusable-connection
retries cannot consume a nonce. HTTP/2 and TLS verification remain enabled. The
client never falls back to unsigned access.

## Wire profile

This fixed profile uses [RFC 9421](https://www.rfc-editor.org/rfc/rfc9421.html)
HTTP Message Signatures and
[RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html) Content-Digest. It
accepts only canonical serialization of this profile, not arbitrary signature
negotiation.

- Label: `sig1`.
- Ordered components: `@method`, `@target-uri`, `content-digest`,
    `content-type`.
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
parameters. Signature metadata is authenticated before any body read. The
listener accepts only the empty-body digest. Freshness and nonce checks must
pass before dispatch. Signature errors do not include credentials or request
content.

## Replay and rotation

The verifier keeps used nonces in memory until their signatures expire, so each
signed request runs at most once. After a restart, it rejects every signature
created before the new process started. A request admitted by the previous
process therefore cannot run again, and no replay state is stored on disk.
Clients retry with a fresh signature. A wall-clock step backward can briefly let
a request whose nonce was already pruned verify again; TLS remains the primary
protection against capture.

The verifier and `signing-init` are not available on Windows because Go file
modes cannot enforce owner-only access there. Windows native clients can use
`secret_env` with normal HTTPS trust.

To rotate a key:

1. Generate a new owner-only secret with a new key ID.
1. Add it with explicit grants and restart the verifier.
1. Switch clients to the new ID and secret.
1. Remove the old key and restart to revoke it. Restart terminates old streams.

## Release and deployment prerequisites

Use a release containing this feature and the required local auth/TLS fixes.
Before routing traffic, verify scoped HTTPS reads, unsigned rejection, denied
mutations, exact prefix rewriting, replay rejection after restart, and
revocation from outside the private network. Configure only the restricted
listener for remote ingress. Keep PostgreSQL private and reviews executing
locally.

Rollback disables the remote listener and removes its ingress route. Revoke
exposed keys and stop existing streams. Browser users continue to use browser
sessions and CSRF controls; never inject long-lived HMAC secrets into
JavaScript, URLs, or localStorage.

HTTP history replication remains separate work. Existing HTTP export pages are
read-only and do not provide idempotent ingest, conflict resolution, deletions,
or bidirectional cursors. A future protocol must define those contracts before
request signing can support full cross-machine history synchronization.
