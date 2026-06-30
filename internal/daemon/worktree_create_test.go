package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

func TestAttributeContentUsesGitLFS(t *testing.T) {
	assert.False(t, attributeContentUsesGitLFS("# *.bin filter=lfs\n*.txt text\n"))
	assert.True(t, attributeContentUsesGitLFS("*.bin filter=lfs diff=lfs merge=lfs -text\n"))
	assert.True(t, attributeContentUsesGitLFS("[attr]lfs filter=lfs diff=lfs merge=lfs -text\n"))
}

func TestCheckoutUsesGitLFS(t *testing.T) {
	t.Run("no attributes", func(t *testing.T) {
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("README.md", "base\n", "base")

		assert.False(t, checkoutUsesGitLFS(context.Background(), repo.Path()))
	})

	t.Run("tracked root attributes", func(t *testing.T) {
		repo := testutil.NewGitRepo(t)
		repo.CommitFiles(map[string]string{
			".gitattributes": "*.bin filter=lfs diff=lfs merge=lfs -text\n",
			"file.bin":       "placeholder\n",
		}, "lfs attrs")

		assert.True(t, checkoutUsesGitLFS(context.Background(), repo.Path()))
	})

	t.Run("tracked nested attributes", func(t *testing.T) {
		repo := testutil.NewGitRepo(t)
		repo.CommitFiles(map[string]string{
			"assets/.gitattributes": "*.bin filter=lfs diff=lfs merge=lfs -text\n",
			"assets/file.bin":       "placeholder\n",
		}, "nested lfs attrs")

		assert.True(t, checkoutUsesGitLFS(context.Background(), repo.Path()))
	})

	t.Run("git info attributes", func(t *testing.T) {
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("file.bin", "placeholder\n", "base")
		out, err := gitOutput(context.Background(), repo.Path(), "rev-parse", "--git-path", "info/attributes")
		require.NoError(t, err)
		attrPath := filepath.Clean(strings.TrimSpace(string(out)))
		if !filepath.IsAbs(attrPath) {
			attrPath = filepath.Join(repo.Path(), attrPath)
		}
		require.NoError(t, os.WriteFile(attrPath, []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644))

		assert.True(t, checkoutUsesGitLFS(context.Background(), repo.Path()))
	})

	t.Run("configured attributes file", func(t *testing.T) {
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("file.bin", "placeholder\n", "base")
		attrPath := filepath.Join(repo.Path(), "custom.attributes")
		require.NoError(t, os.WriteFile(attrPath, []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644))
		repo.Config("core.attributesFile", attrPath)

		assert.True(t, checkoutUsesGitLFS(context.Background(), repo.Path()))
	})

	t.Run("default global attributes file", func(t *testing.T) {
		xdgConfigHome := filepath.Join(t.TempDir(), "xdg-config")
		t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

		repo := testutil.NewGitRepo(t)
		repo.CommitFile("file.bin", "placeholder\n", "base")
		attrPath := filepath.Join(xdgConfigHome, "git", "attributes")
		require.NoError(t, os.MkdirAll(filepath.Dir(attrPath), 0o755))
		require.NoError(t, os.WriteFile(attrPath, []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644))

		assert.True(t, checkoutUsesGitLFS(context.Background(), repo.Path()))
	})
}
