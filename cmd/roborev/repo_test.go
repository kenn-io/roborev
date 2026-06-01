package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveRepoIdentifier(t *testing.T) {
	// 1. Simple identifiers (no disk interaction or simple non-path)
	t.Run("simple identifiers", func(t *testing.T) {
		tests := []struct {
			name  string
			input string
			want  string
		}{
			{"name unchanged", "my-project", "my-project"},
			{"name with slash unchanged", "org/project", "org/project"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assertPath(t, resolveRepoIdentifier(t.Context(), tt.input), tt.want)
			})
		}
	})

	// 2. Permission test (isolated)
	t.Run("inaccessible path", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("skipping permission test on Windows")
		}
		if os.Getuid() == 0 {
			t.Skip("skipping permission test when running as root")
		}

		tmpDir := t.TempDir()
		orgDir := filepath.Join(tmpDir, "org")
		require.NoError(t, os.Mkdir(orgDir, 0o755), "Failed to create org dir")

		// Ensure we are in a safe directory (tmpDir) before modifying permissions of orgDir
		chdir(t, tmpDir)

		require.NoError(t, os.Chmod(orgDir, 0o000), "Failed to chmod")
		defer func() { _ = os.Chmod(orgDir, 0o755) }()

		// The test expects "org/project" because it can't stat "org" to see if it's a repo,
		// so it treats it as a name string.
		assertPath(t, resolveRepoIdentifier(t.Context(), "org/project"), "org/project")
	})

	// 3. Git repo resolution
	t.Run("git repo resolution", func(t *testing.T) {
		root := newTestGitRepo(t).Dir
		// Create common structure: root/sub/dir
		subDir := filepath.Join(root, "sub", "dir")
		require.NoError(t, os.MkdirAll(subDir, 0o755))

		// Also create a non-git temp dir for that one case
		nonGitDir := t.TempDir()
		resolvedNonGit, err := filepath.EvalSymlinks(nonGitDir)
		require.NoError(t, err, "Failed to resolve symlinks")

		tests := []struct {
			name  string
			dir   string // directory to execute from
			input string
			want  string
		}{
			{
				name:  "dot in subdir",
				dir:   filepath.Join(root, "sub", "dir"),
				input: ".",
				want:  root,
			},
			{
				name:  "relative path",
				dir:   filepath.Join(root, "sub"),
				input: "./",
				want:  root,
			},
			{
				name:  "absolute path",
				dir:   root,
				input: subDir,
				want:  root,
			},
			{
				name:  "parent traversal",
				dir:   filepath.Join(root, "sub", "dir"),
				input: "..",
				want:  root,
			},
			{
				name:  "non-git path returns absolute path",
				dir:   resolvedNonGit,
				input: ".",
				want:  resolvedNonGit,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				chdir(t, tt.dir)
				got := resolveRepoIdentifier(t.Context(), tt.input)
				assertPath(t, got, tt.want)
			})
		}
	})
}

func assertPath(t *testing.T, got, want string) {
	t.Helper()
	assert.Equal(t, want, got)
}
