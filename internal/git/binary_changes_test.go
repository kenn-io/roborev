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
