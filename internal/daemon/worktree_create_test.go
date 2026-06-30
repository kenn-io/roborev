package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"

	"go.kenn.io/roborev/internal/testutil"
)

func TestAttributeContentUsesGitLFS(t *testing.T) {
	assert.False(t, attributeContentUsesGitLFS("# *.bin filter=lfs\n*.txt text\n"))
	assert.True(t, attributeContentUsesGitLFS("*.bin filter=lfs diff=lfs merge=lfs -text\n"))
	assert.True(t, attributeContentUsesGitLFS("[attr]lfs filter=lfs diff=lfs merge=lfs -text\n"))
}

func TestPullGitLFSPropagatesPullFailure(t *testing.T) {
	script := `#!/bin/sh
if [ "$1" = "lfs" ] && [ "$2" = "env" ]; then exit 0; fi
if [ "$1" = "lfs" ] && [ "$2" = "pull" ]; then exit 9; fi
exit 1
`
	if runtime.GOOS == "windows" {
		script = "@echo off\r\n" +
			"if \"%1\"==\"lfs\" if \"%2\"==\"env\" exit /b 0\r\n" +
			"if \"%1\"==\"lfs\" if \"%2\"==\"pull\" exit /b 9\r\n" +
			"exit /b 1\r\n"
	}
	cleanup := testutil.MockBinaryInPath(t, "git", script)
	t.Cleanup(cleanup)

	err := pullGitLFS(context.Background(), gitcmd.New(), t.TempDir())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "git lfs pull")
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

	t.Run("tracked symlink attributes fail open", func(t *testing.T) {
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("README.md", "base\n", "base")
		repo.WriteFile("link-target.txt", "*.bin text\n")
		blob := strings.TrimSpace(repo.Run("hash-object", "-w", "link-target.txt"))
		repo.RunGit("update-index", "--add", "--cacheinfo", "120000,"+blob+",.gitattributes")
		repo.RunGit("commit", "-m", "symlink attrs")
		repo.WriteFile(".gitattributes", "*.bin text\n")

		assert.True(t, checkoutUsesGitLFS(context.Background(), repo.Path()))
	})

	t.Run("oversized tracked attributes fail open", func(t *testing.T) {
		repo := testutil.NewGitRepo(t)
		repo.CommitFile(".gitattributes", strings.Repeat("x", maxGitAttributeFileBytes+1), "large attrs")

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

	t.Run("configured symlink attributes file without lfs", func(t *testing.T) {
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("file.bin", "placeholder\n", "base")
		targetPath := filepath.Join(repo.Path(), "custom.attributes.target")
		attrPath := filepath.Join(repo.Path(), "custom.attributes")
		require.NoError(t, os.WriteFile(targetPath, []byte("*.bin text\n"), 0o644))
		if err := os.Symlink(targetPath, attrPath); err != nil {
			t.Skipf("symlink attributes file: %v", err)
		}
		repo.Config("core.attributesFile", attrPath)

		assert.False(t, checkoutUsesGitLFS(context.Background(), repo.Path()))
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

	t.Run("default global symlink attributes file without lfs", func(t *testing.T) {
		xdgConfigHome := filepath.Join(t.TempDir(), "xdg-config")
		t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

		repo := testutil.NewGitRepo(t)
		repo.CommitFile("file.bin", "placeholder\n", "base")
		targetPath := filepath.Join(xdgConfigHome, "git", "attributes.target")
		attrPath := filepath.Join(xdgConfigHome, "git", "attributes")
		require.NoError(t, os.MkdirAll(filepath.Dir(attrPath), 0o755))
		require.NoError(t, os.WriteFile(targetPath, []byte("*.bin text\n"), 0o644))
		if err := os.Symlink(targetPath, attrPath); err != nil {
			t.Skipf("symlink attributes file: %v", err)
		}

		assert.False(t, checkoutUsesGitLFS(context.Background(), repo.Path()))
	})
}
