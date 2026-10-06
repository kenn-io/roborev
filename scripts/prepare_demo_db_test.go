package scripts

import (
	"database/sql"
	"encoding/json"
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

var docsScreenshotRepos = []string{"roborev"}

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

	assert.Contains(t, text, "github.com/kenn-io/roborev")
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
	_, err := db.Exec(`UPDATE review_jobs SET review_type = ? WHERE id = 200`, `C:\Users\Alice\roborev`)
	require.NoError(t, err)

	demoDB := runPrepareDemoDB(t, tempDir, sourceDB)
	text := readScreenshotDemoText(t, demoDB)

	assert.NotContains(t, text, `C:\Users\Alice`)
	assert.NotContains(t, text, `C:/Users/Bob`)
	assert.NotContains(t, text, "Alice")
	assert.NotContains(t, text, "Bob")
	assert.Contains(t, text, "/home/maintainer")
}

func TestPrepareDemoDBPreservesSanitizedRealReviewContent(t *testing.T) {
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
		 SET prompt = 'REAL_PUBLIC_PROMPT from C:\Users\Alice\roborev',
		     diff_content = 'REAL_PUBLIC_DIFF',
		     git_ref = 'feature/public-review',
		     branch = 'feature/public-review',
		     command_line = 'roborev review feature/public-review'
		 WHERE id = 300`,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`UPDATE reviews
		 SET prompt = 'REAL_PUBLIC_REVIEW_PROMPT from /Users/Alice/roborev',
		     structured_output = ?
		 WHERE job_id = 300`,
		screenshotReviewDocument(t, "REAL_PUBLIC_REVIEW_FINDING from /Users/Alice/roborev with api_key=abc123 and github_pat_1234567890ABCDEFGHIJKLMNOP"),
	)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE commits SET subject = 'REAL_PUBLIC_COMMIT_SUBJECT' WHERE id = 300`)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO responses (commit_id, job_id, responder, response)
		 VALUES (300, 300, 'maintainer', 'REAL_PUBLIC_RESPONSE')`,
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

	assert.Contains(t, text, "REAL_PUBLIC_PROMPT")
	assert.Contains(t, text, "REAL_PUBLIC_REVIEW_PROMPT")
	assert.Contains(t, text, "REAL_PUBLIC_REVIEW_FINDING")
	assert.Contains(t, text, "REAL_PUBLIC_COMMIT_SUBJECT")
	assert.Contains(t, text, "feature/public-review")
	assert.Contains(t, text, "/home/maintainer")
	assert.NotContains(t, text, `C:\Users\Alice`)
	assert.NotContains(t, text, "/Users/Alice")
	assert.NotContains(t, text, "/home/maintainer/Alice")
	assert.NotContains(t, text, "api_key=abc123")
	assert.Contains(t, text, "api_key=[REDACTED]")
	assert.NotContains(t, text, "github_pat_1234567890ABCDEFGHIJKLMNOP")
	assert.Contains(t, text, "REAL_PUBLIC_DIFF")
	assert.NotContains(t, text, "REAL_PUBLIC_RESPONSE")
	assert.NotContains(t, text, "TASK_LOCAL_SECRET")
	assert.NotContains(t, text, "DIRTY_LOCAL_SECRET")
}

func TestPrepareDemoDBRedactsConfiguredPrivateTerms(t *testing.T) {
	tempDir := t.TempDir()
	sourceDB := filepath.Join(tempDir, "source.db")
	db := createScreenshotSourceDB(t, sourceDB)
	defer db.Close()

	insertScreenshotRepo(t, db, 1, "/public/roborev", "roborev", "git@github.com:kenn-io/roborev.git")
	insertScreenshotReview(t, db, 1, 1, "PRIVATE_SCREENSHOT_MARKER", 0)

	termsFile := filepath.Join(tempDir, "private-terms.txt")
	require.NoError(t, os.WriteFile(termsFile, []byte("# local screenshot denylist\nprivate_screenshot_marker\n"), 0o600))
	demoDB := runPrepareDemoDBWithTerms(t, tempDir, sourceDB, termsFile)
	text := readScreenshotDemoText(t, demoDB)

	assert.NotContains(t, strings.ToLower(text), "private_screenshot_marker")
	assert.Contains(t, text, "[REDACTED]")
}

func TestPrepareDemoDBSelectsOnlyCompletedJobsWithReviewVerdicts(t *testing.T) {
	tempDir := t.TempDir()
	sourceDB := filepath.Join(tempDir, "source.db")
	db := createScreenshotSourceDB(t, sourceDB)
	defer db.Close()

	for i, name := range docsScreenshotRepos {
		repoID := int64(i + 1)
		insertScreenshotRepo(t, db, repoID, "/public/"+name, name, "git@github.com:kenn-io/"+name+".git")
		insertScreenshotReview(t, db, repoID, repoID, "PUBLIC "+name, 1)
	}

	insertScreenshotJobWithoutReview(t, db, 1, 400, "FAILED_NO_REVIEW_SECRET", "failed")
	insertScreenshotReviewWithStatus(t, db, 1, 401, "CANCELED_WITH_REVIEW_SECRET", 0, "canceled")
	insertScreenshotReviewWithStatus(t, db, 1, 402, "SKIPPED_WITH_REVIEW_SECRET", 0, "skipped")
	insertScreenshotReview(t, db, 1, 403, "DONE_FAILING_VERDICT", 0)

	demoDB := runPrepareDemoDB(t, tempDir, sourceDB)
	text := readScreenshotDemoText(t, demoDB)

	assert.NotContains(t, text, "FAILED_NO_REVIEW_SECRET")
	assert.NotContains(t, text, "CANCELED_WITH_REVIEW_SECRET")
	assert.NotContains(t, text, "SKIPPED_WITH_REVIEW_SECRET")
	assert.Contains(t, text, "DONE_FAILING_VERDICT")
}

func TestPrepareDemoDBPreservesRealCommitAndRefMetadata(t *testing.T) {
	tempDir := t.TempDir()
	sourceDB := filepath.Join(tempDir, "source.db")
	db := createScreenshotSourceDB(t, sourceDB)
	defer db.Close()

	for i, name := range docsScreenshotRepos {
		repoID := int64(i + 1)
		insertScreenshotRepo(t, db, repoID, "/public/"+name, name, "git@github.com:kenn-io/"+name+".git")
		insertScreenshotReview(t, db, repoID, repoID, "PUBLIC "+name, 1)
	}

	insertScreenshotReview(t, db, 1, 500, "REAL_PUBLIC_METADATA", 0)
	_, err := db.Exec(
		`UPDATE commits
		 SET sha = 'abc1234',
		     author = 'REAL_PUBLIC_METADATA_AUTHOR',
		     subject = 'REAL_PUBLIC_METADATA_SUBJECT'
		 WHERE id = 500`,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`UPDATE review_jobs
		 SET git_ref = 'abc1234',
		     branch = 'feature/real-public-branch',
		     command_line = 'roborev review abc1234'
		 WHERE id = 500`,
	)
	require.NoError(t, err)

	demoDB := runPrepareDemoDB(t, tempDir, sourceDB)
	text := readScreenshotDemoText(t, demoDB)

	assert.Contains(t, text, "abc1234")
	assert.Contains(t, text, "REAL_PUBLIC_METADATA_AUTHOR")
	assert.Contains(t, text, "REAL_PUBLIC_METADATA_SUBJECT")
	assert.Contains(t, text, "feature/real-public-branch")
}

func TestPrepareDemoDBSkipsReviewsWithoutStructuredDocument(t *testing.T) {
	tempDir := t.TempDir()
	sourceDB := filepath.Join(tempDir, "source.db")
	db := createScreenshotSourceDB(t, sourceDB)
	defer db.Close()

	insertScreenshotRepo(t, db, 1, "/public/roborev", "roborev", "git@github.com:kenn-io/roborev.git")
	insertScreenshotReview(t, db, 1, 1, "STRUCTURED_REVIEW", 0)
	insertScreenshotReview(t, db, 1, 2, "LEGACY_MARKDOWN_REVIEW", 0)
	_, err := db.Exec(`UPDATE reviews SET output = 'LEGACY_MARKDOWN_REVIEW', structured_output = NULL WHERE job_id = 2`)
	require.NoError(t, err)

	demoDB := runPrepareDemoDB(t, tempDir, sourceDB)
	text := readScreenshotDemoText(t, demoDB)

	assert.Contains(t, text, "STRUCTURED_REVIEW")
	assert.NotContains(t, text, "LEGACY_MARKDOWN_REVIEW")
}

func TestPrepareDemoDBKeepsStructuredReviewsValidJSON(t *testing.T) {
	tempDir := t.TempDir()
	sourceDB := filepath.Join(tempDir, "source.db")
	db := createScreenshotSourceDB(t, sourceDB)
	defer db.Close()

	insertScreenshotRepo(t, db, 1, "/public/roborev", "roborev", "git@github.com:kenn-io/roborev.git")
	insertScreenshotReview(t, db, 1, 1, "QUOTED_PATH", 0)
	_, err := db.Exec(
		`UPDATE reviews SET structured_output = ? WHERE job_id = 1`,
		screenshotReviewDocument(t, `Run "/Users/Alice/roborev/main.go" first`),
	)
	require.NoError(t, err)

	demoDB := runPrepareDemoDB(t, tempDir, sourceDB)
	demo, err := sql.Open("sqlite", demoDB)
	require.NoError(t, err)
	defer demo.Close()

	var raw string
	require.NoError(t, demo.QueryRow(`SELECT structured_output FROM reviews`).Scan(&raw))
	var document map[string]string
	require.NoError(t, json.Unmarshal([]byte(raw), &document))
	assert.Equal(t, `Run "/home/maintainer" first`, document["summary"])
}

func TestPrepareDemoDBMergesCheckoutsOfOneRepository(t *testing.T) {
	tempDir := t.TempDir()
	sourceDB := filepath.Join(tempDir, "source.db")
	db := createScreenshotSourceDB(t, sourceDB)
	defer db.Close()

	insertScreenshotRepo(t, db, 1, "/public/roborev", "roborev", "git@github.com:kenn-io/roborev.git")
	insertScreenshotRepo(t, db, 2, "/public/roborev-worktree", "roborev", "https://github.com/kenn-io/roborev.git")
	insertScreenshotReview(t, db, 1, 1, "MAIN_CHECKOUT_REVIEW", 0)
	insertScreenshotReview(t, db, 2, 2, "WORKTREE_REVIEW", 1)
	// Both checkouts reviewed the same commit.
	_, err := db.Exec(`UPDATE commits SET sha = 'abc1234' WHERE id IN (1, 2)`)
	require.NoError(t, err)

	demoDB := runPrepareDemoDB(t, tempDir, sourceDB)
	demo, err := sql.Open("sqlite", demoDB)
	require.NoError(t, err)
	defer demo.Close()

	var repos, commits, jobs, jobRepos int
	require.NoError(t, demo.QueryRow(`SELECT COUNT(*) FROM repos`).Scan(&repos))
	require.NoError(t, demo.QueryRow(`SELECT COUNT(*) FROM commits`).Scan(&commits))
	require.NoError(t, demo.QueryRow(
		`SELECT COUNT(*), COUNT(DISTINCT j.repo_id) FROM review_jobs j JOIN commits c ON c.id = j.commit_id`,
	).Scan(&jobs, &jobRepos))
	assert := assert.New(t)
	assert.Equal(1, repos)
	assert.Equal(1, commits)
	assert.Equal(2, jobs)
	assert.Equal(1, jobRepos)
}

func screenshotReviewDocument(t *testing.T, summary string) string {
	t.Helper()

	raw, err := json.Marshal(map[string]string{"summary": summary})
	require.NoError(t, err)
	return string(raw)
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
  branch TEXT,
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
  command_line TEXT,
  min_severity TEXT NOT NULL DEFAULT '',
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
  structured_output TEXT,
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

	insertScreenshotReviewWithStatus(t, db, repoID, jobID, marker, verdict, "done")
}

func insertScreenshotReviewWithStatus(t *testing.T, db *sql.DB, repoID, jobID int64, marker string, verdict int, status string) {
	t.Helper()

	commitID := jobID
	sha := fmt.Sprintf("%07x", jobID)
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
		 (id, repo_id, commit_id, git_ref, branch, status, enqueued_at, started_at, finished_at, worker_id, prompt, diff_content, command_line, min_severity, uuid)
		 VALUES (?, ?, ?, ?, 'main', ?, '2026-06-20 12:00:00', '2026-06-20 12:00:01', '2026-06-20 12:00:02', 'worker-1', ?, ?, ?, 'medium', ?)`,
		jobID,
		repoID,
		commitID,
		sha,
		status,
		"Prompt "+marker,
		"Diff "+marker,
		"roborev review "+sha,
		fmt.Sprintf("00000000-0000-4000-8000-%012d", jobID),
	)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO reviews (job_id, agent, prompt, output, verdict_bool, structured_output)
		 VALUES (?, 'codex', ?, '', ?, ?)`,
		jobID,
		"Prompt "+marker,
		verdict,
		screenshotReviewDocument(t, "Output "+marker),
	)
	require.NoError(t, err)
}

func insertScreenshotJobWithoutReview(t *testing.T, db *sql.DB, repoID, jobID int64, marker, status string) {
	t.Helper()

	commitID := jobID
	sha := fmt.Sprintf("%07x", jobID)
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
		 (id, repo_id, commit_id, git_ref, branch, status, enqueued_at, started_at, finished_at, worker_id, prompt, diff_content, command_line, uuid)
		 VALUES (?, ?, ?, ?, 'main', ?, '2026-06-20 12:00:00', '2026-06-20 12:00:01', '2026-06-20 12:00:02', 'worker-1', ?, ?, ?, ?)`,
		jobID,
		repoID,
		commitID,
		sha,
		status,
		"Prompt "+marker,
		"Diff "+marker,
		"roborev review "+sha,
		fmt.Sprintf("00000000-0000-4000-8000-%012d", jobID),
	)
	require.NoError(t, err)
}

