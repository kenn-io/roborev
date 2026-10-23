package testutil

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

// UnjudgedRuntimeRecord uses a live foreign PID namespace so host-side
// discovery cannot prove that its process is gone.
func UnjudgedRuntimeRecord(t *testing.T, network, address string) kitdaemon.RuntimeRecord {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux PID namespaces")
	}

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	t.Setenv("ROBOREV_TEST_RUNTIME_WRITER", ready)
	executable, err := os.Executable()
	require.NoError(t, err)
	runPIDNamespaceCommand(t, []string{executable}, ready)
	runtimePath, err := os.ReadFile(ready)
	require.NoError(t, err)
	rec, err := (kitdaemon.RuntimeStore{Dir: filepath.Dir(string(runtimePath))}).Read(string(runtimePath))
	require.NoError(t, err)
	rec.Network, rec.Address = network, address
	require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(rec))
	return rec
}

// UnknownRuntimeRecord represents an identity this version cannot judge.
func UnknownRuntimeRecord(network, address string) kitdaemon.RuntimeRecord {
	return kitdaemon.RuntimeRecord{
		PID: math.MaxInt32, Network: network, Address: address,
		ProcessIdentityV2: "future-v1:unavailable",
	}
}

// RuntimeWriterMain handles the namespace fixture subprocess before TestMain
// replaces its data directory. The caller supplies the application's writer.
func RuntimeWriterMain(write func() (string, error)) bool {
	ready := os.Getenv("ROBOREV_TEST_RUNTIME_WRITER")
	if ready == "" {
		return false
	}
	must := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	must(os.Setenv("ROBOREV_DATA_DIR", filepath.Dir(ready)))
	must(os.Setenv("XDG_RUNTIME_DIR", filepath.Join(filepath.Dir(ready), "runtime")))
	must(os.MkdirAll(os.Getenv("XDG_RUNTIME_DIR"), 0o700))
	path, err := write()
	must(err)
	must(os.WriteFile(ready, []byte(path), 0o600))
	_, err = io.Copy(io.Discard, os.Stdin)
	must(err)
	return true
}
