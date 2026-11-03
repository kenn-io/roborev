package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

// TestMain isolates the entire daemon test package from the production
// ~/.roborev directory. Without this, NewServer creates activity/error
// logs at DefaultActivityLogPath() → ~/.roborev/activity.log, polluting
// the production log with test events and confusing running TUIs.
func TestMain(m *testing.M) {
	if testutil.RuntimeWriterMain(func() (string, error) {
		err := WriteRuntime(DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:1"}, nil, "test", nil)
		return RuntimePath(), err
	}) {
		return
	}

	// Re-invoked as a fake `kata` binary by tests that copy the test
	// executable onto PATH as kata (see installFakeKata in
	// ci_poller_test.go) to prove kata content cannot reach CI prompts.
	if os.Getenv("ROBOREV_TEST_FAKE_KATA") == "1" {
		fmt.Println(`{"issues":[{"short_id":"abc4","title":"Leaked task","body":"Secret kata body."}]}`)
		os.Exit(0)
	}
	removeStub := installStubReviewAgent()
	code := testenv.RunIsolatedMain(m)
	removeStub()
	os.Exit(code)
}

// installStubReviewAgent puts a codex stub on PATH so servers built with the
// default config report a runnable review agent, as on a machine with codex
// installed. Tests about agent availability set PATH themselves.
func installStubReviewAgent() func() {
	dir, err := os.MkdirTemp("", "roborev-daemon-test-agents-")
	if err != nil {
		panic(err)
	}
	codex, script := filepath.Join(dir, "codex"), "#!/bin/sh\nexit 1\n"
	if runtime.GOOS == "windows" {
		codex, script = codex+".cmd", "@echo off\r\nexit /b 1\r\n"
	}
	if err := os.WriteFile(codex, []byte(script), 0o755); err != nil {
		panic(err)
	}
	os.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+dir)
	return func() { _ = os.RemoveAll(dir) }
}
