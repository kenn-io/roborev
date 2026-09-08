package agent

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCIContainerMissingMountFailsClosed(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("ROBOREV_CI_AGENT_IMAGE", "review-image")
	t.Setenv("ROBOREV_CI_REPO", t.TempDir())
	t.Setenv("TMPDIR", "")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo unisolated")
	configureSubprocess(cmd)
	out, err := cmd.CombinedOutput()
	require.ErrorContains(t, err, "CI isolation requires")
	assert.Empty(t, out)
}
