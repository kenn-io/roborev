//go:build !windows

package agent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/proctrack"
)

// etxtbsyGuard keeps writeTempCommand's startup probe from running the script.
const etxtbsyGuard = "case \"$1\" in *etxtbsy*) exit 0;; esac\n"

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func requireProcessGone(t *testing.T, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, 5*time.Second, 10*time.Millisecond, "process %d still running", pid)
}

// A sleeping grandchild that inherited stdout used to hold Claude's schema
// read open until the job timed out. In a tracked attempt the grandchild is
// stopped when Claude exits, and the result it already wrote is kept.
func TestClaudeSchemaResultWhileGrandchildHoldsStdout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; --help) echo 'usage: claude --tools'; exit 0;; esac
sleep 60 &
echo $! > %q
echo '{"type":"result","result":"{\"findings\":[]}"}'
`, pidFile)
	a := NewClaudeAgent(writeTempCommand(t, script))
	ctx, scope := proctrack.New(t.Context(), 0)
	defer scope.Close()

	began := time.Now()
	value, err := a.ReviewWithSchema(ctx, t.TempDir(), "HEAD", "review", jsontext.Value(`{"type":"object"}`), nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{"findings":[]}`, string(value))
	assert.Less(t, time.Since(began), 10*time.Second)
	requireProcessGone(t, readPIDFile(t, pidFile))
}

func TestACPTerminalExitStopsItsGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmdPath := writeTempCommand(t, fmt.Sprintf("#!/bin/sh\n"+etxtbsyGuard+"sleep 60 &\necho $! > %q\n", pidFile))
	client := newTerminalTestClient(t, t.TempDir())
	_, scope := proctrack.New(t.Context(), 0)
	defer scope.Close()
	client.procScope = scope

	created, err := client.CreateTerminal(t.Context(), acp.CreateTerminalRequest{
		SessionId: acp.SessionId("session-1"), Command: cmdPath,
	})
	require.NoError(t, err)
	_, err = client.WaitForTerminalExit(t.Context(), acp.WaitForTerminalExitRequest{
		SessionId: acp.SessionId("session-1"), TerminalId: created.TerminalId,
	})
	require.NoError(t, err)
	requireProcessGone(t, readPIDFile(t, pidFile))
}

func TestACPTerminalRefusedAfterAttemptCleanup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cmdPath := writeTempCommand(t, fmt.Sprintf("#!/bin/sh\n"+etxtbsyGuard+"touch %q\n", marker))
	client := newTerminalTestClient(t, t.TempDir())
	_, scope := proctrack.New(t.Context(), 0)
	require.NoError(t, scope.Close())
	client.procScope = scope

	_, err := client.CreateTerminal(t.Context(), acp.CreateTerminalRequest{
		SessionId: acp.SessionId("session-1"), Command: cmdPath,
	})
	require.ErrorIs(t, err, proctrack.ErrClosed)
	time.Sleep(100 * time.Millisecond)
	assert.NoFileExists(t, marker)
}

// A background job in its own process group survives the terminal group's
// stop and keeps the output pipe open; waiting for the terminal used to hang
// until that job exited.
func TestACPTerminalExitWithDetachedOutputHolder(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// perl leaves the group, then records its pid; the script exits only after
	// that, so the group stop cannot catch it first.
	cmdPath := writeTempCommand(t, fmt.Sprintf(`#!/bin/sh
`+etxtbsyGuard+`perl -e 'setpgrp(0, 0); open(my $f, ">", $ARGV[0]) or die; print $f "$$\n"; close $f; exec "sleep", "60"' %[1]q &
while [ ! -s %[1]q ]; do sleep 0.01; done
echo finished
`, pidFile))
	client := newTerminalTestClient(t, t.TempDir())
	_, scope := proctrack.New(t.Context(), 0)
	defer scope.Close()
	client.procScope = scope

	created, err := client.CreateTerminal(t.Context(), acp.CreateTerminalRequest{
		SessionId: acp.SessionId("session-1"), Command: cmdPath,
	})
	require.NoError(t, err)
	holder := readPIDFile(t, pidFile)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = client.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{
		SessionId: acp.SessionId("session-1"), TerminalId: created.TerminalId,
	})
	require.NoError(t, err)
	out, err := client.TerminalOutput(t.Context(), acp.TerminalOutputRequest{
		SessionId: acp.SessionId("session-1"), TerminalId: created.TerminalId,
	})
	require.NoError(t, err)
	assert.Contains(t, out.Output, "finished")
	require.NoError(t, syscall.Kill(holder, 0), "the detached holder must still be running: %s", out.Output)
}
