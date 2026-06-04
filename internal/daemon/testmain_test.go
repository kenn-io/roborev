package daemon

import (
	"os"
	"testing"

	kittelemetry "go.kenn.io/kit/telemetry"

	"go.kenn.io/roborev/internal/testenv"
)

// TestMain isolates the entire daemon test package from the production
// ~/.roborev directory. Without this, NewServer creates activity/error
// logs at DefaultActivityLogPath() → ~/.roborev/activity.log, polluting
// the production log with test events and confusing running TUIs. It also
// disables kit PostHog telemetry for daemon instances built in-process.
func TestMain(m *testing.M) {
	kittelemetry.DisablePostHogTelemetry()
	os.Exit(testenv.RunIsolatedMain(m))
}
