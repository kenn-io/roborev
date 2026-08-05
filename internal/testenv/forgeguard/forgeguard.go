// Package forgeguard installs the test-only forge API HTTP guard.
package forgeguard

import "go.kenn.io/roborev/internal/testenv"

func init() {
	testenv.InstallForgeAPIGuard()
}
