package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
)

// mcpListenerStatus is the CLI discovery contract for running HTTP MCP
// listeners. BackendURL is the daemon API base the listener reads from, so a
// host can match the listener to the daemon it already selected.
type mcpListenerStatus struct {
	PID        int    `json:"pid"`
	Transport  string `json:"transport"`
	URL        string `json:"url"`
	BackendURL string `json:"backend_url"`
}

var (
	mcpStatusListRuntimes = daemon.ListAllRuntimes
	mcpStatusProbe        = daemon.ProbeDaemonPing
)

func mcpStatusCmd() *cobra.Command {
	var asJSON bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List running HTTP MCP listeners",
		Long: `List daemons that currently serve the streamable HTTP MCP endpoint.

Nothing is started. Each entry names the MCP URL and the daemon API base URL
it reads from. With --json the output is a JSON array, empty when no daemon
has [mcp] enabled.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			listeners, err := discoverMCPListeners(timeout)
			if err != nil {
				return err
			}
			return writeMCPStatus(cmd.OutOrStdout(), listeners, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "render output as JSON")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Second, "daemon probe timeout")
	return cmd
}

// discoverMCPListeners probes every recorded daemon (or only the --server
// endpoint when set) and keeps the ones advertising an MCP URL.
func discoverMCPListeners(timeout time.Duration) ([]mcpListenerStatus, error) {
	var endpoints []daemon.DaemonEndpoint
	expectedPIDs := map[daemon.DaemonEndpoint]int{}
	if serverAddr != "" {
		endpoints = append(endpoints, getDaemonEndpoint())
	} else {
		runtimes, err := mcpStatusListRuntimes()
		if err != nil {
			return nil, fmt.Errorf("list daemon runtimes: %w", err)
		}
		for _, rt := range runtimes {
			ep := rt.Endpoint()
			endpoints = append(endpoints, ep)
			expectedPIDs[ep] = rt.PID
		}
	}

	listeners := []mcpListenerStatus{}
	seen := map[string]bool{}
	for _, ep := range endpoints {
		probe, err := mcpStatusProbe(ep, timeout)
		if err != nil || probe.MCPURL == "" {
			continue
		}
		// A stale runtime record can point at a port now owned by a
		// different daemon; only trust responders that match the record.
		if expected, ok := expectedPIDs[ep]; ok && expected != 0 && probe.PID != expected {
			continue
		}
		if seen[probe.MCPURL] {
			continue
		}
		seen[probe.MCPURL] = true
		listeners = append(listeners, mcpListenerStatus{
			PID:        probe.PID,
			Transport:  "http",
			URL:        probe.MCPURL,
			BackendURL: ep.BaseURL(),
		})
	}
	return listeners, nil
}

func writeMCPStatus(out io.Writer, listeners []mcpListenerStatus, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(listeners)
	}
	if len(listeners) == 0 {
		_, err := fmt.Fprintln(out, "No HTTP MCP listeners are running.")
		return err
	}
	for _, l := range listeners {
		if _, err := fmt.Fprintf(out, "MCP %s (pid %d, daemon %s)\n", l.URL, l.PID, l.BackendURL); err != nil {
			return err
		}
	}
	return nil
}
