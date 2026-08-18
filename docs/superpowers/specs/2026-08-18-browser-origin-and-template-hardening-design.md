# Browser Origin and Template Hardening Design

## Goal

Keep reverse-proxy path mounting while making the browser application's
security boundary and production HTML contract explicit and testable.

## Security boundary

Remote browser access must use an origin dedicated to Roborev. A configured
`base_path` changes URL routing only. It does not isolate Roborev from scripts
served at other paths on the same origin.

The session cookie remains scoped to `base_path + "/"` when a prefix is set.
That scope reduces incidental cookie transmission, but it is not an
authorization boundary. Same-origin scripts can issue requests to any path and
can satisfy Roborev's origin and fetch-metadata checks.

Roborev cannot detect whether a reverse proxy serves another application on
the configured origin. The dedicated-origin requirement is therefore an
operational configuration contract. The generated configuration comments and
browser documentation will state this requirement. Existing text that claims
path scoping isolates credentials from sibling applications will be removed.

## Production HTML injection

The production template contains two canonical, self-closing markers:

```html
<meta name="roborev-base-path" content="" />
<base href="/" />
```

The embedded handler will match those exact markers and replace each marker
exactly once. Handler construction will fail with a descriptive error when a
production distribution is missing either marker or contains a duplicate. This
turns a broken build/template contract into a startup failure instead of a UI
that silently sends requests to root paths.

The compilation stub does not contain browser application markup and will keep
its existing behavior. Marker validation applies only to production
distributions. Injected values remain HTML escaped.

## Tests

Unit tests will load the real `web/index.html` template, inject a synthetic
prefix, and verify both rendered markers. Table-driven failure cases will cover
missing and duplicate markers.

The release-asset check will construct the embedded handler from the real Vite
output and verify prefixed HTML injection. Existing browser end-to-end tests
will continue to verify that direct deep links load under the content security
policy.

Focused package tests, the release-asset check, the full Go suite, browser
tests, and repository hooks will run before the implementation is committed and
pushed.

## Public communication

Public changes will describe only the generic dedicated-origin requirement,
path-routing behavior, and template contract. They will not include deployment
hostnames, machine names, network topology, or proxy configuration from any
specific installation.
