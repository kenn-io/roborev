package daemon

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/version"
)

func TestMCPServerIconRoutes(t *testing.T) {
	for _, key := range []string{"", "synthetic-icon-key"} {
		t.Run("key="+key, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			cfg := config.DefaultConfig()
			cfg.MCP.Enabled = true
			cfg.AuthKey = key
			server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			handler := server.httpServer.Handler

			const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"synthetic-client","version":"1"}}}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(initialize))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			got := httptest.NewRecorder()
			handler.ServeHTTP(got, req)
			require.Equal(t, http.StatusOK, got.Code, got.Body.String())
			var response struct {
				Result mcp.InitializeResult `json:"result"`
			}
			// The default streamable transport returns JSON-RPC in an SSE event.
			var data string
			for line := range strings.SplitSeq(got.Body.String(), "\n") {
				if strings.HasPrefix(line, "data: {") {
					data = strings.TrimPrefix(line, "data: ")
					break
				}
			}
			require.NotEmpty(t, data, got.Body.String())
			require.NoError(t, json.Unmarshal([]byte(data), &response))
			info := response.Result.ServerInfo
			require.NotNil(t, info)
			assert.Equal(t, "roborev", info.Name)
			assert.Equal(t, version.Version, info.Version)
			assert.Equal(t, "https://roborev.io", info.WebsiteURL)
			require.Len(t, info.Icons, 1)
			const prefix = "data:image/png;base64,"
			require.True(t, strings.HasPrefix(info.Icons[0].Source, prefix))
			declared, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(info.Icons[0].Source, prefix))
			require.NoError(t, err)

			for _, path := range []string{"/icon.png", "/favicon.ico"} {
				for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
					t.Run(method+path, func(t *testing.T) {
						assert := assert.New(t)
						got := httptest.NewRecorder()
						req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
						// Public images also work without a client certificate on TLS listeners.
						req.TLS = &tls.ConnectionState{}
						handler.ServeHTTP(got, req)
						if method != http.MethodGet && method != http.MethodHead {
							assert.Equal(http.StatusMethodNotAllowed, got.Code)
							assert.Equal("GET, HEAD", got.Header().Get("Allow"))
							return
						}
						require.Equal(t, http.StatusOK, got.Code)
						assert.Equal("image/png", got.Header().Get("Content-Type"))
						assert.Equal("nosniff", got.Header().Get("X-Content-Type-Options"))
						assert.Equal("public, max-age=86400", got.Header().Get("Cache-Control"))
						assert.Equal(strconv.Itoa(len(declared)), got.Header().Get("Content-Length"))
						assert.Empty(got.Header().Get("WWW-Authenticate"))
						if method == http.MethodHead {
							assert.Empty(got.Body.Bytes())
						} else {
							assert.Equal(declared, got.Body.Bytes())
						}
					})
				}
			}

			if key != "" {
				for _, path := range []string{"/mcp", "/api/status", "/icon.png/extra", "/unknown-icon"} {
					for _, authorization := range []string{"", "Bearer wrong-key"} {
						got := httptest.NewRecorder()
						req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
						req.Header.Set("Authorization", authorization)
						handler.ServeHTTP(got, req)
						assert.Equal(t, http.StatusUnauthorized, got.Code, path)
					}
				}
			}
		})
	}
}

func TestMCPServerIconRoutesDisabled(t *testing.T) {
	server, _ := newMCPTestServer(t, false)
	for _, path := range []string{"/icon.png", "/favicon.ico"} {
		got := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(got, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, got.Code, path)
	}
}
