//go:build !windows

package tui

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControlSocketPermissions(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	cleanup, err := startControlListener(
		socketPath, newTestProgramUnix(t),
	)
	require.NoError(t, err, "startControlListener")
	t.Cleanup(cleanup)

	info, err := os.Stat(socketPath)
	require.NoError(t, err, "stat socket")
	assert.NotZero(t, info.Mode().Type()&os.ModeSocket,
		"expected socket type, got %s", info.Mode().Type())
	perm := info.Mode().Perm()
	assert.Zero(t, perm&0o077,
		"socket permissions %o allow group/other access", perm)
}

func TestEnsureSocketDirTightensExistingDir(t *testing.T) {
	socketDir := t.TempDir()

	// Simulate a pre-existing data directory created with 0755.
	require.NoError(t, os.Chmod(socketDir, 0o755))

	require.NoError(t, ensureSocketDir(socketDir))

	di, err := os.Stat(socketDir)
	require.NoError(t, err, "stat socket dir")
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm(),
		"socket directory should be tightened to 0700")
}

func TestRemoveStaleSocket_IncompatibleSocketRefused(t *testing.T) {
	path := shortSocketPath(t, "dgram")
	// Create a DGRAM socket -- dial with STREAM will fail with a
	// non-ECONNREFUSED error, which should NOT be treated as stale.
	fd, err := syscall.Socket(
		syscall.AF_UNIX, syscall.SOCK_DGRAM, 0,
	)
	require.NoError(t, err, "create dgram socket fd")
	defer syscall.Close(fd)
	require.NoError(t,
		syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}),
		"bind dgram socket")
	t.Cleanup(func() { os.Remove(path) })

	err = removeStaleSocket(path)
	require.Error(t, err,
		"expected error for incompatible socket")
	assert.FileExists(t, path,
		"incompatible socket should not be deleted")
}

func TestCleanupDoesNotUnlinkSuccessorSocket(t *testing.T) {
	socketPath := shortSocketPath(t, "succ")

	// Listener A binds.
	lnA, err := net.Listen("unix", socketPath)
	require.NoError(t, err, "bind listener A")
	lnA.(*net.UnixListener).SetUnlinkOnClose(false)

	// Simulate the race window: A's cleanup removes its own
	// socket, then B binds to the same path before A closes.
	require.NoError(t, os.Remove(socketPath))
	lnB, err := net.Listen("unix", socketPath)
	require.NoError(t, err, "bind listener B")
	t.Cleanup(func() { lnB.Close() })

	// A's Close must not unlink B's socket. Without
	// SetUnlinkOnClose(false), this would delete B's file.
	lnA.Close()

	assert.FileExists(t, socketPath,
		"successor socket should survive predecessor close")
}

// newTestProgramUnix creates a tea.Program for Unix-only tests.
func newTestProgramUnix(t *testing.T) *tea.Program {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{})
		},
	))
	t.Cleanup(ts.Close)
	m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())
	p := tea.NewProgram(m, tea.WithoutRenderer())
	go func() { _, _ = p.Run() }()
	t.Cleanup(func() { p.Kill() })
	time.Sleep(100 * time.Millisecond)
	return p
}
