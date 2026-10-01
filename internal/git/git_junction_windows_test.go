//go:build windows

package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/fslink"
)

// TestValidateRepoLocalPathNoSymlinksTraversesJunction covers a repository
// reached through a Windows directory junction. Canonicalizing a path below the
// junction used to fail with "The system cannot find the path specified", which
// aborted snapshot-dir validation.
func TestValidateRepoLocalPathNoSymlinksTraversesJunction(t *testing.T) {
	target := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(target, "sub", "deep"), 0o755))

	link := filepath.Join(t.TempDir(), "junction")
	require.NoError(t, fslink.CreateJunction(target, link))

	err := ValidateRepoLocalPathNoSymlinks(link, filepath.Join(link, "sub", "deep"))
	require.NoError(t, err)
}

// TestValidateRepoLocalPathNoSymlinksRejectsJunctionComponent covers a
// junction inside the repository, which snapshot_dir rejects like a symlink
// whether it points inside or outside the repository.
func TestValidateRepoLocalPathNoSymlinksRejectsJunctionComponent(t *testing.T) {
	tests := []struct {
		name   string
		target func(t *testing.T, repo string) string
	}{
		{
			name: "inside repo",
			target: func(t *testing.T, repo string) string {
				dir := filepath.Join(repo, "real")
				require.NoError(t, os.Mkdir(dir, 0o755))
				return dir
			},
		},
		{
			name: "outside repo",
			target: func(t *testing.T, _ string) string {
				return t.TempDir()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			link := filepath.Join(repo, "snap")
			require.NoError(t, fslink.CreateJunction(tt.target(t, repo), link))

			err := ValidateRepoLocalPathNoSymlinks(repo, filepath.Join(link, "out"))
			require.ErrorContains(t, err, "snapshot_dir must not contain symlinks or junctions")
		})
	}
}
