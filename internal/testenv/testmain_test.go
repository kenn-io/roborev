package testenv

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	InstallForgeAPIGuard()
	os.Exit(m.Run())
}
