package mcpserver

import (
	"context"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPPath is the path the streamable HTTP endpoint is served on.
const HTTPPath = "/mcp"

// Server wraps an MCP server whose tools access roborev data through a Backend.
type Server struct {
	backend  Backend
	mcp      *mcp.Server
	activity sync.WaitGroup
}

// New builds a server exposing the roborev tools. backend must not
// be nil.
// activity may be nil; it runs independently and must bound its work.
func New(backend Backend, version string, activity func(context.Context)) *Server {
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
	s.mcp.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" && activity != nil {
				s.activity.Go(func() { activity(context.WithoutCancel(ctx)) })
			}
			return next(ctx, method, req)
		}
	})
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
	defer s.activity.Wait()
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// Run serves one session over t. It exists so tests can drive the server
// through in-memory transports.
func (s *Server) Run(ctx context.Context, t mcp.Transport) error {
	defer s.activity.Wait()
	return s.mcp.Run(ctx, t)
}
