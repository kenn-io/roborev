#!/bin/bash
# Prepare an isolated demo database with selected repo reviews
# This script ONLY READS from the source database - never modifies it

set -euo pipefail

SOURCE_DB="${ROBOREV_DATA_DIR:-$HOME/.roborev}/reviews.db"
DEMO_DIR="${TMPDIR:-/tmp}/roborev-demo-data"
DEST_DB="$DEMO_DIR/reviews.db"

# Safety checks
if [[ ! -f "$SOURCE_DB" ]]; then
    echo "Error: Source database not found at $SOURCE_DB"
    exit 1
fi

# Create demo directory
mkdir -p "$DEMO_DIR"

if [[ -f "$DEST_DB" ]]; then
    echo "Removing existing demo database..."
    rm "$DEST_DB"
fi

echo "Source: $SOURCE_DB (READ ONLY)"
echo "Destination: $DEST_DB"
echo ""

# Source DBs prior to the rename used "addressed"; newer DBs use "closed".
REVIEW_STATUS_COL="$(sqlite3 "$SOURCE_DB" "SELECT CASE
  WHEN EXISTS (SELECT 1 FROM pragma_table_info('reviews') WHERE name = 'closed') THEN 'closed'
  WHEN EXISTS (SELECT 1 FROM pragma_table_info('reviews') WHERE name = 'addressed') THEN 'addressed'
  ELSE ''
END;")"

if [[ -z "$REVIEW_STATUS_COL" ]]; then
    echo "Error: Source reviews table has neither 'closed' nor legacy 'addressed' column"
    exit 1
fi

echo "Review status column: $REVIEW_STATUS_COL"
echo ""

# Create new database with schema
echo "Creating demo database schema..."
sqlite3 "$DEST_DB" <<'SCHEMA'
CREATE TABLE repos (
  id INTEGER PRIMARY KEY,
  root_path TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE commits (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  sha TEXT UNIQUE NOT NULL,
  author TEXT NOT NULL,
  subject TEXT NOT NULL,
  timestamp TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE review_jobs (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  commit_id INTEGER REFERENCES commits(id),
  git_ref TEXT NOT NULL,
  agent TEXT NOT NULL DEFAULT 'codex',
  status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed','canceled')) DEFAULT 'queued',
  enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
  started_at TEXT,
  finished_at TEXT,
  worker_id TEXT,
  error TEXT,
  prompt TEXT,
  retry_count INTEGER NOT NULL DEFAULT 0,
  diff_content TEXT,
  reasoning TEXT NOT NULL DEFAULT 'thorough',
  agentic INTEGER NOT NULL DEFAULT 0,
  uuid TEXT,
  source_machine_id TEXT,
  updated_at TEXT,
  synced_at TEXT,
  model TEXT,
  branch TEXT,
  token_usage TEXT,
  agent_invoked INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE reviews (
  id INTEGER PRIMARY KEY,
  job_id INTEGER UNIQUE NOT NULL REFERENCES review_jobs(id),
  agent TEXT NOT NULL,
  prompt TEXT NOT NULL,
  output TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  closed INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE responses (
  id INTEGER PRIMARY KEY,
  commit_id INTEGER REFERENCES commits(id),
  responder TEXT NOT NULL,
  response TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_review_jobs_status ON review_jobs(status);
CREATE INDEX idx_review_jobs_repo ON review_jobs(repo_id);
CREATE INDEX idx_review_jobs_git_ref ON review_jobs(git_ref);
CREATE UNIQUE INDEX idx_review_jobs_uuid ON review_jobs(uuid);
CREATE INDEX idx_review_jobs_branch ON review_jobs(branch);
CREATE INDEX idx_commits_sha ON commits(sha);
SCHEMA

# Extract selected repos (READ from source, WRITE to dest)
echo "Extracting reviews..."

# Attach source as read-only and copy filtered data
sqlite3 "$DEST_DB" <<SQL
ATTACH DATABASE 'file:$SOURCE_DB?mode=ro' AS source;

-- Copy selected repos
INSERT INTO repos (id, root_path, name, created_at)
SELECT id, root_path, name, created_at
FROM source.repos
WHERE name IN ('roborev', 'agentsview', 'msgvault');

-- Copy commits for those repos
INSERT INTO commits (id, repo_id, sha, author, subject, timestamp, created_at)
SELECT c.id, c.repo_id, c.sha, c.author, c.subject, c.timestamp, c.created_at
FROM source.commits c
WHERE c.repo_id IN (SELECT id FROM repos);

-- Copy review_jobs for those repos
INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status,
                         enqueued_at, started_at, finished_at, worker_id, error,
                         prompt, retry_count, diff_content, reasoning, agentic,
                         uuid, source_machine_id, updated_at, synced_at, model, branch,
                         token_usage, agent_invoked)
SELECT j.id, j.repo_id, j.commit_id, j.git_ref, j.agent, j.status,
       j.enqueued_at, j.started_at, j.finished_at, j.worker_id, j.error,
       j.prompt, j.retry_count, j.diff_content, j.reasoning, j.agentic,
       j.uuid, j.source_machine_id, j.updated_at, j.synced_at, j.model, j.branch,
       j.token_usage, j.agent_invoked
FROM source.review_jobs j
WHERE j.repo_id IN (SELECT id FROM repos);

-- Copy reviews for those jobs
INSERT INTO reviews (id, job_id, agent, prompt, output, created_at, closed)
SELECT r.id, r.job_id, r.agent, r.prompt, r.output, r.created_at, r.${REVIEW_STATUS_COL}
FROM source.reviews r
WHERE r.job_id IN (SELECT id FROM review_jobs);

-- Copy responses for those commits
INSERT INTO responses (id, commit_id, responder, response, created_at)
SELECT r.id, r.commit_id, r.responder, r.response, r.created_at
FROM source.responses r
WHERE r.commit_id IN (SELECT id FROM commits);

DETACH DATABASE source;
SQL

# Rewrite repo paths for Docker container
echo "Rewriting repo paths for Docker..."
sqlite3 "$DEST_DB" <<'PATHS'
UPDATE repos SET root_path = '/repos/roborev' WHERE name = 'roborev';
UPDATE repos SET root_path = '/repos/agentsview' WHERE name = 'agentsview';
UPDATE repos SET root_path = '/repos/msgvault' WHERE name = 'msgvault';

-- Clear sync metadata so reviews appear as local, not [remote]
UPDATE review_jobs SET source_machine_id = NULL, synced_at = NULL;
PATHS

# Validate expected repos were copied
EXPECTED_REPOS="roborev agentsview msgvault"
for repo in $EXPECTED_REPOS; do
    if ! sqlite3 "$DEST_DB" "SELECT 1 FROM repos WHERE name = '$repo'" | grep -q 1; then
        echo "Error: expected repo '$repo' not found in demo database after copy"
        exit 1
    fi
done

# Report stats
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
