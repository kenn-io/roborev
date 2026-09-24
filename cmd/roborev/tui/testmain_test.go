package tui

import (
	"os"
	"testing"
	"time"

	"go.kenn.io/roborev/internal/testenv"
)

func TestMain(m *testing.M) {
	// Renderer snapshots use local timestamps. Set the zone before tests start
	// so concurrent time readers never race with a test changing time.Local.
	time.Local = time.UTC
	os.Exit(testenv.RunIsolatedMain(m))
}