func runPrepareDemoDB(t *testing.T, tempDir, sourceDB string) string {
	t.Helper()
	return runPrepareDemoDBWithTerms(t, tempDir, sourceDB, "")
}

func runPrepareDemoDBWithTerms(t *testing.T, tempDir, sourceDB, termsFile string) string {
	t.Helper()
	requireMise(t)

	scriptPath := filepath.Join("..", "docs", "screenshots", "prepare-demo-db.sh")
	homeDir := filepath.Join(tempDir, "maintainer-home")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))

	// Keep the real HOME so mise and uv find their own configuration and
	// caches. ROBOREV_DOCS_HOME isolates the redacted home directory and the
	// default private terms file instead.
	environment := "export TMPDIR=" + shellQuote(bashPath(t, tempDir)) +
		" ROBOREV_DOCS_SOURCE_DB=" + shellQuote(bashPath(t, sourceDB)) +
		" ROBOREV_DOCS_HOME=" + shellQuote(bashPath(t, homeDir))
	if termsFile != "" {
		environment += " KENN_PRIVATE_TERMS_FILE=" + shellQuote(bashPath(t, termsFile))
	}
	cmd := exec.Command(
		"bash",
		"-c",
		environment+"; exec bash "+shellQuote(bashPath(t, scriptPath)),
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	return filepath.Join(tempDir, "roborev-demo-data", "reviews.db")
}

// requireMise skips when mise is missing locally, because prepare-demo-db.sh
// gets uv from the repo's mise.lock. CI installs mise, so a missing binary
// there is a failure.
func requireMise(t *testing.T) {
	t.Helper()

	_, err := exec.LookPath("mise")
	if err == nil {
		return
	}
	require.Empty(t, os.Getenv("CI"), "CI must install mise for the demo database tests: %v", err)
	t.Skip("mise is not on PATH; prepare-demo-db.sh needs it to run the pinned uv")
}

func readScreenshotDemoText(t *testing.T, dbPath string) string {
	t.Helper()

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()

	rows, err := db.Query(`
SELECT root_path || ' ' || name || ' ' || COALESCE(identity, '') FROM repos
UNION ALL SELECT sha || ' ' || subject || ' ' || author FROM commits
UNION ALL SELECT git_ref || ' ' || COALESCE(branch, '') || ' ' || COALESCE(command_line, '') || ' ' || COALESCE(prompt, '') || ' ' || COALESCE(diff_content, '') FROM review_jobs
UNION ALL SELECT prompt || ' ' || output || ' ' || COALESCE(structured_output, '') FROM reviews
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
