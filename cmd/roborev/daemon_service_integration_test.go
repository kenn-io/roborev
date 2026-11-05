//go:build integration

package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// installRoborev atomically replaces path with a copy of binary, as a package
// upgrade does.
func installRoborev(t *testing.T, binary, path string) {
	t.Helper()
	data, err := os.ReadFile(binary)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path+".next", data, 0o700))
	require.NoError(t, os.Rename(path+".next", path))
}
