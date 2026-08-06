---
title: PostgreSQL Sync
description: Sync reviews across multiple machines using PostgreSQL
---

Sync reviews across multiple machines using PostgreSQL as a central store while
keeping SQLite as the local primary database.

## Purpose

The primary purpose is to consolidate reviews from multiple machines into a
single database without duplicates. Each review job, review, and response has a
UUID that uniquely identifies it across all machines, so PostgreSQL ends up with
exactly one copy of each item regardless of which machines have synced.

## Configuration

```toml
# ~/.roborev/config.toml
[sync]
enabled = true
postgres_url = "postgres://roborev:${ROBOREV_PG_PASS}@nas.tailnet:5432/roborev"
interval = "1h"           # Sync interval (default: 1h)
machine_name = "laptop"   # Friendly name for this machine
```

!!! note

    Sync configuration is not hot-reloaded. After changing `[sync]` settings in
    `config.toml`, restart the daemon with `roborev daemon restart` for changes to
    take effect.

## Key Features

- **UUID-based deduplication**: Each job, review, and response has a globally
    unique UUID. Local review numeric IDs may differ between machines, but UUIDs
    ensure PostgreSQL stores only one copy of each item.
- **Local-first**: SQLite remains the source of truth. CLI commands query SQLite
    only.
- **Graceful degradation**: If PostgreSQL is unreachable, everything continues
    to work. Sync resumes when connectivity returns.
- **Schema isolation**: Tables are created in a dedicated `roborev` schema,
    avoiding conflicts with other applications.
- **Remote indicator**: Jobs pulled from other machines show `[remote]` in the
    TUI and status output.

## Commands

```bash
roborev sync status    # Show sync configuration and pending items
roborev sync now       # Trigger immediate sync
```

## What Gets Synced

- Completed jobs (done, failed, canceled)
- Reviews with closed/open state
- Responses/notes

Jobs in `queued` or `running` states remain local-only until they complete.

## Credential Expansion

Sensitive values can reference environment variables:

```toml
postgres_url = "postgres://roborev:${ROBOREV_PG_PASS}@host:5432/db"
```

Set the password in your shell:

```bash
export ROBOREV_PG_PASS="secret"
```

Alternatively, keep the password in a file and reference its absolute path:

```toml
postgres_url = "postgres://roborev:${file:/run/secrets/roborev-postgres-password}@host:5432/db"
```

Store the raw password in the file; roborev trims leading and trailing
whitespace and percent-encodes URL-reserved characters before substituting it
into the URL. Ensure the account running the daemon can read the file, and
restrict the file's permissions to that account. If the file is missing or
unreadable, configuration validation warns about it and the reference expands to
an empty value, causing the PostgreSQL connection to fail.

File references and environment variables can be used together in the same URL.
Restart the daemon after editing `postgres_url`. Updating only the contents of a
referenced password file does not require a restart; roborev reads it again on
the next connection attempt.

## Changing Sync Servers

When you change the `postgres_url` to a different server or reset the database,
roborev automatically detects this and performs a full resync:

- Each PostgreSQL database has a unique identifier stored in a `sync_metadata`
    table
- On connection, roborev compares this ID with the last synced database
- If different, all local `synced_at` timestamps and pull cursors are cleared
- This triggers a complete bidirectional sync: all local data is pushed, and all
    remote data is pulled

This means you can safely:

- Switch between PostgreSQL servers (e.g., dev to prod)
- Reset/recreate your PostgreSQL database
- Sync the same local database to multiple PostgreSQL instances over time

The sync will never be left in an inconsistent state where it thinks data was
synced but the target database doesn't have it.

## See Also

- [Configuration](/configuration/) - Global configuration options
- [Repository Management](/guides/repository-management/) - Managing repos
