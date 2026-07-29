# CI ACP Snapshot Validation

## Problem

CI resolves configuration from the repository's default branch, then freezes
the named ACP execution entries needed by each queued job. Snapshot creation
currently ignores an `acp.<name>` reference when that entry is missing. The
worker interprets an empty ACP snapshot as a job created before snapshotting
existed and resolves the job against the live working tree instead.

This lets an invalid newly queued job cross the configuration boundary and use
an ACP command that was not present in the default-branch configuration that
the poller evaluated.

## Design

Snapshot creation will distinguish built-in agents from named ACP references.
Built-in agents require no ACP snapshot entry. Every canonical `acp.<name>`
primary or backup reference must resolve in the effective default-branch plus
global ACP configuration; otherwise snapshot creation returns an error.

`buildPanelOpts` will propagate that error for panel members and synthesis jobs
before calling `CreateCIPanelRun`. No new job with an unresolved named ACP
reference can therefore be persisted. Existing jobs that genuinely predate ACP
snapshotting retain the current worker fallback behavior.

## Error Handling

The error will identify the unresolved canonical ACP agent and the member or
synthesis snapshot being built. The poller will abort the panel enqueue instead
of silently omitting the configuration.

## Test

A focused `buildPanelOpts` regression will create a working tree containing an
`[acp.goose]` entry while passing default-branch configuration without that
entry. A member or synthesis reference to `acp.goose` must return an error and
produce no enqueue options. This proves that live working-tree configuration
cannot rescue invalid default-branch configuration.
