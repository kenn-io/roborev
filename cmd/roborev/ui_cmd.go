package main

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
)

var (
	uiEnsureDaemon        = ensureUIDaemon
	uiGetAnyRunningDaemon = getAnyRunningDaemon
	uiListAllRuntimes     = daemon.ListAllRuntimes
	uiProbeDaemon         = daemon.ProbeDaemon
)

func uiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ui [job-id]",
		Short: "Open the Roborev browser UI",
		Args:  validateUIArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if err := uiEnsureDaemon(); err != nil {
				return err
			}
			runtimeInfo, err := uiRuntimeInfo()
			if err != nil {
				return fmt.Errorf("discover daemon: %w", err)
			}
			if runtimeInfo.WebOrigin == "" {
				return fmt.Errorf("the daemon browser listener is disabled")
			}
			target, err := uiURL(runtimeInfo.WebOrigin, args)
			if err != nil {
				return err
			}
			if err := openBrowserURL(target); err != nil {
				return fmt.Errorf("could not open %s: %w; open this URL manually", target, err)
			}
			return nil
		},
	}
}

func ensureUIDaemon() error {
	if serverAddr == "" {
		return ensureDaemon()
	}
	_, err := daemon.ProbeDaemon(getDaemonEndpoint(), 2*time.Second)
	if err != nil {
		return fmt.Errorf("daemon error: %w", err)
	}
	return nil
}

func uiRuntimeInfo() (*daemon.RuntimeInfo, error) {
	if serverAddr == "" {
		return uiGetAnyRunningDaemon()
	}
	selected := getDaemonEndpoint()
	probe, err := uiProbeDaemon(selected, 2*time.Second)
	if err != nil {
		return nil, err
	}
	runtimes, err := uiListAllRuntimes()
	if err != nil {
		return nil, err
	}
	if probe.PID != 0 {
		for _, runtimeInfo := range runtimes {
			if runtimeInfo.PID == probe.PID {
				return runtimeInfo, nil
			}
		}
	} else {
		for _, runtimeInfo := range runtimes {
			if slices.Contains(runtimeInfo.Endpoints(), selected) {
				return runtimeInfo, nil
			}
		}
	}
	return nil, fmt.Errorf("browser metadata is unavailable for the selected daemon")
}

func validateUIArgs(_ *cobra.Command, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("accepts at most one job ID")
	}
	if len(args) == 1 {
		jobID, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || jobID <= 0 {
			return fmt.Errorf("job ID must be a positive integer")
		}
	}
	return nil
}

func uiURL(origin string, args []string) (string, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("daemon published an invalid browser origin")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("daemon published an invalid browser origin")
	}
	parsed.Path = "/reviews"
	if len(args) == 1 {
		parsed.Path += "/" + args[0]
	}
	return parsed.String(), nil
}
