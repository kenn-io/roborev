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

func stubWebAssetsEmbedded(t *testing.T, embedded bool) {
	t.Helper()
	original := webAssetsEmbedded
	webAssetsEmbedded = func() bool { return embedded }
	t.Cleanup(func() { webAssetsEmbedded = original })
}

func TestVersionCmdHumanOutput(t *testing.T) {
	stubWebAssetsEmbedded(t, true)
	var output bytes.Buffer
	cmd := versionCmd()
	cmd.SetOut(&output)

	require.NoError(t, cmd.Execute())
	assert.Equal(t, fmt.Sprintf("roborev %s\n", version.Version), output.String())
}

func TestVersionCmdHumanOutputFlagsMissingWebAssets(t *testing.T) {
	stubWebAssetsEmbedded(t, false)
	var output bytes.Buffer
	cmd := versionCmd()
	cmd.SetOut(&output)

	require.NoError(t, cmd.Execute())
	assert.Equal(t, fmt.Sprintf(
		"roborev %s (no embedded web assets; reinstall from an official release or build with 'make build')\n",
		version.Version,
	), output.String())
}

func TestVersionCmdJSONOutput(t *testing.T) {
	for _, embedded := range []bool{true, false} {
		stubWebAssetsEmbedded(t, embedded)
		var output bytes.Buffer
		cmd := versionCmd()
		cmd.SetOut(&output)
		cmd.SetArgs([]string{"--json"})

		require.NoError(t, cmd.Execute())

		var got struct {
			Name      string `json:"name"`
			Version   string `json:"version"`
			WebAssets bool   `json:"web_assets"`
		}
		require.NoError(t, json.Unmarshal(output.Bytes(), &got))
		assert.Equal(t, "roborev", got.Name)
		assert.Equal(t, version.Version, got.Version)
		assert.Equal(t, embedded, got.WebAssets)
	}
}
