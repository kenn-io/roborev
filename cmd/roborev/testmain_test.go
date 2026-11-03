package main

import (
	"fmt"
	"os"
	"testing"

	kittelemetry "go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/roborev/internal/testenv"
)

// updateDaemonBuildDir belongs to the package run, so both integration tests
// can use its binaries after the first test finishes.
var updateDaemonBuildDir string

// TestMain isolates the entire test package from the real ~/.roborev directory
// and disables external I/O in tests that call newTuiModel by passing
// WithExternalIODisabled(). This prevents the 200+ tests from spawning
// git subprocesses and exhausting macOS CI runner resources. It also disables
// kit PostHog telemetry except in the dedicated telemetry test process.
func TestMain(m *testing.M) {
	if os.Getenv("ROBOREV_TEST_DAEMON_TELEMETRY") != "1" {
		kittelemetry.DisableProcess()
	}
	code := testenv.RunIsolatedMain(m)
	if updateDaemonBuildDir != "" {
		if err := os.RemoveAll(updateDaemonBuildDir); err != nil {
			fmt.Fprintf(os.Stderr, "remove update daemon binaries: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}
