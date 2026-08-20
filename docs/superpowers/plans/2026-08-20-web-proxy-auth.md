# Web Proxy Authentication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an explicitly configured HTTPS reverse proxy admit Roborev browser users without a Roborev token while preserving browser sessions and remote-user restrictions.

**Architecture:** Add `web.auth_mode = "proxy"` as a fail-closed configuration mode. Carry the resolved authentication kind into browser policy, allow automatic session minting only for the configured public Host with exact same-origin browser metadata and forwarding headers, and mint a remote principal rather than a privileged local principal.

**Tech Stack:** Go 1.26, `net/http`, BurntSushi TOML configuration, Testify, Svelte/Vitest, Zensical Markdown.

## Global Constraints

- The browser listener remains loopback-only.
- Proxy mode requires an exact HTTPS `public_origin` and forbids `auth_token` and `auth_token_file`.
- Empty `auth_mode` preserves existing local and token behavior; no mode is inferred from a missing token.
- Proxy users retain remote capabilities, CSRF enforcement, process-local sessions, and base-path cookie scope.
- Public tests and documentation contain no private hostnames, paths, or infrastructure details.

---

### Task 1: Validate the proxy authentication configuration

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.WebAuthModeProxy = "proxy"`
- Produces: `WebConfig.AuthMode string`
- Preserves: `WebConfig.ResolveAuthToken() (string, error)` for legacy token modes

- [ ] **Step 1: Write failing table tests for the configuration contract**

Extend `TestWebConfigNormalization` with a returned `wantAuthMode` and these cases:

```go
{
    name: "proxy authentication",
    contents: "[web]\nlisten = \"127.0.0.1:7374\"\npublic_origin = \"https://reviews.example.com\"\nauth_mode = \"proxy\"\n",
    wantOrigin: "https://reviews.example.com",
    wantAuthMode: WebAuthModeProxy,
},
{name: "reject unknown auth mode", contents: "[web]\nauth_mode = \"trusted\"\n", wantErr: "auth mode"},
{name: "reject proxy mode without origin", contents: "[web]\nauth_mode = \"proxy\"\n", wantErr: "public origin"},
{name: "reject proxy mode over HTTP", contents: "[web]\npublic_origin = \"http://127.0.0.1:7374\"\nauth_mode = \"proxy\"\n", wantErr: "HTTPS"},
{name: "reject proxy mode with inline token", contents: "[web]\npublic_origin = \"https://reviews.example.com\"\nauth_mode = \"proxy\"\nauth_token = \"" + strongToken + "\"\n", wantErr: "must not configure"},
{name: "reject proxy mode with token file", contents: "[web]\npublic_origin = \"https://reviews.example.com\"\nauth_mode = \"proxy\"\nauth_token_file = \"/does/not/need/to/exist\"\n", wantErr: "must not configure"},
```

Also add `auth_mode = "unknown"` to `TestDisabledWebConfigIgnoresInactiveSettings` to preserve disabled-section behavior.

- [ ] **Step 2: Run the focused configuration tests and verify failure**

Run: `go test ./internal/config -run 'TestWebConfigNormalization|TestDisabledWebConfigIgnoresInactiveSettings'`

Expected: FAIL because `WebConfig.AuthMode` and `WebAuthModeProxy` do not exist.

- [ ] **Step 3: Add the mode field and fail-closed normalization**

Add the public constant and field:

```go
const WebAuthModeProxy = "proxy"

type WebConfig struct {
    Enabled       bool   `toml:"enabled" comment:"Serve the browser application on a separate listener."`
    Listen        string `toml:"listen" comment:"Loopback browser listener address. Port 0 selects an ephemeral port."`
    PublicOrigin  string `toml:"public_origin" comment:"Exact dedicated HTTPS browser origin used by a reverse proxy."`
    BasePath      string `toml:"base_path" comment:"Optional browser routing prefix; not a same-origin security boundary."`
    AuthMode      string `toml:"auth_mode" comment:"Browser admission mode. Set proxy to trust an external access boundary."`
    AuthToken     string `toml:"auth_token" sensitive:"true" comment:"Token exchanged for a process-local browser session."`
    AuthTokenFile string `toml:"auth_token_file" comment:"Host-local file containing the browser auth token."`
}
```

In `normalizeWebConfig`, accept only `""` and `WebAuthModeProxy`. Before token-file resolution, reject either token setting in proxy mode. Require a nonempty origin, normalize it through `normalizeWebOrigin`, and require the normalized scheme to be `https`. Preserve the existing token requirement when `AuthMode == ""`.

- [ ] **Step 4: Run configuration tests**

Run: `go test ./internal/config`

Expected: PASS.

- [ ] **Step 5: Commit the configuration contract**

Stage `internal/config/config.go` and `internal/config/config_test.go`, run the staged hooks, and commit with subject `feat: add proxy browser auth configuration`.

---

### Task 2: Model proxy admission and remote sessions

**Files:**
- Modify: `internal/daemon/browser_endpoint.go`
- Modify: `internal/daemon/browser_policy.go`
- Modify: `internal/daemon/browser_session.go`
- Test: `internal/daemon/browser_endpoint_test.go`
- Test: `internal/daemon/browser_policy_test.go`
- Test: `internal/daemon/browser_session_test.go`

**Interfaces:**
- Produces: `BrowserEndpoint.authentication string` with `local`, `token`, or `proxy`
- Produces: `BrowserPolicy.AllowsProxySession(*http.Request) bool`
- Produces: `BrowserSessionConfig.AllowProxy bool`
- Produces: `(*BrowserSessionManager).NewProxySession() (SessionCredentials, error)`

- [ ] **Step 1: Write failing endpoint, policy, and session tests**

Add an endpoint test that resolves a loopback listener with HTTPS public origin and `AuthMode: config.WebAuthModeProxy`, then asserts `endpoint.authentication == "proxy"`.

Add `TestBrowserPolicyProxySessionRequiresEveryTrustCondition`. Its valid request has Host `reviews.example.com`, Origin `https://reviews.example.com`, and `X-Forwarded-For: 192.0.2.1`. Assert rejection after independently removing the forwarding header, changing Host to `127.0.0.1:7374`, or changing Origin.

