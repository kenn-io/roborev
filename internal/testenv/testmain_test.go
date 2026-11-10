package testenv

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if marker := os.Getenv("ROBOREV_TEST_STALLED_GO"); marker != "" {
		_ = os.WriteFile(marker, nil, 0o600)
		os.Exit(0)
	}
	InstallForgeAPIGuard()
	os.Exit(m.Run())
}
