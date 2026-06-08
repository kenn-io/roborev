package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestUniqueJobRepoPathsDedupesInOrder(t *testing.T) {
	jobs := []storage.ReviewJob{
		{RepoPath: "/repo/a"},
		{RepoPath: ""},
		{RepoPath: "/repo/a"},
		{RepoPath: "/repo/b"},
		{RepoPath: "/repo/b"},
		{RepoPath: "/repo/c"},
	}

	assert.Equal(t, []string{"/repo/a", "/repo/b", "/repo/c"}, uniqueJobRepoPaths(jobs))
}
