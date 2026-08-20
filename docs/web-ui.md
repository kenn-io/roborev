---
title: Browser UI
description: Browse reviews and analyze project review health in the native Roborev web application
---

Roborev includes a browser application in its release packages. It is served by
the same daemon that owns the review queue and SQLite database, on a separate
browser-only listener.

<figure class="hero-shot" data-lightbox>
  <img src="/assets/generated/web-ui.png" alt="Roborev browser application showing the review queue and an open review" loading="eager">
</figure>

## Open the Application

For normal local use, install a Roborev release and run:

```bash
roborev ui
```

The command starts the daemon when needed and opens its browser application. No
web configuration is required: the browser listener is enabled on loopback and
uses an available port by default.

Open a particular review with its local job ID:

```bash
roborev ui 42
```

This opens `/reviews/42`, or the same route below `web.base_path` when a path
prefix is configured. Job IDs belong to one daemon's SQLite database, so a
numeric review URL is not portable between machines.

The application has two workspaces:

- **Reviews** provides the live queue, filters, logs, prompts, rendered review
    output, comments, and review actions.
- **Analytics** summarizes cost, latency, failures, outcomes, and agent attempts
    from the daemon's SQLite history.

The Go module source archive does not contain the generated application assets.
An installation made with `go install` still provides the CLI and terminal UI,
but the native browser application requires a release package or a source build
made with `make build`.

## Reviews Workspace

The Reviews workspace follows the daemon's queue in real time. Use the
repository and branch picker, status filter, ref search, and closed-review
toggle to narrow the list. Columns can be sorted after all matching rows are
loaded. Panel reviews appear as a synthesis row that can be expanded to show its
individual reviewers.

Select a row to open its detail drawer without leaving the queue. The drawer
provides:

- rendered review output and existing comments;
- the persisted agent log and the exact stored prompt;
- close or reopen, rerun, and eligible cancellation actions; and
- review-output copying and a comment form.

