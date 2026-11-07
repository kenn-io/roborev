//go:build linux

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/testutil"
)

func TestIsPIDAliveForUpdateRejectsUnreapedChild(t *testing.T) {
	pid, exit := testutil.StartLivenessChild(t)
	assert.True(t, isPIDAliveForUpdateDefault(pid))
	exit()
	assert.False(t, isPIDAliveForUpdateDefault(pid))
}
