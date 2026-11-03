package main

import (
	"os"
	"testing"

	"go.kenn.io/roborev/internal/testenv"
)

// TestMain isolates the root e2e test package from production
// ~/.roborev. TestE2EEnqueueAndReview creates a daemon.NewServer
// which opens activity/error logs at config.DataDir().
func TestMain(m *testing.M) {
	os.Exit(testenv.RunIsolatedMain(m))
}
