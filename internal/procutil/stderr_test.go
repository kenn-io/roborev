package procutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithStderrKeepsCommandStderr(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
	_, err := cmd.Output()
	require.Error(t, err)

	got := WithStderr(err)

	require.ErrorContains(t, got, "not a git repository")
	_, ok := errors.AsType[*exec.ExitError](got)
	assert.True(t, ok, "wrapped error must still expose *exec.ExitError")
}

func TestWithStderrLeavesOtherErrorsUnchanged(t *testing.T) {
	assert.Equal(t, exec.ErrNotFound, WithStderr(exec.ErrNotFound))
	assert.NoError(t, WithStderr(nil))
}