Available actions depend on both the job and the browser session. For example, a
running review can be canceled but has no completed output to close, while
remote sessions have the narrower mutation policy described under
[Browser Sessions](#browser-sessions).

The most common keyboard controls are:

| Key | Action |
|-----|--------|
| `j` / `k` | Move down or up through the queue |
| `Enter` | Open the selected review |
| `Left` / `Right` | Collapse or expand a review panel |
| `/` | Focus ref search |
| `a` | Close or reopen the open review |
| `c` | Focus its comment field |
| `l` / `p` | Show its log or prompt |
| `y` | Copy its review output |
| `Esc` | Close the detail drawer |
| `?` | Show all keyboard shortcuts |

Review deep links and Analytics filters are stored in the URL. They can be
bookmarked on one daemon, but numeric job IDs and local data are not portable to
a different daemon.

## Analytics

Open **Analytics** in the application shell, or navigate directly to
`/analytics` (below `web.base_path` when configured). Filters are encoded in the
URL, so a time range and project, source, agent, model, or bucket selection can
be bookmarked and shared with another user of the same daemon.

Project analytics use the display names shown elsewhere in Roborev. Repositories
with the same display name are intentionally grouped together.

The page separates two populations:

- A **logical review** is a standalone review, range review, dirty review,
    compact review, or panel synthesis parent. Panel member jobs are not counted
    as additional logical reviews.
- An **agent attempt** is a terminal job that actually invoked an agent. Cost
    and attempt latency use this population.

The headline values have these meanings:

- **Failure rate** is failing review verdicts divided by all rated reviews:
    `F / (P + F)`. Both open and addressed failures remain failures. Reviews
    without a parsed verdict do not enter the denominator.
- **Run errors** are logical reviews whose daemon jobs failed to complete. They
    are operational errors, not failing review verdicts, and are reported
    separately with canceled and skipped reviews.
- **Review latency** runs from enqueue to finish. A panel synthesis parent's
    latency therefore includes the complete panel run. Percentiles use linear
    interpolation.
- **Estimated cost** includes eligible attempts that have a readable cost in
    their token-usage data. It counts attempts by `finished_at` within the
    selected half-open time range and is not an external billing total.
- **Pricing coverage** is priced eligible attempts divided by all eligible
    attempts. When coverage is incomplete, estimated cost is a lower bound.
    Retries clear the prior job's token-usage data, so historical retried work
    can also be undercounted.
- **Outcomes** partition verdicts into pass, fail-open, and fail-addressed.
    Addressed means the review's mutable `closed` state is true. Historical
    outcome mixes can therefore change when a user closes or reopens a review.

Agent and model filters apply to attempt metrics. Logical-review metrics retain
their review-based population rather than silently changing meaning.

Analytics refresh when the page opens, when its filters change, when the browser
regains focus, or when **Refresh** is selected. If a refresh for the current
filters fails, the page labels the existing snapshot as stale and offers a
retry. It never displays results from old filters under a new filter selection.

## Private Network Access

Remote browser access requires an HTTPS reverse proxy. Keep both the CLI
listener and browser listener on loopback; expose only the browser listener
through the proxy. Roborev rejects a non-loopback browser listener because the
daemon-to-proxy connection uses plain HTTP.

Choose whether Roborev authenticates each browser with a token or delegates
admission to an external proxy and private-network boundary.

### Token Authentication

Choose a fixed loopback port. Generate a 32-byte base64url token:

```bash
openssl rand -base64 32 | tr '+/' '-_' | tr -d '='
```

Store the generated value in a host-local file readable by the Roborev daemon,
with an optional single trailing newline, and configure that file in
`~/.roborev/config.toml`:

```toml
[web]
enabled = true
listen = "127.0.0.1:7374"
public_origin = "https://reviews.example.com"
base_path = "/reviews"
auth_token_file = "/etc/roborev/web-auth-token"
```

`public_origin` must be the exact origin users open, without a path or trailing
slash, and that origin must serve only Roborev-controlled content. Serve sibling
applications from separate origins. `base_path` is an optional canonical path
prefix: it starts with `/`, has no trailing slash, and must be preserved by the
reverse proxy. It cannot contain a percent escape, backslash, control character,
or surrounding whitespace. The browser session cookie is scoped to that prefix
to reduce incidental transmission, but same-origin scripts can still make
requests below it. The prefix provides routing, not isolation. `auth_token_file`
and `auth_token` are mutually exclusive; the token file must contain exactly one
token. Protect the token file and restart the daemon after changing any `[web]`
setting:

```bash
roborev daemon restart
```

The restart output prints the canonical `Web UI` URL. You can retrieve it again
at any time with `roborev daemon status`; if browser serving is disabled, both
commands report `Web UI: unavailable` explicitly.

### Proxy Authentication

When the reverse proxy and private network already restrict access to intended
users, enable proxy mode and omit both token settings:

```toml
[web]
enabled = true
listen = "127.0.0.1:7374"
public_origin = "https://reviews.example.com"
base_path = "/reviews"
auth_mode = "proxy"
```

Proxy mode requires an exact HTTPS `public_origin`. It automatically creates a
remote browser session after validating the public Host, exact Origin,
same-origin browser fetch metadata, and conventional forwarding headers. It does
not grant owner-local capabilities: proxy users retain the existing remote
mutation restrictions and every mutation still requires a tab-scoped CSRF
credential.

Roborev does not verify proxy identity, tailnet identity, or a proxy user
header. The external origin and network path are the admission boundary.
Forwarding headers confirm the expected request shape but are not
authentication. Keep the listener loopback-bound and ensure the public origin is
not reachable outside the intended network.

Proxy mode trusts every application on the configured origin. Scripts at a
sibling path can make same-origin requests below Roborev's `base_path`, so a
base path is not isolation. Use a dedicated origin unless every sibling
application is equally trusted.

### Tailscale Serve

[Tailscale Serve](https://tailscale.com/docs/reference/tailscale-cli/serve) can
provide the private HTTPS reverse proxy. Configure the fixed listener above,
then run:

```bash
tailscale serve --bg http://127.0.0.1:7374
```

For tokenless tailnet access, set `auth_mode = "proxy"`, use the HTTPS origin
printed by `tailscale serve` as `web.public_origin`, and restart Roborev. Open
that origin on another device in the tailnet; no Roborev token prompt appears.
To retain Roborev token authentication, omit `auth_mode` and configure one token
source instead. A path-mounted deployment needs a reverse proxy that preserves
the configured `base_path`.

Do not combine tokenless proxy mode with Tailscale Funnel. Serve keeps access
inside the tailnet; Funnel makes the service public and therefore removes the
network admission boundary.

### Other Reverse Proxies

An alternative proxy must:

- terminate HTTPS and preserve the public `Host`;
- set conventional forwarding headers;
- pass streaming responses without buffering, especially `/api/stream/events`
    and `/api/job/output?stream=1`; and
- route only the browser listener, never the CLI listener.

Forwarding headers do not authenticate a request. In proxy mode, network and
proxy policy admit the user; Roborev then validates the exact Host, Origin, and
same-origin fetch metadata before creating a browser session.

## Browser Sessions

For loopback-only use without `web.auth_token`, Roborev creates a local browser
session automatically. Owner-local browser bootstrap also supports clients that
omit the `Origin` header, but only for a direct loopback connection with an
exact loopback Host, no forwarding headers, no remote authentication, and no
cross-site fetch metadata. In token mode, every browser, including one on the
daemon host, must enter the token. In proxy mode, browsers admitted through the
configured public origin bootstrap automatically.

In token mode, login exchanges the token for an HTTP-only cookie and credentials
stored in the current tab's `sessionStorage`. The application does not retain
the daemon token. Proxy mode creates the same credentials automatically after
admission checks pass. Opening a deep link in a new tab uses the cookie to
bootstrap fresh tab-scoped credentials.

Browser sessions are held in memory. Restarting or upgrading the daemon signs
out every browser tab. Token users must enter the token again; proxy users
receive a replacement session automatically on their next valid bootstrap.
Sessions are never written to disk. Logging out or reaching the session expiry
also closes any active event or job-output streams.

Remote browser sessions can cancel only standalone, non-agentic, non-CI code
reviews and cannot rerun jobs. Panel and CI cancellations, reruns, and
stored-prompt workflows remain available through the loopback CLI API or an
automatically bootstrapped local browser session. The application hides cancel
and rerun controls when the current browser session or selected job does not
permit them.

Invalid token exchanges trigger a process-wide exponential cooldown of up to one
minute. The daemon checks a valid token before applying that cooldown, so a
valid login remains available and resets the failure count immediately.

Disable the browser listener with:

```bash
roborev config set web.enabled false --global
roborev daemon restart
```

With the listener disabled, `roborev ui` reports that browser access is
unavailable instead of opening a dead URL.
