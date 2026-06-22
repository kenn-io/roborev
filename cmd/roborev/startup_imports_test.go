package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoborevStartupAvoidsTerminalProbeImports(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go list -deps failed:\n%s", out)

	deps := strings.Fields(string(out))
	assert.NotContains(t, deps, "charm.land/lipgloss/v2/compat")
}
