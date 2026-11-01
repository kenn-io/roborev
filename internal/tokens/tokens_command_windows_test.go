//go:build windows

package tokens

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCreateNoWindow = 0x08000000

func TestBuildAgentsviewCmdHidesConsole(t *testing.T) {
	cmd := buildAgentsviewCmd(t.Context(), "agentsview", "--version")
	require.NotNil(t, cmd.SysProcAttr)
	assert.NotZero(t, cmd.SysProcAttr.CreationFlags&testCreateNoWindow)
}
