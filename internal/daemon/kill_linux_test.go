//go:build linux

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/testutil"
)

func TestIsProcessAliveRejectsUnreapedChild(t *testing.T) {
	pid, exit := testutil.StartLivenessChild(t)
	assert.True(t, isProcessAlive(pid))
	exit()
	assert.False(t, isProcessAlive(pid))
}
