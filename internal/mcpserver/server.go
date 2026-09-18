package mcpserver

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPPath is the path the streamable HTTP endpoint is served on.
const HTTPPath = "/mcp"

// Server wraps an MCP server whose tools access roborev data through a Backend.
type Server struct {
	backend Backend
	mcp     *mcp.Server
}

// New builds a server exposing the roborev tools. backend must not
// be nil.
func New(backend Backend, version string) *Server {
	if backend == nil {
		panic("mcpserver: backend is required")
	}
	s := &Server{backend: backend}
	s.mcp = mcp.NewServer(
		&mcp.Implementation{Name: "roborev", Version: version},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{
			Resources: &mcp.ResourceCapabilities{},
			Tools:     &mcp.ToolCapabilities{},
		}},
	)
	s.registerTools()
	s.registerWriteTools()
	s.registerGuidance()
	return s
}

// HTTPHandler serves the stateless streamable HTTP transport at HTTPPath.
func (s *Server) HTTPHandler() http.Handler {
	stream := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != HTTPPath {
			http.NotFound(w, r)
			return
		}
		stream.ServeHTTP(w, r)
	})
}

// RunStdio serves one session over the process's stdin and stdout and
// returns when the client disconnects or ctx is canceled.
func (s *Server) RunStdio(ctx context.Context) error {
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// Run serves one session over t. It exists so tests can drive the server
// through in-memory transports.
func (s *Server) Run(ctx context.Context, t mcp.Transport) error {
	return s.mcp.Run(ctx, t)
}
