# Remote daemon access over a tailnet

## Purpose

Let a roborev client on one machine read reviews from, and queue reviews on,
a roborev daemon running on another machine in the same Tailscale tailnet.
This replaces running an unauthenticated reverse proxy in front of the
daemon's loopback API.

## Decisions

| Area                 | Decision                                                                                                                                                          |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Listener             | A separate, opt-in remote listener. The loopback API and Unix socket stay unchanged and unauthenticated.                                                           |
| Authentication       | Tailscale identity. The daemon asks tailscaled who the connecting peer is. There are no tokens.                                                                    |
| Whois transport      | Run `tailscale whois --json <peer-ip:port>`. No Tailscale Go dependency.                                                                                           |
| Authorization        | A tailnet policy grant of the app capability `kenn.io/cap/roborev`. The grant value selects the access level.                                                      |
| Access levels        | `read` and `queue`. Admin and host-path operations are never remote.                                                                                              |
| Repo mapping         | The client sends its repo identity. The daemon maps it to a repo already registered on the daemon host.                                                           |
| Missing commits      | The daemon fetches once from the clone's configured remotes. If commits are still missing, the client uploads them as a git pack.                                  |
| Uploaded commits     | Pinned under `refs/roborev/uploads/<sha>`. The daemon deletes an upload ref once a remote-tracking branch contains the commit.                                     |
| Dirty reviews        | Rejected for remote clients.                                                                                                                                     |
| Hooks and branch     | Remote reviews fire daemon hooks like local ones. The client-reported branch is used for display and hook `branches` matching.                                    |
| Client configuration | Global `[remote] server = "http://<host>:<port>"`. `--server` accepts the same URL. A remote client never starts or restarts a local daemon.                      |
| kit                  | No change. `kitdaemon.RequireLoopback` still guards the loopback API.                                                                                              |

## What works remotely

| Level   | Operations                                                                                                                                             |
| ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `read`  | `GET` ping, status, health, jobs, review, search, comments, repos, branches, summary, cost, activity, job/output, job/log, stream/events; `POST` jobs/batch |
| `queue` | Everything in `read`, plus enqueue, job/cancel, job/rerun, review/close, comment, remote/pack                                                           |

Every other route answers `403` with
`"<route> is not available over the remote API; run it on the daemon host"`.
That includes repo register, remap, repos/resolve, fix jobs and patches,
job/applied, job/rebased, job/update-branch, sync, queue pause, token backfill,
shutdown, update, agent-hook routes, exports, UI routes, and `/mcp`.

Remote enqueue accepts only commit-based reviews: job type `review` or `range`,
including panel fan-out. It rejects dirty, task, insights, compact, fix,
analyze, and agentic requests with `403` and a message that names the job type
and says it needs a local daemon.

## Daemon side

### Configuration

```toml
[remote_api]
enabled = true
listen = "100.101.102.103:7474"   # this host's Tailscale address
tailscale_path = ""               # optional; default: "tailscale" from PATH
```

- `listen` must be a literal Tailscale address: IPv4 in `100.64.0.0/10` or IPv6
  in `fd7a:115c:a1e0::/48`. Startup fails with an actionable error otherwise.
  Only tailnet peers can be identified by whois, so no other address is useful.
- The listener serves plain HTTP. WireGuard encrypts tailnet traffic.
- A reverse proxy in front of this listener breaks authentication, because
  every request then comes from the proxy. The docs say so.
- `remote_api` is global config only, like `server_addr`.

### Request authentication

For each new TCP connection on the remote listener, the daemon runs
`tailscale whois --json <remote-addr>` before serving the first request. The
result is cached for the life of that connection. A revoked grant takes effect
on the peer's next connection.

- A whois failure (tailscaled down, binary missing, or unknown peer) returns
  `403` with a message naming the cause. The daemon never falls back to
  allowing the request.
- The daemon reads the top-level `CapMap["kenn.io/cap/roborev"]`. Each entry is a
  JSON object with an `access` field. The highest level present wins
  (`queue` > `read`). If there is no entry or no known level, the response is
  `403 "tailnet policy grants this node no roborev access"`.
- The caller's identity (node name, login name or tags, access level) goes
  into the request context and the daemon log for each mutation.

Example tailnet policy grant:

```json
{
  "grants": [{
    "src": ["tag:dev"],
    "dst": ["tag:roborev"],
    "ip":  ["tcp:7474"],
    "app": { "kenn.io/cap/roborev": [{ "access": "queue" }] }
  }]
}
```

### Route allowlist

A remote handler wraps the existing core mux. It checks the route against the
table above and the caller's access level, then forwards to the same handlers
the loopback API uses. `routeKey` and the allowlist pattern follow
`browser_handler.go`.

### Repo identity

`EnqueueRequest` gains `repo_identity`. The `jobs` and `branches` queries gain
a `repo_identity` parameter.

- On the remote listener, `repo_path` and `repo` (path filters) are rejected.
  Clients must send `repo_identity`.
- The daemon resolves the identity with `GetRepoByIdentity`. It then replaces
  the request's `repo_path` with that repo's `RootPath` and calls the normal
  handler.
- An unknown identity returns `404`
  `"repo <identity> is not registered on the daemon host; run roborev init there"`.
- The loopback API accepts `repo_identity` too, but it is only required
  remotely.

### Missing commits

