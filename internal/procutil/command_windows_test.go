//go:build windows

package procutil

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCreateNoWindow = 0x08000000

func TestHideConsoleSetsCreateNoWindow(t *testing.T) {
	assert := assert.New(t)

	cmd := exec.Command("tasklist")
	HideConsole(cmd)

	require.NotNil(t, cmd.SysProcAttr)
	assert.True(cmd.SysProcAttr.HideWindow, "HideWindow must be set")
	assert.NotZero(cmd.SysProcAttr.CreationFlags&testCreateNoWindow,
		"CREATE_NO_WINDOW must be set so the child cannot open a visible console window")
}

func TestHideConsolePreservesExistingCreationFlags(t *testing.T) {
	assert := assert.New(t)

	const createNewProcessGroup = 0x00000200
	cmd := exec.Command("tasklist")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	HideConsole(cmd)

	assert.NotZero(cmd.SysProcAttr.CreationFlags&createNewProcessGroup,
		"pre-existing creation flags must be preserved")
	assert.NotZero(cmd.SysProcAttr.CreationFlags & testCreateNoWindow)
}
