//go:build windows

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCreateNoWindow = 0x08000000

func TestNewGitCmdHidesConsole(t *testing.T) {
	assert := assert.New(t)

	cmd := newGitCmd("status")
	require.NotNil(t, cmd.SysProcAttr)
	assert.NotZero(cmd.SysProcAttr.CreationFlags & testCreateNoWindow)

	ctxCmd := newGitCmdContext(t.Context(), "status")
	require.NotNil(t, ctxCmd.SysProcAttr)
	assert.NotZero(ctxCmd.SysProcAttr.CreationFlags & testCreateNoWindow)
}

func TestNewGitCommitCmdHidesConsole(t *testing.T) {
	cmd := newGitCommitCmd(t.Context(), "", "commit", "-m", "test")
	require.NotNil(t, cmd.SysProcAttr)
	assert.NotZero(t, cmd.SysProcAttr.CreationFlags&testCreateNoWindow)
}
