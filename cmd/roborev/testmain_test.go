package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	kittelemetry "go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/roborev/internal/testenv"
)

// TestMain isolates the entire test package from the real ~/.roborev directory
// and disables external I/O in tests that call newTuiModel by passing
// WithExternalIODisabled(). This prevents the 200+ tests from spawning
// git subprocesses and exhausting macOS CI runner resources. It also disables
// kit PostHog telemetry except in the dedicated telemetry test process.
func TestMain(m *testing.M) {
	if (os.Getenv("ROBOREV_TEST_FAKE_CLAUDE") == "1" || os.Getenv("ROBOREV_TEST_FAKE_CODEX") == "1") && slices.Contains(os.Args, "mcp") && slices.Contains(os.Args, "serve") {
		os.Exit(fakeMCPServer())
	}
	if os.Getenv("ROBOREV_TEST_FAKE_CLAUDE") == "1" {
		fmt.Println("Scope: User config\nStatus: Connected")
		os.Exit(0)
	}
	if os.Getenv("ROBOREV_TEST_FAKE_CODEX") == "1" {
		os.Exit(fakeCodexAppServer())
	}
	if os.Getenv("ROBOREV_TEST_DAEMON_TELEMETRY") != "1" {
		kittelemetry.DisableProcess()
	}
	os.Exit(testenv.RunIsolatedMain(m))
}

func fakeMCPServer() int {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	for _, name := range []string{"roborev_list_repos", "roborev_list_jobs", "roborev_get_review", "roborev_list_comments", "roborev_get_job_output", "roborev_add_comment", "roborev_close_review", "roborev_snooze", "roborev_complete_fix"} {
		if name == "roborev_list_jobs" && os.Getenv("ROBOREV_TEST_WRONG_MCP") != "" {
			continue
		}
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	}
	session, err := server.Connect(context.Background(), &mcp.StdioTransport{}, nil)
	if err != nil {
		return 1
	}
	if err := session.Wait(); err != nil {
		return 1
	}
	return 0
}
