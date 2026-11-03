package mcpserver

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"net/http"
	"time"

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

// WithIconRoutes serves the public mark outside the next handler's authentication.
// Both aliases serve the PNG declared in serverInfo, including /favicon.ico.
func WithIconRoutes(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeContent(w, r, "icon.png", time.Time{}, bytes.NewReader(serverIconPNG))
	})
	mux.Handle("/icon.png", handler)
	mux.Handle("/favicon.ico", handler)
	mux.Handle("/", next)
	return mux
}
