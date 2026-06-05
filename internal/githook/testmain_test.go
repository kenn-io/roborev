package githook

import (
	"os"
	"testing"

	"go.kenn.io/roborev/internal/testenv"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunIsolatedMain(m))
}
