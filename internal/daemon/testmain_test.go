package daemon

import (
	"fmt"
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
	// Re-invoked as a fake `kata` binary by tests that copy the test
	// executable onto PATH as kata (see installFakeKata in
	// ci_poller_test.go) to prove kata content cannot reach CI prompts.
	if os.Getenv("ROBOREV_TEST_FAKE_KATA") == "1" {
		fmt.Println(`{"issues":[{"short_id":"abc4","title":"Leaked task","body":"Secret kata body."}]}`)
		os.Exit(0)
	}
	kittelemetry.DisablePostHogTelemetry()
	os.Exit(testenv.RunIsolatedMain(m))
}
