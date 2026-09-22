//go:build windows

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/fslink"
)

// TestValidateAndResolvePathTraversesJunction covers an agent working in a
// checkout reached through a Windows directory junction. Canonicalizing a path
// below the junction used to fail, so every repository read was rejected as
// ErrPathTraversal and the agent could not open a single file.
func TestValidateAndResolvePathTraversesJunction(t *testing.T) {
	target := t.TempDir()
	sub := filepath.Join(target, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(target, "readme.txt"), []byte("hi"), 0o600))

	link := filepath.Join(t.TempDir(), "junction")
	require.NoError(t, fslink.CreateJunction(target, link))

	client := setupTestClient("plan", link)

	resolved, err := client.validateAndResolvePath("readme.txt", false)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(resolved, "readme.txt"), "resolved path %q", resolved)

	written, err := client.validateAndResolvePath(filepath.Join("sub", "new.txt"), true)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(written, "new.txt"), "resolved path %q", written)
}
