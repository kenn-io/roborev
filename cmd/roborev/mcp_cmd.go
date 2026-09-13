package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/mcpserver"
	"go.kenn.io/roborev/internal/version"
)

const mcpRequestTimeout = 60 * time.Second

func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol server for roborev review data",
		Long: `Expose read-only roborev review data to MCP clients.

Two transports are available:

  stdio  'roborev mcp serve' runs a stdio server that reads from the daemon
         over its HTTP API. Configure it as a command-based MCP server.
  http   Set [mcp] enabled = true in ~/.roborev/config.toml and restart the
         daemon to serve streamable HTTP at <daemon address>/mcp.

Both transports expose the same tools: status, repositories, branches, jobs,
reviews, comments, and job output. Nothing is written.`,
	}
	cmd.AddCommand(mcpServeCmd())
	return cmd
}

func mcpServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve read-only roborev tools over stdio",
		Long: `Serve the roborev MCP tools over stdin/stdout.

The daemon is started when needed. All diagnostics go to stderr so stdout
carries only protocol messages.

Example client configuration:

  {
    "mcpServers": {
      "roborev": { "command": "roborev", "args": ["mcp", "serve"] }
    }
  }`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The stdio transport owns stdout; keep daemon lifecycle
			// messages on stderr so they never corrupt the protocol stream.
			lifecycleOut = cmd.ErrOrStderr()
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}
			ep := getDaemonEndpoint()
			backend := mcpserver.NewHTTPBackend(ep.BaseURL(), ep.HTTPClient(mcpRequestTimeout))
			server := mcpserver.New(backend, version.Version)
			return server.RunStdio(cmd.Context())
		},
	}
}
