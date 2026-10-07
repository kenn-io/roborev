package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	mcpStatusRuntimeStale = (*daemon.RuntimeInfo).Stale
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
	type target struct {
		endpoints   []daemon.DaemonEndpoint // Probe order.
		backend     daemon.DaemonEndpoint
		expectedPID int
	}
	var targets []target
	if serverAddr != "" {
		ep := getDaemonEndpoint()
		targets = append(targets, target{endpoints: []daemon.DaemonEndpoint{ep}, backend: ep})
	} else {
		runtimes, err := mcpStatusListRuntimes()
		if err != nil {
			return nil, fmt.Errorf("list daemon runtimes: %w", err)
		}
		for _, rt := range runtimes {
			// A crashed daemon's freed port could now belong to another
			// account, and the probe carries auth_key.
			if mcpStatusRuntimeStale(rt) {
				continue
			}
			// Probe the private socket first so auth_key stays off TCP.
			targets = append(targets, target{
				endpoints:   rt.PreferredEndpoints(),
				backend:     rt.Endpoint(),
				expectedPID: rt.PID,
			})
		}
	}

	listeners := []mcpListenerStatus{}
	seen := map[string]bool{}
	for _, tg := range targets {
		for _, ep := range tg.endpoints {
			probe, err := mcpStatusProbe(ep, timeout)
			if err != nil {
				continue
			}
			// A stale runtime record can point at a port now owned by a
			// different daemon; only trust responders that match the record.
			if tg.expectedPID != 0 && probe.PID != tg.expectedPID {
				continue
			}
			if probe.MCPURL != "" && !seen[probe.MCPURL] {
				seen[probe.MCPURL] = true
				listeners = append(listeners, mcpListenerStatus{
					PID:        probe.PID,
					Transport:  "http",
					URL:        probe.MCPURL,
					BackendURL: tg.backend.BaseURL(),
				})
			}
			break
		}
	}
	return listeners, nil
}

func writeMCPStatus(out io.Writer, listeners []mcpListenerStatus, asJSON bool) error {
	if asJSON {
		return json.MarshalEncode(jsontext.NewEncoder(out), listeners)
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
