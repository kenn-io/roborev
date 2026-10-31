package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexSandboxPrepareSymlinkedTMPDIR(t *testing.T) {
	skipIfWindows(t)
	tmp := t.TempDir()
	alias := filepath.Join(t.TempDir(), "tmp")
	require.NoError(t, os.Symlink(tmp, alias))
	t.Setenv("TMPDIR", alias)
	t.Run("repo access", TestCodexSandboxRepoContainment)
}
