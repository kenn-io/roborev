package testenv

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	InstallGitHubAPIGuard()
	os.Exit(m.Run())
}
