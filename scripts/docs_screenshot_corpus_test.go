package scripts

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
)

// A corpus holds a standalone copy of the prepared database, including
// changes that are still in its write-ahead log while it is open.
func TestDocsScreenshotCorpusKeepsWriteAheadLogChanges(t *testing.T) {
	requirePython(t)
	tempDir := t.TempDir()
	source := filepath.Join(tempDir, "prepared.db")
	db, err := sql.Open("sqlite", source)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA wal_autocheckpoint=0",
		"CREATE TABLE reviews (summary TEXT)",
		"INSERT INTO reviews VALUES ('edited after preparation')",
	} {
		_, err := db.ExecContext(t.Context(), statement)
		require.NoError(t, err, statement)
	}
	wal, err := os.Stat(source + "-wal")
	require.NoError(t, err)
	require.Positive(t, wal.Size(), "the insert must still be in the write-ahead log")

	corpus := filepath.Join(tempDir, "corpus")
	runCorpusScript(t, "write", source, corpus)

	assert.NoFileExists(t, filepath.Join(corpus, "reviews.db-wal"))
	copied, err := sql.Open("sqlite", filepath.Join(corpus, "reviews.db"))
	require.NoError(t, err)
	defer copied.Close()
	var summary string
	require.NoError(t, copied.QueryRowContext(t.Context(), "SELECT summary FROM reviews").Scan(&summary))
	assert.Equal(t, "edited after preparation", summary)
}

// A corpus capture must reject a database that differs from its approved
// manifest before it builds or runs anything in Docker.
func TestDocsScreenshotCaptureRejectsChangedCorpus(t *testing.T) {
	requirePython(t)
	tempDir := t.TempDir()
	source := filepath.Join(tempDir, "prepared.db")
	db, err := sql.Open("sqlite", source)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "CREATE TABLE reviews (summary TEXT)")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	corpus := filepath.Join(tempDir, "corpus")
	runCorpusScript(t, "write", source, corpus)
	approved, err := os.ReadFile(filepath.Join(corpus, "reviews.db"))
	require.NoError(t, err)

	// An unchanged corpus yields exactly the approved database.
	copied := filepath.Join(tempDir, "copied")
	runCorpusScript(t, "extract", filepath.Join(corpus, "manifest.json"), copied)
	content, err := os.ReadFile(filepath.Join(copied, "reviews.db"))
	require.NoError(t, err)
	assert.Equal(t, approved, content)

	require.NoError(t, os.WriteFile(filepath.Join(corpus, "reviews.db"), []byte("changed reviews"), 0o600))
	bin := filepath.Join(tempDir, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	dockerCalls := filepath.Join(tempDir, "docker-calls")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"),
		[]byte("#!/usr/bin/env bash\necho \"$@\" >> "+shellQuote(bashPath(t, dockerCalls))+"\n"), 0o755))

	script := filepath.Join("..", "docs", "screenshots", "build-generated-assets.sh")
	cmd := exec.CommandContext(t.Context(), "bash", "-c",
		"export PATH="+shellQuote(bashPath(t, bin))+":\"$PATH\""+
			" DOCS_SCREENSHOTS_CORPUS="+shellQuote(bashPath(t, filepath.Join(corpus, "manifest.json")))+
			"; exec bash "+shellQuote(bashPath(t, script))+" "+shellQuote(bashPath(t, filepath.Join(tempDir, "output"))))
	cmd.Env = testenv.UserEnv()
	output, err := cmd.CombinedOutput()

	require.Error(t, err, string(output))
	assert.Contains(t, string(output), "reviews.db differs from the corpus manifest")
	assert.NoFileExists(t, dockerCalls, "a changed corpus must not reach Docker")
	assert.NoDirExists(t, filepath.Join(tempDir, "output", "assets"))
}

func runCorpusScript(t *testing.T, subcommand string, paths ...string) {
	t.Helper()
	command := "exec python3 " + shellQuote(bashPath(t, filepath.Join("..", "docs", "screenshots", "corpus.py"))) +
		" " + subcommand
	for _, arg := range paths {
		command += " " + shellQuote(bashPath(t, arg))
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", command)
	cmd.Env = testenv.UserEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

// requirePython skips where python3 is missing; capture workers and the
// Linux and macOS runners provide it.
func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on PATH")
	}
}
