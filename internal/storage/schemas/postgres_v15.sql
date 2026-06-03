-- PostgreSQL schema version 15
-- On top of v14 (responses.inserted_at), adds subagent review panel columns
-- (panel_run_uuid, panel_role, panel_name, panel_member_name,
-- panel_member_index, panel_member_config_json) and job-level failover
-- override columns (backup_agent, backup_model) to review_jobs. claim_blocked
-- is local-only (SQLite scheduling gate) and intentionally NOT present here.
-- Also defines ci_pr_review_attempts (local CI-poller retry state) appended to
-- v15 for schema parity (not sync-replicated).
-- Note: Version is managed by EnsureSchema(), not this file.

CREATE SCHEMA IF NOT EXISTS roborev;

CREATE TABLE IF NOT EXISTS roborev.schema_version (
  version INTEGER PRIMARY KEY,
  applied_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS roborev.machines (
  id SERIAL PRIMARY KEY,
  machine_id UUID UNIQUE NOT NULL,
  name TEXT,
  last_seen_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS roborev.repos (
  id SERIAL PRIMARY KEY,
  identity TEXT UNIQUE NOT NULL,
  created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS roborev.commits (
  id SERIAL PRIMARY KEY,
  repo_id INTEGER REFERENCES roborev.repos(id),
  sha TEXT NOT NULL,
  author TEXT NOT NULL,
  subject TEXT NOT NULL,
  timestamp TIMESTAMP WITH TIME ZONE NOT NULL,
  created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
  UNIQUE(repo_id, sha)
);

CREATE TABLE IF NOT EXISTS roborev.review_jobs (
  id SERIAL PRIMARY KEY,
  uuid UUID UNIQUE NOT NULL,
  repo_id INTEGER NOT NULL REFERENCES roborev.repos(id),
  commit_id INTEGER REFERENCES roborev.commits(id),
  git_ref TEXT NOT NULL,
  branch TEXT,
  session_id TEXT,
  agent TEXT NOT NULL,
  model TEXT,
  provider TEXT,
  requested_model TEXT,
  requested_provider TEXT,
  reasoning TEXT,
  job_type TEXT NOT NULL DEFAULT 'review',
  review_type TEXT NOT NULL DEFAULT '',
  patch_id TEXT,
  status TEXT NOT NULL CHECK(status IN ('queued', 'running', 'done', 'failed', 'canceled', 'applied', 'rebased', 'skipped')),
  agentic BOOLEAN DEFAULT FALSE,
  enqueued_at TIMESTAMP WITH TIME ZONE NOT NULL,
  started_at TIMESTAMP WITH TIME ZONE,
  finished_at TIMESTAMP WITH TIME ZONE,
  retry_not_before TIMESTAMP WITH TIME ZONE,
  prompt TEXT,
  diff_content TEXT,
  error TEXT,
  token_usage TEXT,
  worktree_path TEXT,
  min_severity TEXT NOT NULL DEFAULT '',
  backup_agent TEXT NOT NULL DEFAULT '',
  backup_model TEXT NOT NULL DEFAULT '',
  skip_reason TEXT,
  source TEXT,
  panel_run_uuid TEXT,
  panel_role TEXT,
  panel_name TEXT,
  panel_member_name TEXT,
  panel_member_index INTEGER,
  panel_member_config_json TEXT,
  source_machine_id UUID NOT NULL,
  created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
  updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS roborev.reviews (
  id SERIAL PRIMARY KEY,
  uuid UUID UNIQUE NOT NULL,
  job_uuid UUID NOT NULL REFERENCES roborev.review_jobs(uuid),
  agent TEXT NOT NULL,
  prompt TEXT NOT NULL,
  output TEXT NOT NULL,
  closed BOOLEAN NOT NULL DEFAULT FALSE,
  updated_by_machine_id UUID NOT NULL,
  created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
  updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS roborev.responses (
  id SERIAL PRIMARY KEY,
  uuid UUID UNIQUE NOT NULL,
  job_uuid UUID NOT NULL REFERENCES roborev.review_jobs(uuid),
  responder TEXT NOT NULL,
  response TEXT NOT NULL,
  source_machine_id UUID NOT NULL,
  created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
  inserted_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT clock_timestamp()
);

-- ci_pr_review_attempts holds local CI-poller retry state keyed by
-- (github_repo, pr_number, head_sha). It is the durable source of truth for
-- whether a HEAD is being reviewed and, when an AI-provider outage defers it,
-- when to retry. One row per reviewed HEAD. next_attempt_at is NULL while a
-- run is in-flight or pending and set once deferred. state is one of
-- 'pending', 'deferred', or 'done'. This table is created in both the SQLite
-- and Postgres backends for schema parity per the design, but it is NOT
-- registered in any sync cursor (not sync-replicated). Avoid inline
-- semicolons in this comment -- pgSchemaStatements splits the embedded
-- Postgres schema on semicolons, so a literal one here would fragment the
-- comment into a bad statement.
CREATE TABLE IF NOT EXISTS roborev.ci_pr_review_attempts (
  id BIGSERIAL PRIMARY KEY,
  github_repo TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  head_sha TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 1,
  first_attempt_at TIMESTAMPTZ NOT NULL,
  next_attempt_at TIMESTAMPTZ,
  last_error_class TEXT NOT NULL DEFAULT '',
  consecutive_genuine_attempts INTEGER NOT NULL DEFAULT 0,
  last_error_excerpt TEXT NOT NULL DEFAULT '',
  last_panel_run_uuid TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'pending',
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(github_repo, pr_number, head_sha)
);

CREATE INDEX IF NOT EXISTS idx_review_jobs_source ON roborev.review_jobs(source_machine_id);
CREATE INDEX IF NOT EXISTS idx_review_jobs_updated ON roborev.review_jobs(updated_at);
-- Note: idx_review_jobs_branch, idx_review_jobs_job_type,
-- idx_review_jobs_patch_id, and idx_review_jobs_panel are created by
-- migration code, not here (to support upgrades from older versions
-- where those columns don't exist yet — the embedded schema is replayed
-- on every startup, including against older databases).
CREATE INDEX IF NOT EXISTS idx_reviews_job_uuid ON roborev.reviews(job_uuid);
CREATE INDEX IF NOT EXISTS idx_reviews_updated ON roborev.reviews(updated_at);
CREATE INDEX IF NOT EXISTS idx_responses_job_uuid ON roborev.responses(job_uuid);
CREATE INDEX IF NOT EXISTS idx_responses_id ON roborev.responses(id);
-- idx_responses_inserted is created by EnsureSchema after migrations so
-- upgrades from older schemas do not try to index a column before it exists.
-- Partial unique indexes for auto-design dedup are created by EnsureSchema
-- (fresh-init block + v12 migration step) — placing them here would break
-- v1->v12 migrations where the source column doesn't yet exist when this
-- schema is replayed.

CREATE TABLE IF NOT EXISTS roborev.sync_metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
