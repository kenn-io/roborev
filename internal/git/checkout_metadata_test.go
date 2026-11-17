package git

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadCheckoutMetadataWithoutGitExecutable(t *testing.T) {
	assert := assert.New(t)
	repo := NewTestRepo(t)
	repo.CommitFile("base.txt", "base", "base")
	wantHead := repo.HeadSHA()
	wantBranch := strings.TrimSpace(repo.Run("branch", "--show-current"))
	t.Setenv("PATH", t.TempDir())

	metadata, err := ReadCheckoutMetadata(repo.Dir)
	require.NoError(t, err)

	assert.Equal(cleanEvalPath(repo.Dir), cleanEvalPath(metadata.WorktreeRoot))
	assert.Equal(filepath.Join(metadata.WorktreeRoot, ".git"), metadata.GitDir)
	assert.Equal(metadata.GitDir, metadata.CommonDir)
	assert.Equal(wantHead, metadata.Head)
	assert.Equal(wantBranch, metadata.Branch)
}

func TestReadCheckoutMetadataLinkedWorktreeWithoutGitExecutable(t *testing.T) {
	assert := assert.New(t)
	repo := NewTestRepo(t)
	repo.CommitFile("base.txt", "base", "base")
	worktree := repo.AddWorktree("feature/metadata")
	wantHead := strings.TrimSpace(worktree.Run("rev-parse", "HEAD"))
	t.Setenv("PATH", t.TempDir())

	metadata, err := ReadCheckoutMetadata(worktree.Dir)
	require.NoError(t, err)

	assert.Equal(cleanEvalPath(worktree.Dir), cleanEvalPath(metadata.WorktreeRoot))
	assert.Equal(wantHead, metadata.Head)
	assert.Equal("feature/metadata", metadata.Branch)
	assert.NotEqual(metadata.GitDir, metadata.CommonDir)
	assert.Equal(cleanEvalPath(filepath.Join(repo.Dir, ".git")), cleanEvalPath(metadata.CommonDir))
}

func TestReadUnbornCheckoutMetadataWithoutGitExecutable(t *testing.T) {
	assert := assert.New(t)
	repo := NewTestRepo(t)
	repo.Run("symbolic-ref", "HEAD", "refs/heads/first")
	t.Setenv("PATH", t.TempDir())

	metadata, err := ReadUnbornCheckoutMetadata(repo.Dir)
	require.NoError(t, err)
	assert.Equal(cleanEvalPath(repo.Dir), cleanEvalPath(metadata.WorktreeRoot))
	assert.Equal(filepath.Join(metadata.WorktreeRoot, ".git"), metadata.GitDir)
	assert.Equal(metadata.GitDir, metadata.CommonDir)
	assert.Empty(metadata.Head)
	assert.Equal("first", metadata.Branch)

	_, err = ReadCheckoutMetadata(repo.Dir)
	require.Error(t, err, "an unborn branch has no HEAD commit to report")
}

func TestReadUnbornCheckoutMetadataRejectsCommittedHead(t *testing.T) {
	repo := NewTestRepo(t)
	repo.CommitFile("base.txt", "base", "base")

	_, err := ReadUnbornCheckoutMetadata(repo.Dir)
	require.Error(t, err)
}
