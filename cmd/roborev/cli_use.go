package main

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/streamfmt"
	"go.kenn.io/roborev/internal/telemetry"
)

// cliUseCommands lists commands a person types whose work goes through the daemon; agent, hook, service and own-surface commands stay out.
var cliUseCommands = map[string]bool{
	"roborev review":                 true,
	"roborev wait":                   true,
	"roborev status":                 true,
	"roborev doctor":                 true,
	"roborev show":                   true,
	"roborev list":                   true,
	"roborev search":                 true,
	"roborev comment":                true,
	"roborev respond":                true,
	"roborev close":                  true,
	"roborev cancel":                 true,
	"roborev fix":                    true,
	"roborev refine":                 true,
	"roborev run":                    true,
	"roborev prompt":                 true,
	"roborev analyze":                true,
	"roborev compact":                true,
	"roborev insights":               true,
	"roborev summary":                true,
	"roborev cost":                   true,
	"roborev stream":                 true,
	"roborev snooze":                 true,
	"roborev pause":                  true,
	"roborev unpause":                true,
	"roborev export reviews":         true,
	"roborev export ci-metrics":      true,
	"roborev export ci-costs":        true,
	"roborev sync now":               true,
	"roborev init":                   true,
	"roborev log":                    true,
	"roborev log clean":              true,
	"roborev repo list":              true,
	"roborev repo show":              true,
	"roborev repo rename":            true,
	"roborev repo move":              true,
	"roborev repo delete":            true,
	"roborev repo merge":             true,
	"roborev legacy-reviews convert": true,
	"roborev legacy-reviews export":  true,
	"roborev legacy-reviews import":  true,
	"roborev backfill-verdicts":      true,
	"roborev backfill-tokens":        true,
	"roborev sync status":            true,
}

// cliUseLifecyclePaths are daemon requests a command can make around its own work; a restart's shutdown must not use up the report.
var cliUseLifecyclePaths = map[string]bool{
	"/api/ping":                   true,
	"/api/shutdown":               true,
	"/api/update/prepare":         true,
	"/api/update/renew":           true,
	"/api/update/release":         true,
	daemon.TelemetryEventsPath:    true,
	daemon.TelemetryAgentCallPath: true,
}

var agentUseCommands = map[string]bool{
	"roborev agent-hook run":      true,
	"roborev agent-hook fix-done": true,
	"roborev post-commit":         true,
	"roborev enqueue":             true,
	"roborev remap":               true,
}

var (
	cliTelemetryEnabled = telemetry.EnabledFromEnv
	cliOutputIsTerminal = streamfmt.WriterIsTerminal
	// A healthy local daemon answers in milliseconds; a hung one costs a command at most this, once.
	cliUseTimeout  = telemetry.NotificationTimeout
	cliUsePost     = telemetry.PostAppOpened
	cliAgentPost   = telemetry.PostAgentCall
	cliUseOnce     sync.Once
	cliUseState    atomic.Pointer[cliUsePostState] // nil until a post starts
	cliUseAgent    bool
	cliUseEndpoint atomic.Pointer[daemon.DaemonEndpoint]
	cliActions     sync.WaitGroup
)

type cliUsePostState struct{ ctx context.Context } // Done when the post returns or its deadline passes

// armCLIUse selects human or agent activity for commands that work through the daemon.
func armCLIUse(cmd *cobra.Command) {
	path := cmd.CommandPath()
	if (!cliUseCommands[path] && !agentUseCommands[path]) || !cliTelemetryEnabled() {
		return
	}
	cliUseAgent = agentUseCommands[path] || fromSkill
	daemon.SetClientResponseObserver(observeCLIUse)
}

// cliUseEligible reports whether resp answers a product request: 2xx and not a lifecycle path.
func cliUseEligible(req *http.Request, resp *http.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode < 300 && !cliUseLifecyclePaths[req.URL.Path]
}

// observeCLIUse starts one activity post after the first eligible response from ep.
func observeCLIUse(ep daemon.DaemonEndpoint, req *http.Request, resp *http.Response) {
	if !cliUseEligible(req, resp) {
		return
	}
	cliUseEndpoint.Store(&ep)
	cliUseOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cliUseTimeout)
		cliUseState.Store(&cliUsePostState{ctx: ctx})
		go func() {
			defer cancel()
			if cliUseAgent {
				cliAgentPost(ctx, ep.HTTPClient(0), ep.BaseURL()+daemon.TelemetryAgentCallPath)
			} else {
				cliUsePost(ctx, ep.HTTPClient(0), ep.BaseURL()+daemon.TelemetryEventsPath, telemetry.SurfaceCLI)
			}
		}()
	})
}

func reportMCPActivity(ctx context.Context, ep daemon.DaemonEndpoint) {
	if !cliTelemetryEnabled() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, cliUseTimeout)
	defer cancel()
	cliAgentPost(ctx, ep.HTTPClient(0), ep.BaseURL()+daemon.TelemetryAgentCallPath)
}

// waitCLIUse waits for a started post to finish or reach its deadline.
func waitCLIUse() {
	cliActions.Wait()
	if s := cliUseState.Load(); s != nil {
		<-s.ctx.Done()
	}
}

// reportCLIActionOutput bounds best-effort reporting independently of the command's lifetime.
func reportCLIActionOutput(out io.Writer, event string) {
	reportCLIOutcome(out, event, nil)
}

func reportCLIError(out io.Writer, kind string) {
	reportCLIOutcome(out, telemetry.EventErrorShown, map[string]any{telemetry.PropertyKind: kind})
}

func reportCLIOutcome(out io.Writer, event string, props map[string]any) {
	if fromSkill || cliUseAgent || !cliTelemetryEnabled() || !cliOutputIsTerminal(out) {
		return
	}
	ep := cliUseEndpoint.Load()
	if ep == nil && event == telemetry.EventErrorShown && parsedServerEndpoint != nil && !parsedServerEndpoint.IsRemote() {
		ep = parsedServerEndpoint
	}
	if ep == nil && event != telemetry.EventErrorShown && (parsedServerEndpoint == nil || !parsedServerEndpoint.IsRemote()) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliUseTimeout)
	cliActions.Go(func() {
		defer cancel()
		if ep == nil {
			info, err := getAnyRunningDaemonForStart(ctx)
			if err != nil || info == nil {
				return
			}
			ep = &daemon.DaemonEndpoint{Network: info.Network, Address: info.Address}
		}
		if props == nil {
			props = make(map[string]any)
		}
		props[telemetry.PropertySurface] = telemetry.SurfaceCLI
		_, _ = posthog.PostEvent(ctx, ep.HTTPClient(0), ep.BaseURL()+daemon.TelemetryEventsPath, event, props)
	})
}
