package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/mcpserver"
	"go.kenn.io/roborev/internal/version"
)

const mcpRequestTimeout = 60 * time.Second

var mcpProbeDaemon = daemon.ProbeDaemonPing

// ensureMCPDaemon starts the discovered daemon when needed. An explicit
// --server address is only probed: auto-start would launch a daemon at the
// configured address rather than the selected one and then fail to connect.
func ensureMCPDaemon() error {
	if serverAddr == "" {
		if err := ensureDaemon(); err != nil {
			return fmt.Errorf("daemon not running: %w", err)
		}
		return nil
	}
	ep := getDaemonEndpoint()
	probe, err := mcpProbeDaemon(ep, 2*time.Second)
	if err != nil {
		return fmt.Errorf("daemon at %s is not running (explicit --server endpoints are not started automatically): %w", ep, err)
	}
	if os.Getenv("ROBOREV_SKIP_VERSION_CHECK") != "1" && probe.Version != version.Version {
		return fmt.Errorf("daemon at %s runs version %s but this CLI is %s; restart that daemon before serving MCP from it",
			ep, probe.Version, version.Version)
	}
	return nil
}

func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol server for roborev review data",
		Long: `Expose roborev review data to MCP clients.

Two transports are available:

  stdio  'roborev mcp serve' runs a stdio server that reads from the daemon
         over its HTTP API. Configure it as a command-based MCP server.
  http   Set [mcp] enabled = true in ~/.roborev/config.toml and restart the
         daemon to serve streamable HTTP at <daemon address>/mcp.

Both transports expose the same tools: status, repositories, branches, jobs,
reviews, comments, and job output. Tools can also comment, close reviews,
snooze Agent Hooks, and complete hook fix sessions. Reviews cannot be started.

'roborev mcp status' lists daemons currently serving the HTTP endpoint.`,
	}
	cmd.AddCommand(mcpServeCmd())
	cmd.AddCommand(mcpStatusCmd())
	cmd.AddCommand(mcpInstallCmd())
	return cmd
}

func mcpServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve roborev tools over stdio",
		Long: `Serve the roborev MCP tools over stdin/stdout.

The daemon is started when needed. With an explicit --server address the
daemon must already be running there; nothing is started. All diagnostics go
to stderr so stdout carries only protocol messages.

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
			if err := ensureMCPDaemon(); err != nil {
				return err
			}
			ep := getDaemonEndpoint()
			backend := mcpserver.NewHTTPBackend(ep.BaseURL(), ep.HTTPClient(mcpRequestTimeout))
			server := mcpserver.New(backend, version.Version)
			return server.RunStdio(cmd.Context())
		},
	}
}
