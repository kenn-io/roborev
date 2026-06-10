//go:build windows

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCreateNoWindow = 0x08000000

func TestKillAllDaemonsCommandsFallBackFromWmicToPowerShell(t *testing.T) {
	assert := assert.New(t)

	cmds := killAllDaemonsCommands()
	require.Len(t, cmds, 2)

	// wmic first for older Windows; it no longer exists on current
	// Windows 11 builds, so PowerShell must be the fallback.
	assert.Contains(strings.ToLower(cmds[0].Path), "wmic.exe")
	assert.Contains(strings.Join(cmds[0].Args, " "), "%roborev%daemon%run%")

	assert.Contains(strings.ToLower(cmds[1].Path), "powershell.exe")
	psArgs := strings.Join(cmds[1].Args, " ")
	assert.Contains(psArgs, "*roborev*daemon*run*")
	assert.Contains(psArgs, "Stop-Process")

	for _, cmd := range cmds {
		require.NotNil(t, cmd.SysProcAttr, "%s must run hidden", cmd.Path)
		assert.True(cmd.SysProcAttr.HideWindow, "%s must run hidden", cmd.Path)
		assert.NotZero(cmd.SysProcAttr.CreationFlags&testCreateNoWindow,
			"%s must not open a console window", cmd.Path)
	}
}
