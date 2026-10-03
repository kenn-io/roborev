package main

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/telemetry"
)

// cliUseCommands lists commands a person types whose work goes through the daemon; agent, hook, service and own-surface commands stay out.
var cliUseCommands = map[string]bool{
	"roborev review":            true,
	"roborev wait":              true,
	"roborev status":            true,
	"roborev show":              true,
	"roborev list":              true,
	"roborev search":            true,
	"roborev comment":           true,
	"roborev respond":           true,
	"roborev close":             true,
	"roborev cancel":            true,
	"roborev fix":               true,
	"roborev refine":            true,
	"roborev run":               true,
	"roborev prompt":            true,
	"roborev analyze":           true,
	"roborev compact":           true,
	"roborev insights":          true,
	"roborev summary":           true,
	"roborev cost":              true,
	"roborev stream":            true,
	"roborev snooze":            true,
	"roborev pause":             true,
	"roborev unpause":           true,
	"roborev export reviews":    true,
	"roborev export ci-metrics": true,
	"roborev export ci-costs":   true,
	"roborev sync now":          true,
	"roborev init":              true,
}

// cliUseLifecyclePaths are daemon requests a command can make around its own work; a restart's shutdown must not use up the report.
var cliUseLifecyclePaths = map[string]bool{
	"/api/ping":                true,
	"/api/shutdown":            true,
	"/api/update/prepare":      true,
	"/api/update/renew":        true,
	"/api/update/release":      true,
	daemon.TelemetryEventsPath: true,
}

var (
	cliTelemetryEnabled = telemetry.EnabledFromEnv
	// A healthy local daemon answers in milliseconds; a hung one costs a command at most this, once.
	cliUseTimeout = time.Second
	cliUsePost    = telemetry.PostAppOpened
	cliUseOnce    sync.Once
	cliUseState   atomic.Pointer[cliUsePostState] // nil until a post starts
)

type cliUsePostState struct{ ctx context.Context } // Done when the post returns or its deadline passes

// armCLIUse installs the daemon response observer for a listed command run by a person with telemetry on.
func armCLIUse(cmd *cobra.Command) {
	if fromSkill || !cliUseCommands[cmd.CommandPath()] || !cliTelemetryEnabled() {
		return
	}
	daemon.SetClientResponseObserver(observeCLIUse)
}

// cliUseEligible reports whether resp answers a product request: 2xx and not a lifecycle path.
func cliUseEligible(req *http.Request, resp *http.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode < 300 && !cliUseLifecyclePaths[req.URL.Path]
}

// observeCLIUse starts the one app_opened post after the first eligible response from ep.
func observeCLIUse(ep daemon.DaemonEndpoint, req *http.Request, resp *http.Response) {
	if !cliUseEligible(req, resp) {
		return
	}
	cliUseOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cliUseTimeout)
		cliUseState.Store(&cliUsePostState{ctx: ctx})
		go func() {
			defer cancel()
			cliUsePost(ctx, ep.HTTPClient(0), ep.BaseURL()+daemon.TelemetryEventsPath, telemetry.SurfaceCLI)
		}()
	})
}

// waitCLIUse waits for a started post to finish or reach its deadline.
func waitCLIUse() {
	if s := cliUseState.Load(); s != nil {
		<-s.ctx.Done()
	}
}
