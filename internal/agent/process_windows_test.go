//go:build windows

package agent

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCreateNoWindow = 0x08000000

func TestConfigureSubprocessHidesConsole(t *testing.T) {
	cmd := exec.Command("git", "status")
	_ = configureSubprocess(context.Background(), cmd)
	require.NotNil(t, cmd.SysProcAttr)
	assert.NotZero(t, cmd.SysProcAttr.CreationFlags&testCreateNoWindow)
}
