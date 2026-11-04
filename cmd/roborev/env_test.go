package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFilterDaemonEnv(t *testing.T) {
	env := []string{"PATH=/bin", "HOME=/fixture", "GIT_DIR=/repo/.git", "XPC_SERVICE_NAME=com.example.worker", "XPC_SERVICE_NAME=other", "XPC_SERVICE_NAME_EXTRA=keep"}
	original := append([]string(nil), env...)
	assert.Equal(t, []string{"PATH=/bin", "HOME=/fixture", "XPC_SERVICE_NAME_EXTRA=keep"}, filterDaemonEnv(env))
	assert.Equal(t, original, env)
}
