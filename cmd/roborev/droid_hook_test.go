package main

import (
	"bytes"
	"context"
	"errors"
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