Before calling the enqueue handler, the daemon checks that every commit the
request names exists in the resolved clone. For a range, that is both
endpoints. The check uses `git cat-file -e <sha>^{commit}`.

1. If any commit is missing, run `git fetch --all --quiet` once in the clone.
   A fetch error is logged and treated as "still missing".
2. If commits are still missing, return `409` with
   `{"error": "...", "code": "missing_commits", "missing": ["<sha>", ...]}`.

### Pack upload

`POST /api/remote/pack?repo_identity=<id>` (queue level) takes a git pack as
the raw `application/octet-stream` body, plus `tip` query parameters naming the
commits to pin.

1. Stream the body into `git index-pack --stdin` run in the resolved clone.
   Git validates the pack, and objects land in the clone's object store.
2. For each tip, run `git rev-list --objects <tip> --not --all` to confirm
   the tip and everything it needs is present. On failure, return `409`
   `"daemon clone lacks base commits for <tip>; fetch on the daemon host"`.
3. For each tip, run `git update-ref refs/roborev/uploads/<tip> <tip>`.

No size limit is added. There is no measured constraint that calls for one.
The upload never touches the working tree, the index, branches, or
remote-tracking refs.

### Upload ref cleanup

After each remote fetch in the missing-commit path, the daemon deletes every
`refs/roborev/uploads/<sha>` whose commit is an ancestor of some
`refs/remotes/*` tip. Pruning failures are logged, not returned.

### Hooks and branch

Remote enqueues follow the local path completely. Hooks fire on completion,
and the request's `branch` feeds display and `HookBranch()`. The remote client
always sends `branch` (it errors if one can't be determined for a branch
review), so the daemon never substitutes its own checkout's branch. The daemon
rejects a branch that is not a valid ref name (`git check-ref-format --branch`).

## Client side

### Configuration

```toml
[remote]
server = "http://daemon-host.example-tailnet.ts.net:7474"
```

`--server` accepts the same `http://host:port` form. A loopback or Unix
`--server` keeps today's behavior. Any other host puts the client in remote
mode. `--server` overrides `[remote] server`.

### Remote mode

- `ensureDaemon` becomes a single `GET /api/ping` probe. It never starts,
  restarts, stops, or version-checks a daemon. When the probe fails, the
  message names the server and the underlying error.
- The TUI (`--addr` follows the same rule), MCP stdio, and the agent-hook
  client use the remote endpoint the same way.
- Requests that took a local path send `repo_identity`, computed locally with
  `config.ResolveRepoIdentity`.
- Commands that need a local daemon fail before sending anything. The message
  names the command and says it needs a local daemon. These include dirty
  reviews, `fix`, `refine`, `init`/repo registration, `remap`, daemon
  lifecycle commands, `sync`, `run`, `analyze`, and `compact`.
- A remote `403` or `404` body is shown to the user as returned.

### Queuing and uploads

The post-commit hook, `roborev review <sha>`, and `roborev review --branch`
share one remote enqueue helper.

1. Send the enqueue with `repo_identity`, the git ref, and the branch.
2. On `409 missing_commits`, build a pack:
   `git pack-objects --revs --stdout`, with stdin listing each missing SHA and
   `^<tip>` for every local `refs/remotes/*` ref. Upload it to
   `/api/remote/pack` with `tip` set to each missing SHA.
3. Retry the enqueue once. A second failure is returned as-is.

The post-commit hook keeps its current batching and quiet-failure behavior.

## Errors

| Condition                          | Status | Message                                                                     |
| ---------------------------------- | ------ | --------------------------------------------------------------------------- |
| Whois fails                        | 403    | `tailscale whois failed for <peer>: <cause>`                                |
| No grant                           | 403    | `tailnet policy grants this node no roborev access`                         |
| Read-level caller mutates          | 403    | `<route> requires queue access; this node has read access`                  |
| Route not remote-capable           | 403    | `<route> is not available over the remote API; run it on the daemon host` |
| Unsupported remote job type        | 403    | `<type> reviews need a local daemon`                                        |
| Unknown identity                   | 404    | `repo <identity> is not registered on the daemon host; run roborev init there` |
| Commits missing after fetch        | 409    | `missing_commits` with the SHA list                                        |
| Pack lacks base commits            | 409    | `daemon clone lacks base commits for <tip>; fetch on the daemon host`      |

## Testing

- Remote listener: address validation, and whois outcomes (granted read,
  granted queue, no grant, whois failure) through a fake `tailscale` script on
  `tailscale_path`.
- Allowlist: each level against a read route, a queue route, and a denied
  route.
- Identity mapping: known, unknown, and `repo_path` rejected remotely.
- Missing commits: present, fetched, and still missing (`409` with SHAs).
- Pack upload: a real pack from a test repo imports and pins; a pack without
  base commits returns `409`; upload refs are pruned once reachable from a
  remote-tracking ref.
- Client: `--server` and `[remote]` parsing; remote mode never starts a
  daemon; local-only commands fail with the documented message; the enqueue
  helper uploads and retries on `409`.

## Documentation

- New `docs/` page on remote daemon access: daemon config, the tailnet policy
  grant, client config, what works remotely, and the note about reverse
  proxies.
- Update the `AGENTS.md` and `CLAUDE.md` design constraints to name the upload
  path: remote pack imports write objects and `refs/roborev/uploads/*` refs
  into a registered clone and never touch its working tree.
