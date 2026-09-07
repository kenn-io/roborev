package prompt

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

func TestPreparePreservesCompletePrompt(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := testutil.GetHeadSHA(t, repo.Path())
	content := strings.Repeat("complete file content\n", 1000) + "last diff line\n"
	head := repo.CommitFile("large.txt", content, "complete change")
	discussion := strings.Repeat("complete discussion\n", 1000) + "last discussion comment"
	builder := NewBuilderWithConfig(nil, &config.Config{DefaultMaxPromptSize: 4096}).ForRepo(repo.Path(), 0)
	for _, ref := range []string{head, base + ".." + head} {
		t.Run(ref, func(t *testing.T) {
			text, err := builder.BuildWithAdditionalContext(ref, 0, "test", "", "", discussion)
			require.NoError(t, err)
			assert.Contains(t, text, "last diff line")
			assert.Contains(t, text, discussion)
			prepared, err := builder.Prepare(text, SnapshotTarget{})
			require.NoError(t, err)
			require.NotNil(t, prepared.Cleanup)
			t.Cleanup(prepared.Cleanup)
			saved, err := os.ReadFile(prepared.FilePath)
			require.NoError(t, err)
			assert.Equal(t, text, string(saved))
			assert.Less(t, len(prepared.Prompt), 4096)
		})
	}
	t.Run("dirty", func(t *testing.T) {
		text, err := builder.BuildDirty(content, 0, "test", "", "")
		require.NoError(t, err)
		assert.Contains(t, text, content)
		prepared, err := builder.BuildDirtyWithSnapshot(content, 0, "test", "", "")
		require.NoError(t, err)
		require.NotNil(t, prepared.Cleanup)
		saved, err := os.ReadFile(prepared.FilePath)
		require.NoError(t, err)
		assert.Equal(t, text, string(saved))
		prepared.Cleanup()
		_, err = os.Stat(prepared.FilePath)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})
	t.Run("inline", func(t *testing.T) {
		prepared, err := builder.Prepare("complete small prompt", SnapshotTarget{})
		require.NoError(t, err)
		assert.Equal(t, "complete small prompt", prepared.Prompt)
		assert.Empty(t, prepared.FilePath)
		assert.Nil(t, prepared.Cleanup)
	})
}
