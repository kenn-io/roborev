package mcpserver

import (
	_ "embed"
	"encoding/base64"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverIconPNG is the browser app's favicon rendered at 256x256.
// Regenerate from the repository root with:
//
// rsvg-convert -w 256 -h 256 web/src/assets/favicon.svg -o /tmp/roborev-icon.png
// magick /tmp/roborev-icon.png -strip -define png:exclude-chunks=date,time internal/mcpserver/assets/icon.png
//
//go:embed assets/icon.png
var serverIconPNG []byte

// A data URI lets stdio and HTTP clients load the icon without an external URL.
var serverIconSource = "data:image/png;base64," + base64.StdEncoding.EncodeToString(serverIconPNG)

func serverImplementation(version string) *mcp.Implementation {
	return &mcp.Implementation{
		Name:       "roborev",
		Version:    version,
		WebsiteURL: "https://roborev.io",
		Icons: []mcp.Icon{{
			Source:   serverIconSource,
			MIMEType: "image/png",
			Sizes:    []string{"256x256"},
		}},
	}
}
