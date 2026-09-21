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
