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
	os.Exit(testenv.RunIsolatedMain(m))
}
