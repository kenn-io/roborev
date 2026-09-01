# Native UUID migration design

## Goal

Use Go 1.27's standard `uuid.UUID` type for every UUID value that Roborev
owns. Keep string conversion at explicit foreign text boundaries and prevent
new source imports of `github.com/google/uuid`.

## Scope

The migration covers UUID values in storage models, sync records, daemon
request and response types, command and TUI state, exported data structs,
tests, and generated Go clients. It includes job, review, response, panel-run,
resume-source, source-machine, review-unit, and CI panel UUIDs.

Values that are not UUID identifiers stay in their existing semantic type.
For example, opaque cursors, session IDs, commit hashes, database IDs, and
update drain tokens do not become `uuid.UUID` merely because existing code
uses UUID-shaped text to create them.

## Type rules

- Required UUID values use `uuid.UUID`.
- Optional JSON UUID values use `*uuid.UUID`, so omission remains distinct
  from the nil UUID.
- Nullable `database/sql` scan values use `sql.Null[uuid.UUID]`.
- Functions accept and return `uuid.UUID` while the value remains inside
  Roborev-owned code.
- New UUIDs come directly from `uuid.New()`. The custom `GenerateUUID` helper
  and Google UUID constructors are removed.
- `uuid.Nil()` represents the UUID zero value when an owned contract requires
  a value. It does not replace absence in optional contracts.

## Persistence boundaries

The SQLite and PostgreSQL schemas keep their current column types. The wire
representation remains the canonical UUID string, so this change does not
rewrite stored values or introduce a schema migration.

Go 1.27 `database/sql` scans string and byte values into `uuid.UUID`, converts
`uuid.UUID` query arguments to strings, and supports `sql.Null[uuid.UUID]`.
SQLite code uses those standard paths.

Empty text is never scanned or parsed as a UUID. Queries for nullable UUID
columns stop using `COALESCE(column, '')` and preserve SQL `NULL`, using
`NULLIF(column, '')` where existing rows may contain an empty sentinel. The
`ci_pr_review_attempts.last_panel_run_uuid` column keeps its current
`TEXT NOT NULL DEFAULT ''` schema, but its Go model uses `*uuid.UUID`; reads
normalize the empty sentinel to SQL `NULL`, and writes encode nil as the
column's required empty sentinel at that database boundary.

The generic `sync_state.value` column remains a text boundary. `GetMachineID`
continues to treat an empty stored value as missing and regenerate it; a
non-empty value is parsed once before returning `uuid.UUID`. These conversions
preserve current storage semantics without introducing a second Go
representation.

PostgreSQL code uses pgx directly. UUID columns pass native UUID values where
pgx supports them. Existing text columns convert only at the query boundary.
Each necessary `.String()` or `uuid.Parse` call carries a `forbidigo` nolint
comment that names that database boundary.

Existing nullable rows remain nullable. The migration does not add fallback
parsing, dual fields, wrapper UUID types, or a second storage representation.

## JSON and API contracts

Standard `uuid.UUID` text marshaling preserves the current JSON string shape.
Huma fields use `format:"uuid"` so generated OpenAPI documents mark UUID
values with `format: uuid`.

Both checked-in Go clients use `uuid.UUID` and `*uuid.UUID`. The OpenAPI
generation step adds `x-go-type: uuid.UUID` with the standard-library import to
UUID schemas before invoking either Go generator. This overrides the
generators' legacy Google UUID defaults without editing generated files.

TypeScript clients continue to expose JSON UUID values as strings because
JavaScript has no UUID scalar type.

The public Go client change is intentionally compile-breaking for callers that
construct or compare UUID fields as plain strings. The JSON protocol does not
change.

## Lint enforcement

Enable `forbidigo` with type analysis and forbid:

- `(uuid.UUID).String`, unless a nolint comment names the foreign text
  boundary.
- `uuid.Parse`, unless a nolint comment names the foreign text boundary.

Enable `depguard` and reject source imports of `github.com/google/uuid`, with a
message directing contributors to Go 1.27's `uuid` package.

Remove Roborev's direct `github.com/google/uuid` requirement. The module may
remain an indirect dependency while the OpenAPI generator runtime imports it;
Roborev source and generated source must not import it.

## Testing and verification

Tests exercise behavior owned by Roborev:

- storage writes and reads preserve required, optional, and nullable UUIDs;
- non-panel jobs and empty `last_panel_run_uuid` values hydrate as absent UUIDs,
  and an empty stored machine ID still regenerates;
- API handlers accept valid UUIDs, reject invalid UUID text where UUID input is
  required, and keep the existing JSON string representation;
- OpenAPI generation emits UUID formats and both generated Go clients compile
  with the standard UUID type;
- sync, panel, rerun, and export paths preserve UUID identity across their
  existing workflows.

The migration reuses existing integration coverage instead of adding tests for
the standard library's UUID, SQL, or JSON implementation. Final verification
runs API generation checks, focused storage and daemon tests, the full Go test
suite, and the repository lint command.

## Non-goals

- Changing database column types or stored UUID text.
- Adding compatibility aliases or accepting both string and UUID fields in
  owned Go contracts.
- Converting non-UUID identifiers to UUIDs.
- Replacing the OpenAPI generators or their runtimes.
