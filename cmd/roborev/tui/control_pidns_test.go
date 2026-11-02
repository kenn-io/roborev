//go:build linux

package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestSandboxTUICleanupKeepsHostRuntime(t *testing.T) { //nolint:paralleltest // t.Setenv isolates the shared runtime directory.
	if dataDir := os.Getenv("ROBOREV_TEST_HOST_TUI_DATA"); dataDir != "" {
		t.Setenv("ROBOREV_DATA_DIR", dataDir)
		assert.Zero(t, CleanupStaleTUIRuntimes(t.Context()))
		return
	}

	dataDir := testenv.SetDataDir(t)
	require.NoError(t, WriteTUIRuntime(buildTUIRuntimeInfo(filepath.Join(dataDir, "tui.sock"), "http://127.0.0.1:1")))
	path := tuiRuntimePath(os.Getpid())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	t.Setenv("ROBOREV_TEST_HOST_TUI_DATA", dataDir)
	testutil.RunPIDNamespaceTest(t, nil, "")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
