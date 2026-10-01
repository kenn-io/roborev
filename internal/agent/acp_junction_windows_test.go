//go:build windows

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
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
	readme := filepath.Join(target, "readme.txt")
	require.NoError(t, os.WriteFile(readme, []byte("hi"), 0o600))

	link := filepath.Join(t.TempDir(), "junction")
	require.NoError(t, fslink.CreateJunction(target, link))

	// The target holds no junction, so filepath.EvalSymlinks gives its
	// canonical spelling (expanding any 8.3 TempDir names).
	wantReadme, err := filepath.EvalSymlinks(readme)
	require.NoError(t, err)
	wantSub, err := filepath.EvalSymlinks(sub)
	require.NoError(t, err)

	client := setupTestClient("plan", link)

	resolved, err := client.validateAndResolvePath("readme.txt", false)
	require.NoError(t, err)
	assert.Equal(t, wantReadme, resolved)

	written, err := client.validateAndResolvePath(filepath.Join("sub", "new.txt"), true)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(wantSub, "new.txt"), written)
}

// TestValidateAndResolvePathRejectsJunctionOutsideRepo covers a junction
// inside the repository that points outside it. A write through it used to
// pass the containment check because the trailing junction in the parent
// directory was left unresolved.
func TestValidateAndResolvePathRejectsJunctionOutsideRepo(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o600))
	require.NoError(t, fslink.CreateJunction(outside, filepath.Join(repo, "escape")))

	client := setupTestClient("plan", repo)

	_, err := client.validateAndResolvePath(filepath.Join("escape", "new.txt"), true)
	require.ErrorIs(t, err, ErrPathTraversal)

	_, err = client.validateAndResolvePath(filepath.Join("escape", "secret.txt"), false)
	require.ErrorIs(t, err, ErrPathTraversal)
}