Add a session-manager test using `AllowProxy: true`. Assert `NewProxySession` succeeds, authenticates with `principal.Local == false`, and is rejected by a manager without proxy permission.

- [ ] **Step 2: Run focused daemon tests and verify failure**

Run: `go test ./internal/daemon -run 'TestBrowserEndpointResolution|TestBrowserPolicyProxySession|TestBrowserSession.*Proxy'`

Expected: FAIL because the proxy interfaces do not exist.

- [ ] **Step 3: Resolve the authentication kind at the endpoint**

Replace the endpoint's boolean with an authentication string:

```go
type BrowserEndpoint struct {
    Listener       net.Listener
    Address        string
    DialAddress    string
    Origin         string
    Enabled        bool
    authentication string
}
```

Resolve `proxy` from `web.AuthMode`, `token` from either token setting, and `local` otherwise.

- [ ] **Step 4: Add exact public-host proxy policy**

Track the endpoint-origin authority separately from listener and development authorities. Implement:

```go
func (p BrowserPolicy) AllowsProxySession(request *http.Request) bool {
    if p.authentication != "proxy" || !hasForwardingHeader(request.Header) {
        return false
    }
    host, err := normalizeAuthority(request.Host)
    if err != nil || host != p.publicHost {
        return false
    }
    return p.ValidateOrigin(request) == nil
}
```

Update `AllowsLocalSession` to require `authentication == "local"` while preserving the loopback peer, Host, Origin, and no-forwarding-header checks.

- [ ] **Step 5: Add remote proxy session minting**

Add `AllowProxy`/`allowProxy` and:

```go
func (m *BrowserSessionManager) NewProxySession() (SessionCredentials, error) {
    if !m.allowProxy {
        return SessionCredentials{}, ErrProxyWebSessionDisabled
    }
    return m.newSession(BrowserPrincipal{})
}
```

Do not set `Local: true`; proxy users must retain remote capability restrictions.

- [ ] **Step 6: Run focused and package tests**

Run: `go test ./internal/daemon`

Expected: PASS.

- [ ] **Step 7: Commit the proxy policy and session model**

Stage the six daemon files, run staged hooks, and commit with subject `feat: model trusted proxy browser sessions`.

---

### Task 3: Bootstrap proxy users through the browser handler

**Files:**
- Modify: `internal/daemon/browser_server.go`
- Modify: `internal/daemon/browser_handler.go`
- Modify: `internal/daemon/browser_routes.go`
- Test: `internal/daemon/browser_handler_test.go`
- Test: `internal/daemon/browser_server_test.go`

**Interfaces:**
- Consumes: `BrowserPolicy.AllowsProxySession`
- Consumes: `BrowserSessionManager.NewProxySession`
- Produces: session status `authentication: "proxy"`

- [ ] **Step 1: Write failing handler behavior tests**

Create a proxy fixture with public origin `https://reviews.example.com`, base path `/reviews`, proxy authentication, `AllowProxy: true`, and no token.

Add a valid bootstrap request to `/reviews/api/ui/session/bootstrap` with public Host, exact Origin, all three same-origin fetch headers, and `X-Forwarded-For`. Assert HTTP 200, a cookie scoped to `/reviews/`, nonempty tab and CSRF values, and remote capabilities:

```go
map[string]bool{
    "cancel_any_job": false,
    "cancel_review_job": true,
    "rerun_job": false,
}
```

Table-test rejection after changing Host or Origin, removing the forwarding header, or changing any fetch metadata. Add a stale-cookie case that sends an invalid ambient cookie and still receives a replacement cookie and HTTP 200. Add a mutation check showing a proxy session without the returned CSRF value receives 403.

- [ ] **Step 2: Write failing status and integrated-server tests**

