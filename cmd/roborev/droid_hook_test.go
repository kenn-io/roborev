package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/droidhook"
)

func TestDroidHookCmdHasRunSubcommand(t *testing.T) {
	sub, _, err := droidHookCmd().Find([]string{"run"})
	require.NoError(t, err)
	assert.Equal(t, "run", sub.Name())
}

func TestRunDroidHookFailsOpenWhenDaemonUnavailable(t *testing.T) {
	assert := assert.New(t)
	oldPost := postAgentHook
	postAgentHook = func(context.Context, agenthook.Request) (agenthook.Response, error) {
		return agenthook.Response{}, errors.New("daemon unavailable")
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout, stderr bytes.Buffer
	err := runHook(
		droidhook.DefaultOptions(),
		"droid-hook",
		strings.NewReader(`{"session_id":"session-1","hook_event_name":"Stop"}`),
		&stdout,
		&stderr,
	)

	require.NoError(t, err)
	assert.JSONEq(`{}`, stdout.String())
	assert.Contains(stderr.String(), "roborev droid-hook:")
	assert.Contains(stderr.String(), "daemon unavailable")
}

func TestRunDroidHookRejectsMissingSessionID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runHook(
		droidhook.DefaultOptions(),
		"droid-hook",
		strings.NewReader(`{"hook_event_name":"Stop"}`),
		&stdout,
		&stderr,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing session_id")
}

func TestRunDroidHookEmitsBlockWhenTriggered(t *testing.T) {
	oldPost := postAgentHook
	postAgentHook = func(_ context.Context, req agenthook.Request) (agenthook.Response, error) {
		return agenthook.Response{
			SessionID: req.Event.SessionID,
			Triggered: true,
			Reason:    "open failed roborev reviews.",
		}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout, stderr bytes.Buffer
	err := runHook(
		droidhook.DefaultOptions(),
		"droid-hook",
		strings.NewReader(`{"session_id":"session-1","hook_event_name":"Stop"}`),
		&stdout,
		&stderr,
	)
	require.NoError(t, err)
	assert.JSONEq(t, `{"decision":"block","reason":"open failed roborev reviews."}`, stdout.String())
}

func TestRunDroidHookEmitsEmptyWhenNotTriggered(t *testing.T) {
	oldPost := postAgentHook
	postAgentHook = func(_ context.Context, req agenthook.Request) (agenthook.Response, error) {
		return agenthook.Response{SessionID: req.Event.SessionID, Triggered: false}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout, stderr bytes.Buffer
	err := runHook(
		droidhook.DefaultOptions(),
		"droid-hook",
		strings.NewReader(`{"session_id":"session-1","hook_event_name":"Stop"}`),
		&stdout,
		&stderr,
	)
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, stdout.String())
}

func TestDroidHookCmdHasInstallAndDumpSubcommands(t *testing.T) {
	for _, name := range []string{"install", "dump"} {
		sub, _, err := droidHookCmd().Find([]string{name})
		require.NoError(t, err)
		assert.Equal(t, name, sub.Name())
	}
}

func TestDroidHookCmdHasStatusAndResetSubcommands(t *testing.T) {
	for _, name := range []string{"status", "reset"} {
		sub, _, err := droidHookCmd().Find([]string{name})
		require.NoError(t, err)
		assert.Equal(t, name, sub.Name())
	}
}

func TestDroidHookInstallCmdFlags(t *testing.T) {
	cmd := droidHookInstallCmd()
	for _, flag := range []string{"command", "binary", "config", "scope", "timeout", "dry-run"} {
		require.NotNil(t, cmd.Flags().Lookup(flag), "missing flag %s", flag)
	}
}

func TestDroidHookDumpCmdFlags(t *testing.T) {
	cmd := droidHookDumpCmd()
	for _, flag := range []string{"command", "config", "scope", "timeout"} {
		require.NotNil(t, cmd.Flags().Lookup(flag), "missing flag %s", flag)
	}
	assert.Nil(t, cmd.Flags().Lookup("binary"))
	assert.Nil(t, cmd.Flags().Lookup("dry-run"))
}

func TestDroidHookInstallCmdWritesHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	cmd := droidHookInstallCmd()
	cmd.SetArgs([]string{
		"--command", "/tmp/roborev droid-hook run",
		"--config", path,
		"--scope", "user",
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "installed Factory Droid hooks")

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "droid-hook run")
	assert.Contains(t, string(body), `"Execute"`)
	assert.Contains(t, string(body), `"Stop"`)
}
