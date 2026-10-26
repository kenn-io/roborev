package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangedBinaryFiles(t *testing.T) {
	ctx := context.Background()
	repo := NewTestRepo(t)
	repo.WriteFile("keep.txt", "text")
	repo.WriteFile("exhibit.pdf", "%PDF\x00v1")
	repo.WriteFile("old.png", "\x89PNG\x00")
	repo.Run("add", "-A")
	repo.Run("commit", "-m", "base")
	base := repo.HeadSHA()

	repo.WriteFile("keep.txt", "changed text")
	repo.WriteFile("exhibit.pdf", "%PDF\x00v2")
	repo.WriteFile("scan.jpg", "\xff\xd8\x00")
	repo.WriteFile("skipped.bin", "\x00skip")
	repo.Run("rm", "-q", "old.png")
	repo.Run("add", "-A")
	repo.Run("commit", "-m", "change exhibits")
	head := repo.HeadSHA()

	for _, ref := range []string{head, base + ".." + head} {
		changes, err := ChangedBinaryFiles(ctx, repo.Dir, ref, "skipped.bin")
		require.NoError(t, err)
		byPath := map[string]BinaryChange{}
		for _, change := range changes {
			byPath[change.Path] = change
		}
		require.Len(t, byPath, 3, ref)
		assert.Equal(t, "modified", byPath["exhibit.pdf"].Status)
		assert.Equal(t, "added", byPath["scan.jpg"].Status)
		assert.Equal(t, BinaryChange{Path: "old.png", Status: "deleted"}, byPath["old.png"])

		data, err := ReadBlob(ctx, repo.Dir, byPath["exhibit.pdf"].OID)
		require.NoError(t, err)
		assert.Equal(t, "%PDF\x00v2", string(data))
	}
}

func TestChangedBinaryFilesMergeConflictResolution(t *testing.T) {
	ctx := context.Background()
	repo := NewTestRepo(t)
	repo.WriteFile("exhibit.pdf", "%PDF\x00base")
	repo.Run("add", "-A")
	repo.Run("commit", "-m", "base")
	repo.Run("checkout", "-q", "-b", "side")
	repo.WriteFile("exhibit.pdf", "%PDF\x00side")
	repo.Run("commit", "-q", "-am", "side")
	repo.Run("checkout", "-q", "-")
	repo.WriteFile("exhibit.pdf", "%PDF\x00main")
	repo.Run("commit", "-q", "-am", "main")
	// Merge without auto-resolving, then commit bytes that differ from both
	// parents, as a hand-resolved binary conflict would.
	repo.Run("merge", "-q", "--no-ff", "--no-commit", "-s", "ours", "side")
	repo.WriteFile("exhibit.pdf", "%PDF\x00resolved")
	repo.Run("add", "exhibit.pdf")
	repo.Run("commit", "-q", "--no-edit")

	changes, err := ChangedBinaryFiles(ctx, repo.Dir, repo.HeadSHA())
	require.NoError(t, err)
	require.Len(t, changes, 1)
	data, err := ReadBlob(ctx, repo.Dir, changes[0].OID)
	require.NoError(t, err)
	assert.Equal(t, "%PDF\x00resolved", string(data))
}
