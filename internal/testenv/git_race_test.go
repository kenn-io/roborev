//go:build race

package testenv

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitFixtureRaceLauncherExit(t *testing.T) {
	// The race detector documents atexit_sleep_ms (default 1000). A child-only
	// 60s delay lets this external-process watchdog detect wrapper finalization.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--version")
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=60000")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.Contains(t, string(output), "git version ")
}
