package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/version"
)

func TestVersionCmdHumanOutput(t *testing.T) {
	var output bytes.Buffer
	cmd := versionCmd()
	cmd.SetOut(&output)

	require.NoError(t, cmd.Execute())
	assert.Equal(t, fmt.Sprintf("roborev %s\n", version.Version), output.String())
}

func TestVersionCmdJSONOutput(t *testing.T) {
	var output bytes.Buffer
	cmd := versionCmd()
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--json"})

	require.NoError(t, cmd.Execute())

	var got struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	require.NoError(t, json.Unmarshal(output.Bytes(), &got))
	assert.Equal(t, "roborev", got.Name)
	assert.Equal(t, version.Version, got.Version)
}
