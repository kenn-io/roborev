package mcpserver

import (
	"context"
	_ "embed"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const guidanceResourceURI = "roborev://mcp/guidance"

//go:embed guidance.md
var guidanceMarkdown string

func (s *Server) registerGuidance() {
	s.mcp.AddResource(&mcp.Resource{
		URI:         guidanceResourceURI,
		Name:        "roborev-mcp-guidance",
		Title:       "Roborev MCP Guidance",
		Description: "How to navigate roborev review data with MCP tools.",
		MIMEType:    "text/markdown",
	}, readGuidanceResource)
}

func readGuidanceResource(
	_ context.Context,
	req *mcp.ReadResourceRequest,
) (*mcp.ReadResourceResult, error) {
	if req.Params.URI != guidanceResourceURI {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      guidanceResourceURI,
			MIMEType: "text/markdown",
			Text:     guidanceMarkdown,
		}},
	}, nil
}
