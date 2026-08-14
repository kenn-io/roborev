package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyWebAssetsCommandIsHiddenAndRejectsCompilationStub(t *testing.T) {
	cmd := verifyWebAssetsCmd()
	assert.True(t, cmd.Hidden)
	assert.Equal(t, "verify-web-assets", cmd.Name())
	require.ErrorContains(t, cmd.Execute(), "compilation stub")
}
