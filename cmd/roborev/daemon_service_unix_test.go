//go:build linux || darwin

package main

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A CLI whose own binary was replaced after it started runs a different
// version than the service would start, so it must not drain the daemon.
func TestDaemonServiceReplacedCLIIsAnotherInstallation(t *testing.T) {
	path, err := os.Executable()
	require.NoError(t, err)
	if os.Getenv("ROBOREV_TEST_EXECUTABLE_REPLACEMENT") == "1" {
		same, err := (&daemonService{executable: path}).sameInstallation()
		require.NoError(t, err)
		require.True(t, same)
		require.NoError(t, os.WriteFile(path+".next", []byte("replacement executable"), 0o700))
		require.NoError(t, os.Rename(path+".next", path))
		same, err = (&daemonService{executable: path}).sameInstallation()
		require.NoError(t, err)
		assert.False(t, same)
		return
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	copyPath := filepath.Join(t.TempDir(), "roborev-test")
	require.NoError(t, os.WriteFile(copyPath, data, 0o700))
	cmd := exec.CommandContext(t.Context(), copyPath, "-test.run=^TestDaemonServiceReplacedCLIIsAnotherInstallation$")
	cmd.Env = append(os.Environ(), "ROBOREV_TEST_EXECUTABLE_REPLACEMENT=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
