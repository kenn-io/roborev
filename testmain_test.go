package main

import (
	"os"
	"testing"

	kittelemetry "go.kenn.io/kit/telemetry"

	"go.kenn.io/roborev/internal/testenv"
)

// TestMain isolates the root e2e test package from production
// ~/.roborev. TestE2EEnqueueAndReview creates a daemon.NewServer
// which opens activity/error logs at config.DataDir(). It also disables kit
// PostHog telemetry for daemon instances built in-process.
func TestMain(m *testing.M) {
	kittelemetry.DisablePostHogTelemetry()
	os.Exit(testenv.RunIsolatedMain(m))
}
