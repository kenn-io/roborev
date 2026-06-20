package scripts

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

var docsScreenshotRepos = []string{"roborev", "kata", "msgvault", "agentsview"}

func TestPrepareDemoDBUsesCanonicalRepoIdentity(t *testing.T) {
	tempDir := t.TempDir()
	sourceDB := filepath.Join(tempDir, "source.db")
	db := createScreenshotSourceDB(t, sourceDB)
	defer db.Close()

	for i, name := range docsScreenshotRepos {
		repoID := int64(i + 1)
		insertScreenshotRepo(t, db, repoID, "/public/"+name, name, "git@github.com:kenn-io/"+name+".git")
		insertScreenshotReview(t, db, repoID, repoID, "PUBLIC "+name, 1)
	}

	insertScreenshotRepo(t, db, 90, "/private/roborev", "roborev", "git@github.com:private/roborev.git")
	insertScreenshotReview(t, db, 90, 900, "PRIVATE roborev", 0)
	insertScreenshotReview(t, db, 90, 901, "PRIVATE roborev extra", 0)

	demoDB := runPrepareDemoDB(t, tempDir, sourceDB)
	text := readScreenshotDemoText(t, demoDB)

	assert.Contains(t, text, "PUBLIC roborev")
	assert.NotContains(t, text, "PRIVATE roborev")
	assert.NotContains(t, text, "/private/roborev")
}

func TestPrepareDemoDBRedactsWindowsUserPaths(t *testing.T) {
	tempDir := t.TempDir()
	sourceDB := filepath.Join(tempDir, "source.db")
	db := createScreenshotSourceDB(t, sourceDB)
	defer db.Close()

	for i, name := range docsScreenshotRepos {
		repoID := int64(i + 1)
		insertScreenshotRepo(t, db, repoID, "/public/"+name, name, "git@github.com:kenn-io/"+name+".git")
		insertScreenshotReview(t, db, repoID, repoID, "PUBLIC "+name, 1)
	}
	insertScreenshotReview(t, db, 1, 200, `Windows paths C:\Users\Alice\roborev\secret.go and C:/Users/Bob/roborev/secret.go`, 0)

	demoDB := runPrepareDemoDB(t, tempDir, sourceDB)
	text := readScreenshotDemoText(t, demoDB)

	assert.NotContains(t, text, `C:\Users\Alice`)
	assert.NotContains(t, text, `C:/Users/Bob`)
	assert.NotContains(t, text, "Alice")
	assert.NotContains(t, text, "Bob")
	assert.Contains(t, text, "/home/maintainer")
}

