#!/usr/bin/env bash
# Prepare an isolated, deterministic demo database for docs screenshots.

set -euo pipefail

DEMO_DIR="${TMPDIR:-/tmp}/roborev-demo-data"
DEST_DB="$DEMO_DIR/reviews.db"

mkdir -p "$DEMO_DIR"
rm -f "$DEST_DB" "$DEST_DB-wal" "$DEST_DB-shm"

echo "Creating synthetic fixture database..."
echo "Destination: $DEST_DB"
echo ""

sqlite3 "$DEST_DB" <<'SCHEMA'
PRAGMA foreign_keys = ON;

CREATE TABLE repos (
  id INTEGER PRIMARY KEY,
  root_path TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  identity TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE commits (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  sha TEXT NOT NULL,
  author TEXT NOT NULL,
  subject TEXT NOT NULL,
  timestamp TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(repo_id, sha)
);

CREATE TABLE review_jobs (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  commit_id INTEGER REFERENCES commits(id),
  git_ref TEXT NOT NULL,
  branch TEXT,
  ci_base_branch TEXT,
  session_id TEXT,
  agent TEXT NOT NULL DEFAULT 'codex',
  model TEXT,
  requested_model TEXT,
  requested_provider TEXT,
  reasoning TEXT NOT NULL DEFAULT 'thorough',
  status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed','canceled','applied','rebased','skipped')) DEFAULT 'queued',
  enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
  started_at TEXT,
  finished_at TEXT,
  worker_id TEXT,
  error TEXT,
  prompt TEXT,
  retry_count INTEGER NOT NULL DEFAULT 0,
  diff_content TEXT,
  dirty_files TEXT,
  output_prefix TEXT,
  job_type TEXT NOT NULL DEFAULT 'review',
  review_type TEXT NOT NULL DEFAULT '',
  provider TEXT,
  skip_reason TEXT,
  source TEXT,
  backup_agent TEXT NOT NULL DEFAULT '',
  backup_model TEXT NOT NULL DEFAULT '',
  agentic INTEGER NOT NULL DEFAULT 0,
  prompt_prebuilt INTEGER NOT NULL DEFAULT 0,
  patch_id TEXT,
  parent_job_id INTEGER,
  patch TEXT,
  worktree_path TEXT DEFAULT '',
  min_severity TEXT NOT NULL DEFAULT '',
  command_line TEXT,
  token_usage TEXT,
  retry_not_before TIMESTAMP,
  panel_run_uuid TEXT,
  panel_role TEXT,
  panel_name TEXT,
  panel_member_name TEXT,
  panel_member_index INTEGER NOT NULL DEFAULT 0,
  panel_member_config_json TEXT,
  claim_blocked INTEGER NOT NULL DEFAULT 0,
  uuid TEXT,
  source_machine_id TEXT,
  updated_at TEXT,
  synced_at TEXT,
  agent_invoked INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE reviews (
  id INTEGER PRIMARY KEY,
  job_id INTEGER UNIQUE NOT NULL REFERENCES review_jobs(id),
  agent TEXT NOT NULL,
  prompt TEXT NOT NULL,
  output TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  closed INTEGER NOT NULL DEFAULT 0,
  verdict_bool INTEGER,
  uuid TEXT,
  updated_at TEXT,
  updated_by_machine_id TEXT,
  synced_at TEXT
);

CREATE TABLE responses (
  id INTEGER PRIMARY KEY,
  commit_id INTEGER REFERENCES commits(id),
  job_id INTEGER REFERENCES review_jobs(id),
  responder TEXT NOT NULL,
  response TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  uuid TEXT,
  source_machine_id TEXT,
  synced_at TEXT
);

CREATE INDEX idx_review_jobs_status ON review_jobs(status);
CREATE INDEX idx_review_jobs_repo ON review_jobs(repo_id);
CREATE INDEX idx_review_jobs_git_ref ON review_jobs(git_ref);
CREATE UNIQUE INDEX idx_review_jobs_uuid ON review_jobs(uuid);
CREATE INDEX idx_review_jobs_branch ON review_jobs(branch);
CREATE INDEX idx_review_jobs_panel ON review_jobs(panel_run_uuid, panel_role, panel_member_index);
CREATE INDEX idx_review_jobs_patch_id ON review_jobs(patch_id);
CREATE INDEX idx_commits_sha ON commits(sha);
CREATE INDEX idx_reviews_closed ON reviews(closed);
CREATE INDEX idx_reviews_verdict_bool ON reviews(verdict_bool);
CREATE UNIQUE INDEX idx_reviews_uuid ON reviews(uuid);
CREATE INDEX idx_responses_job_id ON responses(job_id);
CREATE UNIQUE INDEX idx_responses_uuid ON responses(uuid);

CREATE TABLE sync_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE daemon_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO repos (id, root_path, name, identity, created_at) VALUES
  (1, '/repos/roborev', 'roborev', 'github.com/kenn-io/roborev', '2026-06-20 09:00:00'),
  (2, '/repos/agentsview', 'agentsview', 'github.com/kenn-io/agentsview', '2026-06-20 09:00:00'),
  (3, '/repos/msgvault', 'msgvault', 'github.com/kenn-io/msgvault', '2026-06-20 09:00:00');

INSERT INTO commits (id, repo_id, sha, author, subject, timestamp, created_at) VALUES
  (1, 1, 'f13a001', 'Fixture Maintainer', 'Tighten review queue rendering', '2026-06-20 09:12:00', '2026-06-20 09:12:00'),
  (2, 2, 'a9c4e12', 'Fixture Maintainer', 'Add project filter persistence', '2026-06-20 09:25:00', '2026-06-20 09:25:00'),
  (3, 3, '0d7b8aa', 'Fixture Maintainer', 'Validate export retention settings', '2026-06-20 09:40:00', '2026-06-20 09:40:00'),
  (4, 1, '55ca7de', 'Fixture Maintainer', 'Stream daemon activity updates', '2026-06-20 09:58:00', '2026-06-20 09:58:00'),
  (5, 2, '6e018d0', 'Fixture Maintainer', 'Refresh navigation shortcuts', '2026-06-20 10:05:00', '2026-06-20 10:05:00'),
  (6, 3, '9ab731c', 'Fixture Maintainer', 'Classify small styling change', '2026-06-20 10:20:00', '2026-06-20 10:20:00'),
  (7, 1, 'c4e9902', 'Fixture Maintainer', 'Apply review follow-up patch', '2026-06-20 10:35:00', '2026-06-20 10:35:00'),
  (8, 1, 'd86d284', 'Fixture Maintainer', 'Rebase generated docs assets', '2026-06-20 10:50:00', '2026-06-20 10:50:00');

INSERT INTO review_jobs (
  id, repo_id, commit_id, git_ref, branch, session_id, agent, model, requested_model,
  requested_provider, reasoning, status, enqueued_at, started_at, finished_at,
  worker_id, error, prompt, retry_count, diff_content, dirty_files, output_prefix,
  job_type, review_type, provider, skip_reason, source, backup_agent, backup_model,
  agentic, prompt_prebuilt, patch_id, parent_job_id, patch, worktree_path,
  min_severity, command_line, token_usage, panel_member_index, claim_blocked,
  uuid, updated_at, agent_invoked
) VALUES
  (101, 1, 8, 'd86d284', 'docs/assets', 'fixture-session-8', 'codex', 'gpt-5', 'gpt-5',
   'openai', 'standard', 'rebased', '2026-06-20 10:50:00', '2026-06-20 10:50:12', '2026-06-20 10:51:18',
   'fixture-worker-1', NULL, 'fixture prompt: verify the generated docs asset branch rebase.',
   0, 'diff --git a/docs/assets/generated/tui-hero.svg b/docs/assets/generated/tui-hero.svg', '[]', '',
   'review', 'code', 'openai', '', 'manual', '', '', 0, 0, NULL, NULL, NULL, '',
   '', 'roborev review d86d284', '{"input":3112,"output":642}', 0, 0,
   '00000000-0000-4000-8000-000000000101', '2026-06-20 10:51:18', 1),
  (102, 1, 7, 'c4e9902', 'review/follow-up', 'fixture-session-7', 'codex', 'gpt-5', 'gpt-5',
   'openai', 'standard', 'applied', '2026-06-20 10:35:00', '2026-06-20 10:35:08', '2026-06-20 10:36:44',
   'fixture-worker-1', NULL, 'fixture prompt: review a synthetic follow-up patch before apply.',
   0, 'diff --git a/internal/daemon/server.go b/internal/daemon/server.go', '["internal/daemon/server.go"]', '',
   'review', 'code', 'openai', '', 'manual', '', '', 1, 0, NULL, NULL, NULL, '',
   '', 'roborev refine --once', '{"input":4821,"output":905}', 0, 0,
   '00000000-0000-4000-8000-000000000102', '2026-06-20 10:36:44', 1),
  (103, 3, 6, '9ab731c', 'style/spacing', 'fixture-session-6', 'codex', 'gpt-5-mini', 'gpt-5-mini',
   'openai', 'standard', 'skipped', '2026-06-20 10:20:00', '2026-06-20 10:20:06', '2026-06-20 10:20:22',
   'fixture-worker-2', NULL, 'fixture prompt: classify whether this synthetic styling change needs design review.',
   0, 'diff --git a/web/styles.css b/web/styles.css', '["web/styles.css"]', '',
   'review', 'design', 'openai', 'fixture classifier determined no design-sensitive UI changed', 'auto_design', '', '',
   0, 0, NULL, NULL, NULL, '',
   '', 'roborev review --design 9ab731c', '{"input":1202,"output":128}', 0, 0,
   '00000000-0000-4000-8000-000000000103', '2026-06-20 10:20:22', 1),
  (104, 2, 5, '6e018d0', 'keyboard/shortcuts', 'fixture-session-5', 'codex', 'gpt-5', 'gpt-5',
   'openai', 'thorough', 'canceled', '2026-06-20 10:05:00', NULL, '2026-06-20 10:05:30',
   NULL, 'fixture cancellation requested before review started', 'fixture prompt: review navigation shortcut updates.',
   0, 'diff --git a/ui/shortcuts.ts b/ui/shortcuts.ts', '["ui/shortcuts.ts"]', '',
   'review', 'code', 'openai', '', 'manual', '', '', 0, 0, NULL, NULL, NULL, '',
   '', 'roborev review 6e018d0', NULL, 0, 0,
   '00000000-0000-4000-8000-000000000104', '2026-06-20 10:05:00', 0),
  (105, 1, 4, '55ca7de', 'daemon/activity', 'fixture-session-4', 'codex', 'gpt-5', 'gpt-5',
   'openai', 'thorough', 'done', '2026-06-20 09:58:00', '2026-06-20 09:58:18', '2026-06-20 10:00:04',
   'fixture-worker-3', NULL, 'fixture prompt: review daemon activity stream changes.',
   0, 'diff --git a/internal/daemon/activity.go b/internal/daemon/activity.go', '["internal/daemon/activity.go"]', '',
   'review', 'code', 'openai', '', 'manual', '', '', 0, 0, NULL, NULL, NULL, '',
   '', 'roborev review 55ca7de', '{"input":2980,"output":412}', 0, 0,
   '00000000-0000-4000-8000-000000000105', '2026-06-20 09:58:18', 1),
  (106, 3, 3, '0d7b8aa', 'exports/retention', 'fixture-session-3', 'codex', 'gpt-5', 'gpt-5',
   'openai', 'thorough', 'failed', '2026-06-20 09:40:00', '2026-06-20 09:40:11', '2026-06-20 09:43:05',
   'fixture-worker-2', NULL, 'fixture prompt: review export retention logic.',
   0, 'diff --git a/internal/export/retention.go b/internal/export/retention.go', '["internal/export/retention.go"]', '',
   'review', 'security', 'openai', '', 'manual', '', '', 0, 0, NULL, NULL, NULL, '',
   'medium', 'roborev review --security 0d7b8aa', '{"input":5220,"output":1180}', 0, 0,
   '00000000-0000-4000-8000-000000000106', '2026-06-20 09:43:05', 1),
  (107, 2, 2, 'a9c4e12', 'projects/filter', 'fixture-session-2', 'codex', 'gpt-5', 'gpt-5',
   'openai', 'standard', 'done', '2026-06-20 09:25:00', '2026-06-20 09:25:09', '2026-06-20 09:27:12',
   'fixture-worker-1', NULL, 'fixture prompt: review project filter persistence.',
   0, 'diff --git a/app/project_filters.go b/app/project_filters.go', '["app/project_filters.go"]', '',
   'review', 'code', 'openai', '', 'manual', '', '', 0, 0, NULL, NULL, NULL, '',
   '', 'roborev review a9c4e12', '{"input":3310,"output":540}', 0, 0,
   '00000000-0000-4000-8000-000000000107', '2026-06-20 09:27:12', 1),
  (108, 1, 1, 'f13a001', 'queue/rendering', 'fixture-session-1', 'codex', 'gpt-5', 'gpt-5',
   'openai', 'thorough', 'failed', '2026-06-20 09:12:00', '2026-06-20 09:12:08', '2026-06-20 09:14:39',
   'fixture-worker-1', NULL, 'fixture prompt: review queue rendering behavior.',
   0, 'diff --git a/cmd/roborev/tui/render_queue.go b/cmd/roborev/tui/render_queue.go', '["cmd/roborev/tui/render_queue.go"]', '',
   'review', 'code', 'openai', '', 'manual', '', '', 0, 0, NULL, NULL, NULL, '',
   'high', 'roborev review f13a001', '{"input":4418,"output":990}', 0, 0,
   '00000000-0000-4000-8000-000000000108', '2026-06-20 09:14:39', 1);

INSERT INTO reviews (
  id, job_id, agent, prompt, output, created_at, closed, verdict_bool,
  uuid, updated_at, updated_by_machine_id
) VALUES
  (1, 101, 'codex', 'fixture prompt: verify the generated docs asset branch rebase.',
   'P/F: P

Summary: fixture review passed after confirming the generated docs asset branch points at a reproducible screenshot commit.

Findings: none.', '2026-06-20 10:51:18', 1, 1,
   '10000000-0000-4000-8000-000000000101', '2026-06-20 10:51:18', 'fixture-machine'),
  (2, 102, 'codex', 'fixture prompt: review a synthetic follow-up patch before apply.',
   'P/F: P

Summary: fixture review found the follow-up patch ready to apply.

Findings: none.', '2026-06-20 10:36:44', 1, 1,
   '10000000-0000-4000-8000-000000000102', '2026-06-20 10:36:44', 'fixture-machine'),
  (3, 106, 'codex', 'fixture prompt: review export retention logic.',
   'P/F: F

Summary: fixture review found a medium severity retention bug.

Findings:
- Medium: the synthetic export retention path can skip the final boundary day when the cutoff lands exactly at midnight.', '2026-06-20 09:43:05', 0, 0,
   '10000000-0000-4000-8000-000000000106', '2026-06-20 09:43:05', 'fixture-machine'),
  (4, 105, 'codex', 'fixture prompt: review daemon activity stream changes.',
   'P/F: P

Summary: fixture review passed; daemon activity events render in order and retain stable timestamps.

Findings: none.', '2026-06-20 10:00:04', 0, 1,
   '10000000-0000-4000-8000-000000000105', '2026-06-20 10:00:04', 'fixture-machine'),
  (5, 107, 'codex', 'fixture prompt: review project filter persistence.',
   'P/F: P

Summary: fixture review passed; project filters persist across restarts and empty-state rendering stays stable.

Findings: none.', '2026-06-20 09:27:12', 0, 1,
   '10000000-0000-4000-8000-000000000107', '2026-06-20 09:27:12', 'fixture-machine'),
  (6, 108, 'codex', 'fixture prompt: review queue rendering behavior.',
   'P/F: F

Summary: fixture review found a high severity queue rendering regression.

Findings:
- High: when the terminal is narrow, the synthetic queue fixture shows the status cell overwriting the verdict column instead of truncating first.', '2026-06-20 09:14:39', 0, 0,
   '10000000-0000-4000-8000-000000000108', '2026-06-20 09:14:39', 'fixture-machine');

INSERT INTO responses (id, commit_id, job_id, responder, response, created_at, uuid) VALUES
  (1, 1, 108, 'maintainer', 'Fixture response: adjusted queue truncation and added coverage.', '2026-06-20 09:18:00', '20000000-0000-4000-8000-000000000001'),
  (2, 3, 106, 'maintainer', 'Fixture response: keeping this open until the retention boundary case is fixed.', '2026-06-20 09:47:00', '20000000-0000-4000-8000-000000000002'),
  (3, 2, 107, 'maintainer', 'Fixture response: verified the persistence behavior manually.', '2026-06-20 09:30:00', '20000000-0000-4000-8000-000000000003');
SCHEMA

echo ""
echo "Demo database created successfully!"
echo ""
sqlite3 "$DEST_DB" <<'STATS'
SELECT 'Repos: ' || COUNT(*) FROM repos;
SELECT 'Commits: ' || COUNT(*) FROM commits;
SELECT 'Review Jobs: ' || COUNT(*) FROM review_jobs;
SELECT 'Reviews: ' || COUNT(*) FROM reviews;
SELECT 'Responses: ' || COUNT(*) FROM responses;
STATS

echo ""
echo "To use: ROBOREV_DATA_DIR=$DEMO_DIR roborev tui"
