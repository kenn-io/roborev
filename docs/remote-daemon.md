---
title: Remote Daemon
description: Read reviews from and queue reviews on a roborev daemon on another machine in your tailnet
---

A roborev client can talk to a daemon running on another machine in the same
[Tailscale](https://tailscale.com) tailnet. You can read reviews from that
daemon and queue new reviews on it, without running a daemon on the client
machine.

What works today:

- Reading jobs, reviews, comments, agent output, and cost and summary data.
- The terminal UI, the MCP stdio server, and review history search.
- Queuing reviews of commits and commit ranges, including from the post-commit
    hook.
- Uploading commits the daemon host does not have yet, such as unpushed work.
- Commenting on, closing, canceling, and rerunning reviews.

What does not work remotely:

- Anything that edits code or runs an agent with write access: `fix`, `refine`,
    `run`, `analyze`, and `compact`.
- Dirty reviews. The daemon cannot see the client's working tree.
- `insights`, `pause`, `unpause`, `snooze`, and `remap`.
- Commands that read or change the local database and log files directly:
    `log <job-id>` and the `repo` subcommands.
- Daemon management, sync, export, and the browser application.

Run those on the daemon host. See the [command reference](#command-reference).

## How It Works

The daemon host opens a second listener on its Tailscale address. The normal
loopback API does not change.

When a client connects to that listener, the daemon runs `tailscale whois` on
the connecting address to learn which tailnet node and user it is. It then
checks your tailnet policy for a roborev grant. The grant decides what the
client may do. There are no tokens or passwords to manage.

```text
client machine                          daemon host
roborev CLI / TUI  --tailnet HTTP-->  remote_api listener (100.x.y.z:7474)
                                         |  tailscale whois -> policy grant
                                         v
                                      same handlers as the loopback API
```

WireGuard encrypts the traffic between tailnet nodes, so the listener serves
plain HTTP.

## Set Up the Daemon Host

1. Install Tailscale and join the tailnet. The daemon's user must be able to run
    `tailscale whois`.

1. Find the host's Tailscale address:

    ```bash
    tailscale ip -4
    ```

1. Add a `[remote_api]` section to the global config, `~/.roborev/config.toml`:

    ```toml
    [remote_api]
    enabled = true
    listen = "100.101.102.103:7474"   # this host's Tailscale IP and a fixed port
    tailscale_path = ""               # optional; empty uses tailscale from PATH
    ```

1. Restart the daemon:

    ```bash
    roborev daemon restart
    ```

    The daemon log shows
    `Remote API listening on 100.101.102.103:7474 (tailnet identity required)`.

Rules for `listen`:

- It must be a literal Tailscale IP address: IPv4 in `100.64.0.0/10` or IPv6 in
    `fd7a:115c:a1e0::/48`. A hostname, `0.0.0.0`, a LAN address, or loopback is
    rejected, because only tailnet peers can be identified.
- It needs a fixed, non-zero port.
- The daemon refuses to start when `listen` breaks either rule. The error names
    the bad value.

The whole daemon also stops if it cannot open the listener at startup, for
example because Tailscale is not up yet and the address is not assigned. The
loopback API and the workers stop with it. Start the daemon after Tailscale is
up. If it stopped, run any roborev command on the host that starts the daemon
automatically, such as `roborev status`, or run `roborev daemon start`.

`remote_api` is global config only. Changes need a daemon restart.

## Grant Access in the Tailnet Policy

The daemon reads the app capability `kenn.io/cap/roborev` from the caller's
tailnet policy grants. Each grant value is an object with an `access` field:

| Access  | What the caller can do                                                                                |
| ------- | ----------------------------------------------------------------------------------------------------- |
| `read`  | Read jobs, reviews, comments, logs, repos, branches, summary, cost, activity, search, and live events |
| `queue` | Everything in `read`, plus queue reviews, upload commits, cancel, rerun, comment, and close reviews   |

A grant covers the whole daemon, not one repo:

- A `read` grant shows every registered repo's reviews and agent output, which
    can quote source code. It also shows the daemon host's checkout paths, which
    often contain the host user's name.
- A `queue` grant can cancel, rerun, close, or comment on any job on the daemon,
    including the host owner's own local reviews.

Add a grant like this to your tailnet policy file:

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

- `src` names who may connect, such as a tag or a user like
    `user-a@example.com`.
- `dst` names the daemon host.
- `ip` opens the listener's port.
- If several grants match, the highest access wins (`queue` over `read`).

The daemon refuses a request with `403` and a reason when:

- `tailscale whois` fails, for example because tailscaled is down or the binary
    is missing. The daemon never falls back to allowing the request.
- The policy gives the node no `kenn.io/cap/roborev` grant.
- A `read` caller tries a `queue` action.
- The route is not available remotely.

The daemon runs `tailscale whois` once per connection and keeps a successful
result for the life of that connection. A failed whois is not kept; the next
request on the connection tries again. A changed or revoked grant takes effect
on the caller's next connection.

The daemon log records the node, user or tags, and access level for every remote
request other than a `GET`.

## Set Up the Client

Point the client at the daemon host in the global config on the client machine:

```toml
[remote]
server = "http://daemon-host.example-tailnet.ts.net:7474"
```

Or set it with the config command:

```bash
roborev config set --global remote.server http://daemon-host.example-tailnet.ts.net:7474
```

You can also pass the URL for a single command. `--server`, and `--addr` for
`roborev tui`, override `[remote] server`:

```bash
roborev --server http://daemon-host.example-tailnet.ts.net:7474 list
roborev tui --addr http://daemon-host.example-tailnet.ts.net:7474
```

The URL must be `http://host:port`. The host can be a MagicDNS name or a
Tailscale IP.

- A loopback URL (`http://127.0.0.1:7373`, `http://localhost:7373`), a plain
    `host:port`, or a `unix://` address keeps the normal local mode. This works
    for both `--server` and `tui --addr`, so you can reach the local daemon for
    one command while `[remote] server` is set.
- If `[remote] server` is set but invalid, commands that contact a daemon fail
    with an error that says how to fix it. They never fall back to a local
    daemon. `roborev config` still works so you can repair the setting.

In remote mode, the client never starts, stops, restarts, or version-checks a
daemon. It only pings the remote daemon. When that fails, the error names the
server and the cause, such as the daemon's `403` reason.

`roborev update` still installs the new binary and repairs hooks and skills in
remote mode, but it leaves any local daemon alone.

## Register Repos on the Daemon Host

The daemon host needs its own clone of each repo, registered with its daemon.
Run this in the clone on the daemon host:

```bash
roborev init
```

The client names a repo by its identity, not by a local path. The identity is
the contents of `.roborev-id` if the repo has one, and otherwise the origin
remote URL with any credentials removed.

- The client and daemon clones must have exactly the same identity.
- If the two clones use different URL forms for origin, such as SSH on one
    machine and HTTPS on the other, add the same `.roborev-id` file to both.
- A repo with no remote and no `.roborev-id` cannot be used remotely.

Running `roborev init` on the client still installs the git hooks, which remote
post-commit reviews need. In remote mode it skips registration and tells you to
register the repo on the daemon host.

If an identity matches no registered repo, the daemon returns `404` and says to
run `roborev init` there or add a matching `.roborev-id`. If it matches more
than one checkout, the daemon returns `409` and lists the paths.

## Queue Reviews

These work the same as with a local daemon:

```bash
roborev review                 # HEAD
roborev review <sha>
roborev review <start> <end>
roborev review --branch
```

The post-commit hook queues remote reviews too.

The client resolves every ref to a full commit SHA before it sends the request,
because the daemon would otherwise resolve names in its own clone.

- Symmetric ranges (`A...B`) are rejected. Use `A..B`.
- Only commit and range reviews can be queued remotely. Dirty reviews, custom
    tasks, insights, analysis, and agentic reviews need a local daemon.
- Reruns are limited the same way. A review panel reruns only when every member
    is a non-agentic commit or range review.

The review runs on the daemon host with that host's settings, including the
repo's `.roborev.toml` in the daemon's clone. The branch comes from the client:

- It is the branch you named with `--branch`, the branch of a post-commit batch,
    or your current branch. On a detached HEAD, the client sends no branch and
    the daemon tries to infer one from its own refs.
- The daemon uses that branch for display, for `excluded_branches`, and for the
    `branches` filter of [review event hooks](/docs/guides/hooks/).

Hooks fire for remote reviews the same way they do for local ones.

The post-commit hook must finish within `hook_timeout_seconds` (3 seconds by
default, 30 on Windows). The hook reads this setting on the client machine, from
the client's global config or the repo's `.roborev.toml`, not from the daemon
host. A remote review that needs a fetch or an upload can take longer. Raise the
timeout if remote post-commit reviews fail. Failures are recorded in the hook
log and never block the commit.

## Unpushed Commits

When a review names a commit the daemon clone does not have, the daemon runs
`git fetch --all` once in that clone. If the commit is still missing, the client
uploads it automatically:

1. The daemon answers with the missing SHAs and the commits at the tips of its
    own refs.
1. The client builds a git pack of only the missing history and uploads it.
1. The daemon imports the pack and pins each uploaded commit under
    `refs/roborev/uploads/<sha>`, so git garbage collection keeps it.
1. The client queues the review again.

Uploads write only git objects and those pin refs. They never touch the daemon
clone's working tree, index, or branches.

Pins are removed only after the daemon fetches for a remote review. That fetch
happens when a later remote review names a commit the daemon clone lacks. After
a successful fetch, the daemon deletes every pin whose commit is now on a
remote-tracking branch. Pushing alone does not remove a pin, and neither does a
fetch from another source, such as a manual `git fetch` on the daemon host.

If an upload depends on history the daemon clone lacks, the daemon returns `409`
and asks you to fetch on the daemon host.

## Command Reference

| Works remotely                                                          | Needs a local daemon                                     |
| ----------------------------------------------------------------------- | -------------------------------------------------------- |
| `status`, `list`, `show`, `wait`, `stream`, `search`, `summary`, `cost` | `daemon` subcommands, `ui`, `sync now`, `export`         |
| `review` for commits, ranges, and branches                              | `review --dirty`                                         |
| `comment`, `close`, `cancel`, `tui`, `mcp serve`                        | `fix`, `refine`, `run`, `analyze`, `compact`, `insights` |
| `init` (hooks only), `update` (binary, hooks, and skills only)          | `pause`, `unpause`, `snooze`, `remap`                    |
|                                                                         | `log <job-id>`, `repo` subcommands                       |

A command that needs a local daemon fails in remote mode before it contacts
anything, with a message like:

```text
roborev fix needs a local daemon; the remote daemon at http://daemon-host.example-tailnet.ts.net:7474 cannot run it
```

The post-rewrite hook calls `remap`, which exits quietly in remote mode.

To read a job's agent output remotely, open its log in the TUI.

Through `mcp serve`, the tools that read reviews, comment, and close work. The
snooze and hook fix completion tools get the daemon's `403`, because those
routes are not available remotely.

Agent hooks track local agent sessions, so they ignore `[remote] server` and use
the daemon on the same machine. In remote mode they only look for a local daemon
that is already running. They never start one.

## Repo Filters

Commands and the TUI filter by repo. Against a remote daemon, a repo filter can
be:

- A repo identity. The daemon maps it to its own checkout.
- The exact daemon-side path of a registered repo, as `/api/repos` and the TUI
    repo picker report it. The client sends it unchanged, so this works for
    repos you have never cloned.

For `roborev tui --repo`, pass a local checkout, which the client turns into the
daemon's path for that repo, or an absolute daemon path. With a daemon path,
`--branch` needs an explicit value: `--branch=<name>`.

Path-prefix filters do not work remotely. `roborev search --repo` with a repo
name, rather than an identity or path, returns `404`.

## Routes Over the Remote Listener

The remote listener serves only these API routes:

| Access  | Routes                                                                                                                                                                      |
| ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `read`  | `GET` ping, status, health, jobs, review, search, comments, repos, branches, summary, cost, activity, job/output, job/log, stream/events; `POST` jobs/batch                |
| `queue` | Everything in `read`, plus `POST` enqueue, job/cancel, job/rerun, review/close, comment, remote/pack                                                                       |

Every other route returns `403` with
`<route> is not available over the remote API; run it on the daemon host`. That
includes repo registration, remap, fix jobs and patches, sync, pausing,
shutdown, update, agent-hook routes, exports, the browser application, and
`/mcp`. For browser access from another machine, see
[Browser UI](/docs/web-ui/#private-network-access).

## Do Not Put a Proxy in Front

Connect clients directly to the remote listener. A reverse proxy in front of it
breaks identification: every request would come from the proxy's address, so
whois would identify the proxy instead of the real caller.

Some setups forward a hostname through a reverse proxy to the daemon's loopback
API. That path has no authentication at all. Anyone who can reach the proxy gets
every route, including the ones the remote listener refuses. roborev cannot
authenticate callers that come through such a proxy, so securing it is your
responsibility. Replace it with `[remote_api]` and a tailnet policy grant.

## Security

- **A grant covers every repo on the daemon.** There is no per-repo access. A
    `read` caller sees all reviews, agent output, and daemon-host paths. A
    `queue` caller can cancel, rerun, close, or comment on any job.
- **Queue access spends your quota.** Anyone the `queue` grant covers can make
    the daemon host run review agents on its agent accounts. Grant `queue` only
    to people and machines you trust with that.
- **Hooks fire for remote reviews.** Review event hooks on the daemon host run
    for reviews queued remotely, with the branch the client reported. Hooks that
    act on the `branches` filter trust that branch name. The daemon rejects a
    value that is not a valid branch name.
- **Uploads add data to your clone.** A `queue` caller can store git objects and
    `refs/roborev/uploads/*` refs in any registered clone. There is no size
    limit.
- **Fetches run with your git config.** A remote review of a missing commit runs
    `git fetch --all` in the daemon clone as the daemon's user, with that user's
    credentials.
- **Nothing edits code.** Remote callers cannot run agentic reviews, fixes, or
    tasks, so no remote request leads to an agent writing to the daemon host's
    checkout.
