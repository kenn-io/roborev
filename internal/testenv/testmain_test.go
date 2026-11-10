package testenv

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Fprintln(os.Stderr, "fixture Git launcher missed dispatch")
		os.Exit(97)
	}
	if os.Getenv("ROBOREV_TEST_FIXTURE_RECORDER") == "1" {
		os.Exit(gitFixtureRecorder())
	}
	InstallForgeAPIGuard()
	os.Exit(RunIsolatedMain(m))
}
