# Web Proxy Authentication Design

## Problem

Roborev requires a browser token whenever `web.public_origin` is not loopback.
That is appropriate when Roborev must authenticate each browser itself, but it
duplicates authentication for deployments whose reverse proxy and private
network are already the access-control boundary. Users on those networks must
retrieve and enter an application token even though the proxy is intentionally
the only route to the loopback-bound browser listener.

## Goals

- Add an explicit mode that delegates browser admission to an operator-managed
  reverse proxy or private network.
- Require no Roborev browser token in that mode.
- Keep the browser listener loopback-bound and the CLI listener unexposed.
- Preserve exact Host and Origin validation, same-origin fetch checks, browser
  sessions, tab credentials, CSRF checks, stream expiry, and remote-operation
  restrictions.
- Preserve all existing local and token-authenticated configurations.
- Fail closed when proxy mode is incomplete, contradictory, or misspelled.

## Configuration Contract

`web.auth_mode = "proxy"` explicitly enables proxy authentication:

```toml
[web]
enabled = true
listen = "127.0.0.1:7374"
public_origin = "https://reviews.example.com"
base_path = "/reviews"
auth_mode = "proxy"
```

The empty `auth_mode` retains current behavior. Local loopback browser access
can bootstrap automatically, while a non-loopback `public_origin` requires
`auth_token` or `auth_token_file`.

Proxy mode requires:

- a loopback `listen` address;
- a nonempty, exact HTTPS `public_origin`; and
- neither `auth_token` nor `auth_token_file`.

Only the empty value and `proxy` are accepted. Unknown values fail
normalization. Proxy mode is explicit and is never inferred from a missing
token.

## Request And Session Flow

The browser application keeps its existing startup flow and first posts to
`/api/ui/session/bootstrap`. In proxy mode, Roborev creates a session only when
the request:

- uses the configured public Host;
- has an Origin that exactly matches `public_origin`;
- has `Sec-Fetch-Site: same-origin`, `Sec-Fetch-Mode: cors`, and
  `Sec-Fetch-Dest: empty`; and
- carries a conventional forwarding header, matching the configured proxy
  request shape rather than the direct loopback request shape.

Missing or invalid values receive the existing bootstrap rejection. A direct
request using the loopback listener Host cannot use proxy bootstrap. Existing
ambient cookies can still bootstrap new tab credentials after the same request
checks pass. A stale ambient cookie from a daemon restart is replaced with a
new proxy session after those checks pass, so proxy mode never falls back to a
token prompt.

Proxy bootstrap creates the same process-local ambient session, tab-scoped
session credential, and CSRF credential as token login. Its principal is remote,
not local, so proxy users keep the existing restricted mutation capabilities.
Restart, expiry, and logout invalidate proxy sessions exactly as they invalidate
token sessions. The login endpoint does not become an alternate tokenless path.

The session-status endpoint reports `authentication: "proxy"` in this mode.
The browser needs no mode-specific user interface: successful bootstrap opens
the application, while rejected bootstrap retains the existing login-required
or error behavior.

## Trust Model

Proxy mode delegates admission to the complete external origin and its network
path. Roborev does not validate a Tailscale identity, proxy user header, or
source IP allowlist. The operator must ensure that only intended users can reach
the public origin and that untrusted clients cannot connect directly to the
loopback listener.

Forwarding headers are request-shape validation, not cryptographic proxy
authentication. Loopback binding is the daemon-to-proxy trust boundary. A
process already running on the daemon host is inside that boundary.

All content on the configured origin is trusted. A URL base path is routing,
not isolation: scripts from sibling paths on the same origin can make
same-origin requests to Roborev. Operators that do not trust all applications
on one origin must give Roborev a dedicated origin or retain token mode.

## Compatibility And Error Handling

Existing token settings and token-file resolution are unchanged when
`auth_mode` is empty. Existing direct-loopback automatic sessions are
unchanged. Disabled web configuration continues to ignore inactive web
settings.

Configuration errors identify the violated contract: unsupported auth mode,
missing HTTPS public origin, or token settings combined with proxy mode. The
daemon fails before opening the browser listener.

## Tests

Configuration tests cover valid proxy mode, unknown modes, missing origin,
non-HTTPS origin, non-loopback listener, both forbidden token settings, and
unchanged legacy token and local configurations.

Daemon tests cover:

- automatic proxy bootstrap with the configured Host, Origin, fetch metadata,
  and forwarding headers;
- rejection for a loopback Host, forged or missing Origin, cross-site fetch
  metadata, and missing forwarding headers;
- remote capabilities and CSRF enforcement after proxy bootstrap;
- session status reporting `proxy`;
- base-path cookie scope and request routing; and
- token login and direct-loopback bootstrap regressions.

Browser coverage confirms that automatic bootstrap reaches the application
without rendering the token prompt. Existing frontend bootstrap behavior
requires no new branch when the daemon returns credentials.

## Documentation

The configuration reference and Browser UI guide document proxy mode as an
explicit alternative to token authentication. Examples use generic private
network and reverse-proxy language. The guide warns that proxy mode trusts the
whole origin and that base paths do not isolate sibling applications.
