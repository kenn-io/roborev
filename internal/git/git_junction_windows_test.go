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
