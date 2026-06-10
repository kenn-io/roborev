//go:build windows

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHiddenCommandNeverFlashesConsoleWindows(t *testing.T) {
	assert := assert.New(t)

	cmd := HiddenCommand("taskkill", "/PID", "1", "/F")

	require.NotNil(t, cmd.SysProcAttr)
	assert.True(cmd.SysProcAttr.HideWindow, "HideWindow must be set")
	assert.NotZero(cmd.SysProcAttr.CreationFlags&createNoWindow,
		"CREATE_NO_WINDOW must be set so console tools cannot open visible windows")
	assert.Equal([]string{"taskkill", "/PID", "1", "/F"}, cmd.Args)
}
