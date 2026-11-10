package testenv

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "fixture-waiting-child" {
		os.Exit(gitFixtureWaitingChild(os.Args[2]))
	}
	if os.Getenv("ROBOREV_TEST_FIXTURE_RECORDER") == "1" {
		os.Exit(gitFixtureRecorder())
	}
	InstallForgeAPIGuard()
	os.Exit(RunIsolatedMain(m))
}
