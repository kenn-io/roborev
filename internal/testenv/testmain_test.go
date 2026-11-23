package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "agentsview" {
		os.Exit(fakeAgentsview())
	}
	if os.Getenv("TEST_ISOLATED_CHILD") != "" {
		os.Exit(RunIsolatedMain(m))
	}
	InstallForgeAPIGuard()
	os.Exit(m.Run())
}
