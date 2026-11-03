package testutil

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// RunPIDNamespaceTest runs this test in a sandbox, optionally keeping it alive
// until cleanup after its runtime record or daemon listener is ready.
func RunPIDNamespaceTest(t *testing.T, command []string, readyPath string) {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	command = append(command, executable, "-test.run=^"+t.Name()+"$", "-test.timeout=30s")
	runPIDNamespaceCommand(t, command, readyPath)
}

func runPIDNamespaceCommand(t *testing.T, command []string, readyPath string) {
	t.Helper()
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("requires bubblewrap for PID namespaces")
	}
	args := []string{"--unshare-pid", "--bind", "/", "/", "--proc", "/proc", "--"}
	probe := exec.CommandContext(t.Context(), bwrap, append(args, "true")...)
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("PID namespaces unavailable: %v: %s", err, output)
	}
	// The sandbox runs until cleanup closes its stdin, which happens after
	// t.Context is canceled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bwrap, append(args, command...)...)
	if readyPath == "" {
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		require.NoError(t, stdin.Close())
		require.NoError(t, cmd.Wait(), "%s", output.String())
	})
	// Wall-clock wait: the sandbox process writes its record or starts its listener.
	require.Eventually(t, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
}
