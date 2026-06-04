package main

import (
	"os"
	"testing"

	kittelemetry "go.kenn.io/kit/telemetry"

	"go.kenn.io/roborev/internal/testenv"
)

// TestMain isolates the entire test package from the real ~/.roborev directory
// and disables external I/O in tests that call newTuiModel by passing
// WithExternalIODisabled(). This prevents the 200+ tests from spawning
// git subprocesses and exhausting macOS CI runner resources. It also disables
// kit PostHog telemetry for daemon instances built in-process.
func TestMain(m *testing.M) {
	kittelemetry.DisablePostHogTelemetry()
	os.Exit(testenv.RunIsolatedMain(m))
}
