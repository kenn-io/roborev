package agent

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnavailableError(t *testing.T) {
	cause := errors.New("command could not start")
	marked := MarkUnavailable(cause)

	require.Error(t, marked)
	assert.True(t, IsUnavailable(marked))
	require.ErrorIs(t, marked, cause)
	assert.Same(t, marked, MarkUnavailable(marked), "marking twice should be idempotent")
	require.NoError(t, MarkUnavailable(nil))
	assert.False(t, IsUnavailable(cause))
}
