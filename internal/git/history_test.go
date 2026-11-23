package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlameAndLogShowTheAuthorsLocalDate(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644))
	runGit(t, dir, "add", "a.txt")
	// 23:30 at UTC-5 is already the next day in UTC.
	runGit(t, dir, "-c", "user.name=Author", "-c", "user.email=author@example.com",
		"commit", "--date=2026-01-01T23:30:00-0500", "-m", "late commit")

	hunks, err := BlameLines(t.Context(), dir, "HEAD", "a.txt", 1, 1)
	require.NoError(t, err)
	require.Len(t, hunks, 1)
	commits, err := PathLog(t.Context(), dir, "HEAD", "a.txt", 0, 0, 0)
	require.NoError(t, err)
	require.Len(t, commits, 1)

	assert.Equal(t, "2026-01-01", hunks[0].Commit.Date)
	assert.Equal(t, commits[0].Date, hunks[0].Commit.Date)
}
