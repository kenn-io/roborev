package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
)

func TestRoborevStartupAvoidsTerminalProbeImports(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", ".")
	cmd.Env = testenv.UserEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go list -deps failed:\n%s", out)

	deps := strings.Fields(string(out))
	assert.NotContains(t, deps, "charm.land/lipgloss/v2/compat")
}
