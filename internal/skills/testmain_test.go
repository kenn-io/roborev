package skills

import (
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/roborev/internal/testenv"
)

var authenticatedCodexHome string

func TestMain(m *testing.M) {
	authenticatedCodexHome = os.Getenv("CODEX_HOME")
	if authenticatedCodexHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			authenticatedCodexHome = filepath.Join(home, ".codex")
		}
	}
	if authenticatedCodexHome != "" {
		if absolute, err := filepath.Abs(authenticatedCodexHome); err == nil {
			authenticatedCodexHome = absolute
		} else {
			authenticatedCodexHome = ""
		}
	}
	os.Exit(testenv.RunIsolatedMain(m))
}
