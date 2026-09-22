package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackedFilesAtReturnsOnlyBlobsFromCommit(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.CommitFile("internal/code.go", "package code", "source")
	repo.CommitFile("notes.md", "notes", "docs")
	sha := repo.HeadSHA()

	files, err := TrackedFilesAt(context.Background(), repo.Dir, sha)
	require.NoError(t, err)
	assert.Equal(t, []string{"initial.txt", "internal/code.go", "notes.md"}, files)
	assert.True(t, IsSourceFile("internal/code.go"))
	assert.False(t, IsSourceFile("image.png"))
}

func TestTrackedFilesAtExcludesSymlinks(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("symlink-target", "internal/code.go")
	blob := repo.Run("hash-object", "-w", "symlink-target")
	repo.Run("update-index", "--add", "--cacheinfo", "120000,"+blob+",alias.go")
	repo.Run("commit", "-m", "add symlink")

	files, err := TrackedFilesAt(context.Background(), repo.Dir, repo.HeadSHA())
	require.NoError(t, err)
	assert.NotContains(t, files, "alias.go")
}
