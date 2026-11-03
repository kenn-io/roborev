package mcpserver

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerIconMetadata(t *testing.T) {
	assert := assert.New(t)
	session := connectSession(t, &fakeBackend{})
	info := session.InitializeResult().ServerInfo
	require.NotNil(t, info)
	assert.Equal("roborev", info.Name)
	assert.Equal("test", info.Version)
	assert.Equal("https://roborev.io", info.WebsiteURL)
	require.Len(t, info.Icons, 1)
	icon := info.Icons[0]
	assert.Equal("image/png", icon.MIMEType)
	assert.Equal([]string{"256x256"}, icon.Sizes)
	const prefix = "data:image/png;base64,"
	require.True(t, strings.HasPrefix(icon.Source, prefix))
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(icon.Source, prefix))
	require.NoError(t, err)
	img, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(256, img.Bounds().Dx())
	assert.Equal(256, img.Bounds().Dy())
}
