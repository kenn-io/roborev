package main

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kit/consolewindow"

	"go.kenn.io/roborev/internal/mcpconfig"
)

type mcpHeaders struct {
	headers map[string]string
}

func (h mcpHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for key, value := range h.headers {
		req.Header.Set(key, value)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// probeMCPTools confirms the selected connection exposes RoboRev's tools.
func probeMCPTools(ctx context.Context, server mcpconfig.RegisteredServer) error {
	if server.HeadersHelper != "" {
		return fmt.Errorf("dynamic headers require inspection in the agent")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var transport mcp.Transport
	if server.URL != "" {
		headers := make(map[string]string, len(server.Headers)+1)
		maps.Copy(headers, server.Headers)
		if server.BearerTokenEnvVar != "" {
			headers["Authorization"] = "Bearer " + os.Getenv(server.BearerTokenEnvVar)
		}
		client := &http.Client{Transport: mcpHeaders{headers}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		transport = &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true}
	} else {
		cmd := consolewindow.CommandContext(ctx, server.Command, server.Args...)
		cmd.Env = os.Environ()
		for key, value := range server.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: 100 * time.Millisecond}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "roborev-setup", Version: "1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	want := map[string]bool{
		"roborev_list_repos": false, "roborev_list_jobs": false,
		"roborev_get_review": false, "roborev_list_comments": false,
		"roborev_get_job_output": false, "roborev_add_comment": false,
		"roborev_close_review": false, "roborev_snooze": false,
		"roborev_complete_fix": false,
	}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return err
		}
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			return fmt.Errorf("server did not expose %s", name)
		}
	}
	return nil
}
