package prompt

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

const committedPDF = "%PDF-1.7\x00committed exhibit"

// savedEvidencePath returns the snapshot path the prompt lists for repoPath.
func savedEvidencePath(t *testing.T, text, repoPath string) string {
	t.Helper()
	match := regexp.MustCompile(regexp.QuoteMeta(strconv.Quote(repoPath)) + ` \(\w+\): read ("[^"]+")`).FindStringSubmatch(text)
	require.NotNil(t, match, "prompt lists %s", repoPath)
	saved, err := strconv.Unquote(match[1])
	require.NoError(t, err)
	return saved
}

func TestBuildWithSnapshotBinaryEvidence(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFiles(map[string]string{"old.png": "\x89PNG\x00"}, "add image")
	base := repo.HeadSHA()
	repo.RunGit("rm", "-q", "old.png")
	head := repo.CommitFiles(map[string]string{"exhibits/brief.pdf": committedPDF, "notes.txt": "text"}, "add exhibit")
	// An edited working copy must not replace the committed evidence.
	repo.WriteFile("exhibits/brief.pdf", "%PDF-1.7\x00edited after commit")

	for _, tc := range []struct {
		name  string
		limit int
	}{
		{name: "inline"},
		{name: "prompt file", limit: 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			builder := NewBuilderWithConfig(nil, &config.Config{DefaultMaxPromptSize: tc.limit}).
				ForRepo(repo.Path(), 0).WithBinaryEvidence(true)
			result, err := builder.BuildWithSnapshot(base+".."+head, 0, "test", "", "", nil)
			require.NoError(t, err)
			require.NotNil(t, result.Cleanup)
			text := result.Prompt
			if tc.limit > 0 {
				require.NotEmpty(t, result.FilePath, "oversized prompts move to a file")
				data, err := os.ReadFile(result.FilePath)
				require.NoError(t, err)
				text = string(data)
			}

			assert.Contains(text, "## Committed review evidence")
			assert.Contains(text, `- "old.png" (deleted)`)
			assert.NotContains(text, `"notes.txt" (`)
			saved := savedEvidencePath(t, text, "exhibits/brief.pdf")
			assert.Equal(".pdf", filepath.Ext(saved))
			rel, err := filepath.Rel(filepath.Join(repo.Path(), ".roborev"), saved)
			require.NoError(t, err)
			assert.True(filepath.IsLocal(rel), "evidence lives under snapshot_dir")
			data, err := os.ReadFile(saved)
			require.NoError(t, err)
			assert.Equal(committedPDF, string(data))

			result.Cleanup()
			assert.NoFileExists(saved)
		})
	}
}

func TestBinaryEvidenceOffLeavesPromptUnchanged(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.CommitFile("brief.pdf", committedPDF, "add exhibit")
	builder := NewBuilderWithConfig(nil, &config.Config{}).ForRepo(repo.Path(), 0)

	want, err := builder.Build(head, 0, "test", "", "")
	require.NoError(t, err)
	result, err := builder.BuildWithSnapshot(head, 0, "test", "", "", nil)
	require.NoError(t, err)
	assert.Equal(t, want, result.Prompt)
	assert.NotContains(t, result.Prompt, "Committed review evidence")
}

func TestBinaryEvidenceRejectsDirtyReviews(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	builder := NewBuilderWithConfig(nil, &config.Config{}).ForRepo(repo.Path(), 0).WithBinaryEvidence(true)
	_, err := builder.BuildDirtyWithSnapshot("diff --git a/x b/x\n", 0, "test", "", "")
	require.ErrorIs(t, err, ErrBinaryEvidenceDirty)
}