Assert `GET /api/ui/session` reports `authentication: "proxy"` for the proxy fixture. Start the browser server with a normalized proxy configuration and a compilation stub, issue the real bootstrap request through its loopback dial address with the public Host and headers, and assert automatic HTTP 200 without a login call.

- [ ] **Step 3: Run focused tests and verify failure**

Run: `go test ./internal/daemon -run 'TestBrowserHandlerProxy|TestBrowserServerProxy|TestBrowserSessionStatus'`

Expected: FAIL because proxy bootstrap is not wired.

- [ ] **Step 4: Wire server modes into session configuration**

Pass `AllowLocal: endpoint.authentication == "local"` and `AllowProxy: endpoint.authentication == "proxy"` when creating `BrowserSessionManager`. Keep token resolution and cookie path behavior unchanged.

- [ ] **Step 5: Mint automatic sessions after policy validation**

Refactor bootstrap so a missing or stale ambient cookie can fall through to an allowed automatic mode:

```go
if cookieErr == nil {
    credentials, err = sessions.Bootstrap(cookie.Value)
}
if err != nil {
    switch {
    case policy.AllowsLocalSession(request):
        credentials, err = sessions.NewLocalSession()
    case policy.AllowsProxySession(request):
        credentials, err = sessions.NewProxySession()
    default:
        err = ErrWebSessionRequired
    }
    if err == nil {
        http.SetCookie(w, sessions.Cookie(credentials.Ambient))
    }
}
```

Keep `browserBootstrapOriginAllowed` as the first same-origin/fetch-metadata gate. Update status selection to use the policy authentication string and extend the OpenAPI enum to `local,token,proxy`.

- [ ] **Step 6: Run daemon tests**

Run: `go test ./internal/daemon`

Expected: PASS.

- [ ] **Step 7: Commit handler integration**

Stage the five handler/server files, run staged hooks, and commit with subject `feat: bootstrap browser sessions behind trusted proxies`.

---

### Task 4: Document and verify the public product behavior

**Files:**
- Modify: `docs/configuration.md`
- Modify: `docs/web-ui.md`
- Modify: `web/src/App.test.ts`

**Interfaces:**
- Documents: `web.auth_mode = "proxy"`
- Verifies: successful automatic bootstrap never renders the token prompt

- [ ] **Step 1: Pin frontend behavior**

Add or refine an App test whose bootstrap call returns credentials immediately. Assert the review workspace becomes visible and both the `Connect to Roborev` heading and `Daemon token` input are absent. Use a prefixed path to preserve base-path coverage.

- [ ] **Step 2: Run the frontend test**

Run: `nix run nixpkgs#prek -- run web-test`

Expected: PASS without frontend production changes.

- [ ] **Step 3: Update configuration and Browser UI documentation**

Add `web.auth_mode` to the option table. Present token mode and explicit proxy mode as alternatives. State that proxy mode requires HTTPS public origin, forbids both token settings, trusts the external access boundary and whole origin, and does not treat forwarding headers or a base path as authentication or isolation. Preserve the loopback-listener and streaming requirements.

- [ ] **Step 4: Format and run all relevant tests**

Run:

```bash
gofmt -w internal/config/config.go internal/config/config_test.go \
  internal/daemon/browser_endpoint.go internal/daemon/browser_endpoint_test.go \
  internal/daemon/browser_policy.go internal/daemon/browser_policy_test.go \
  internal/daemon/browser_session.go internal/daemon/browser_session_test.go \
  internal/daemon/browser_server.go internal/daemon/browser_server_test.go \
  internal/daemon/browser_handler.go internal/daemon/browser_handler_test.go \
  internal/daemon/browser_routes.go
make test
nix run nixpkgs#prek -- run web-test
make markdown-ci
```

Expected: all commands PASS.

- [ ] **Step 5: Commit docs and browser coverage**

Stage the documentation and App test, run staged hooks, and commit with subject `docs: explain trusted proxy browser access`.

---

### Task 5: Prepare the public pull request

**Files:**
- Delete: `docs/superpowers/specs/2026-08-20-web-proxy-auth-design.md`
- Delete: `docs/superpowers/plans/2026-08-20-web-proxy-auth.md`

**Interfaces:**
- Produces: a public branch containing only product code, tests, and user documentation

- [ ] **Step 1: Remove working design artifacts**

Delete the spec and plan so `docs/superpowers/` is absent from the final PR diff.

- [ ] **Step 2: Run final privacy and verification gates**

Scan the complete branch diff and commit messages with `kenn:scrub-private-data`. Run `git diff --check`, `make test`, `nix run nixpkgs#prek -- run web-test`, `make markdown-ci`, and `nix run nixpkgs#prek -- run` on the staged cleanup.

- [ ] **Step 3: Commit cleanup**

Commit the artifact removal with subject `chore: remove implementation planning artifacts`.

- [ ] **Step 4: Push and open the PR**

Push `feat/web-proxy-auth` and open a public Roborev PR whose title and body describe only the generic proxy-authentication feature. Do not mention any private deployment, hostname, path, tailnet, or infrastructure owner. Do not add a test-plan section or post PR comments.