func TestPrepareDemoDBCuratesReviewContent(t *testing.T) {
	tempDir := t.TempDir()
	sourceDB := filepath.Join(tempDir, "source.db")
	db := createScreenshotSourceDB(t, sourceDB)
	defer db.Close()

	for i, name := range docsScreenshotRepos {
		repoID := int64(i + 1)
		insertScreenshotRepo(t, db, repoID, "/public/"+name, name, "git@github.com:kenn-io/"+name+".git")
		insertScreenshotReview(t, db, repoID, repoID, "PUBLIC "+name, 1)
	}

	insertScreenshotReview(t, db, 1, 300, "committed review", 0)
	_, err := db.Exec(
		`UPDATE review_jobs
		 SET prompt = 'RAW_LOCAL_SECRET prompt',
		     diff_content = 'RAW_LOCAL_SECRET diff'
		 WHERE id = 300`,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`UPDATE reviews
		 SET prompt = 'RAW_LOCAL_SECRET review prompt',
		     output = 'RAW_LOCAL_SECRET review output'
		 WHERE job_id = 300`,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO responses (commit_id, job_id, responder, response)
		 VALUES (300, 300, 'maintainer', 'RAW_LOCAL_SECRET response')`,
	)
	require.NoError(t, err)

	insertScreenshotReview(t, db, 1, 301, "TASK_LOCAL_SECRET", 0)
	_, err = db.Exec(`UPDATE review_jobs SET job_type = 'task', commit_id = NULL WHERE id = 301`)
	require.NoError(t, err)

	insertScreenshotReview(t, db, 1, 302, "DIRTY_LOCAL_SECRET", 0)
	_, err = db.Exec(`UPDATE review_jobs SET dirty_files = '["private.txt"]' WHERE id = 302`)
	require.NoError(t, err)

	demoDB := runPrepareDemoDB(t, tempDir, sourceDB)
	text := readScreenshotDemoText(t, demoDB)

	assert.NotContains(t, text, "RAW_LOCAL_SECRET")
	assert.NotContains(t, text, "TASK_LOCAL_SECRET")
	assert.NotContains(t, text, "DIRTY_LOCAL_SECRET")
	assert.Contains(t, text, "Docs screenshot fixture")
}

func createScreenshotSourceDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)

	_, err = db.Exec(`
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
  agent TEXT NOT NULL DEFAULT 'codex',
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
  job_type TEXT NOT NULL DEFAULT 'review',
  review_type TEXT NOT NULL DEFAULT '',
  worktree_path TEXT DEFAULT '',
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
  source_machine_id TEXT,
  synced_at TEXT
);
`)
	require.NoError(t, err)
	return db
}

func insertScreenshotRepo(t *testing.T, db *sql.DB, id int64, rootPath, name, identity string) {
	t.Helper()

	_, err := db.Exec(
		`INSERT INTO repos (id, root_path, name, identity) VALUES (?, ?, ?, ?)`,
		id,
		rootPath,
		name,
		identity,
	)
	require.NoError(t, err)
}

func insertScreenshotReview(t *testing.T, db *sql.DB, repoID, jobID int64, marker string, verdict int) {
	t.Helper()

	commitID := jobID
	sha := fmt.Sprintf("%07x", jobID)
	status := "done"
	if verdict == 0 {
		status = "failed"
	}
	_, err := db.Exec(
		`INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
		 VALUES (?, ?, ?, 'Fixture Maintainer', ?, '2026-06-20 12:00:00')`,
		commitID,
		repoID,
		sha,
		"Review "+marker,
	)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO review_jobs
		 (id, repo_id, commit_id, git_ref, status, enqueued_at, started_at, finished_at, worker_id, prompt, diff_content, uuid)
		 VALUES (?, ?, ?, ?, ?, '2026-06-20 12:00:00', '2026-06-20 12:00:01', '2026-06-20 12:00:02', 'worker-1', ?, ?, ?)`,
		jobID,
		repoID,
		commitID,
		sha,
		status,
		"Prompt "+marker,
		"Diff "+marker,
		fmt.Sprintf("00000000-0000-4000-8000-%012d", jobID),
	)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO reviews (job_id, agent, prompt, output, verdict_bool)
		 VALUES (?, 'codex', ?, ?, ?)`,
		jobID,
		"Prompt "+marker,
		"Output "+marker,
		verdict,
	)
	require.NoError(t, err)
}

func runPrepareDemoDB(t *testing.T, tempDir, sourceDB string) string {
	t.Helper()

	cmd := exec.Command("bash", "../docs/screenshots/prepare-demo-db.sh")
	cmd.Env = append(os.Environ(),
		"TMPDIR="+tempDir,
		"ROBOREV_DOCS_SOURCE_DB="+sourceDB,
		"HOME="+filepath.Join(tempDir, "maintainer-home"),
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	return filepath.Join(tempDir, "roborev-demo-data", "reviews.db")
}

func readScreenshotDemoText(t *testing.T, dbPath string) string {
	t.Helper()

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()

	rows, err := db.Query(`
SELECT root_path || ' ' || name || ' ' || COALESCE(identity, '') FROM repos
UNION ALL SELECT subject || ' ' || author FROM commits
UNION ALL SELECT COALESCE(prompt, '') || ' ' || COALESCE(diff_content, '') FROM review_jobs
UNION ALL SELECT prompt || ' ' || output FROM reviews
UNION ALL SELECT responder || ' ' || response FROM responses
`)
	require.NoError(t, err)
	defer rows.Close()

	var builder strings.Builder
	for rows.Next() {
		var text string
		require.NoError(t, rows.Scan(&text))
		builder.WriteString(text)
		builder.WriteByte('\n')
	}
	require.NoError(t, rows.Err())
	return builder.String()
}
