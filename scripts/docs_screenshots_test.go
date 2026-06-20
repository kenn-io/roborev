package scripts

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestPrepareDemoDBUsesSyntheticFixture(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI is required by prepare-demo-db.sh")
	}

	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "empty-live-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	cmd := exec.Command("bash", "../docs/screenshots/prepare-demo-db.sh")
	cmd.Env = append(os.Environ(),
		"TMPDIR="+tempDir,
		"ROBOREV_DATA_DIR="+dataDir,
		"HOME="+filepath.Join(tempDir, "home"),
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.NotContains(t, string(output), "Source:")

	demoDB := filepath.Join(tempDir, "roborev-demo-data", "reviews.db")
	db, err := sql.Open("sqlite", demoDB)
	require.NoError(t, err)
	defer db.Close()

	var repoCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM repos`).Scan(&repoCount))
	assert.Equal(t, 3, repoCount)

	var jobCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM review_jobs`).Scan(&jobCount))
	assert.GreaterOrEqual(t, jobCount, 6)

	var schema string
	require.NoError(t, db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='review_jobs'`,
	).Scan(&schema))
	for _, status := range []string{"applied", "rebased", "skipped"} {
		assert.Contains(t, schema, "'"+status+"'")
	}

	var privateFields int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM review_jobs
		WHERE prompt LIKE '%/Users/%'
		   OR diff_content LIKE '%/Users/%'
		   OR error LIKE '%/Users/%'
	`).Scan(&privateFields))
	assert.Equal(t, 0, privateFields)

	var reviewText string
	require.NoError(t, db.QueryRow(`SELECT output FROM reviews ORDER BY id LIMIT 1`).Scan(&reviewText))
	assert.Contains(t, reviewText, "fixture")
}
