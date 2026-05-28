package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSessionID(t *testing.T) {
	t.Parallel()

	t.Run("Valid session ID matching agent session", func(t *testing.T) {
		agent := &ACPAgent{
			SessionID: "test-session-123",
		}

		client := &acpClient{
			agent: agent,
		}

		sessionID := acp.SessionId("test-session-123")
		err := client.validateSessionID(sessionID)
		require.NoError(t, err, "Expected no error for matching session ID")
	})

	t.Run("Invalid session ID not matching agent session", func(t *testing.T) {
		agent := &ACPAgent{
			SessionID: "test-session-123",
		}

		client := &acpClient{
			agent: agent,
		}

		sessionID := acp.SessionId("wrong-session-456")
		err := client.validateSessionID(sessionID)
		assert.EqualError(t, err, "session ID mismatch: expected test-session-123, got wrong-session-456")
	})

	t.Run("Empty agent session ID rejects non-empty request session ID", func(t *testing.T) {
		agent := &ACPAgent{
			SessionID: "",
		}

		client := &acpClient{
			agent: agent,
		}

		sessionID := acp.SessionId("any-session-789")
		err := client.validateSessionID(sessionID)
		require.Error(t, err, "Expected error for non-empty request session ID before session is established")
		assert.EqualError(t, err, "session ID mismatch: no active session, got any-session-789")
	})

	t.Run("Empty request session ID with non-empty agent session", func(t *testing.T) {
		agent := &ACPAgent{
			SessionID: "test-session-123",
		}

		client := &acpClient{
			agent: agent,
		}

		sessionID := acp.SessionId("")
		err := client.validateSessionID(sessionID)
		require.Error(t, err, "Expected error for empty session ID when agent has session ID")
		assert.EqualError(t, err, "session ID mismatch: expected test-session-123, got ")
	})

	t.Run("Both session IDs empty", func(t *testing.T) {
		agent := &ACPAgent{
			SessionID: "",
		}

		client := &acpClient{
			agent: agent,
		}

		sessionID := acp.SessionId("")
		err := client.validateSessionID(sessionID)
		require.NoError(t, err, "Expected no error when both session IDs are empty")
	})

	t.Run("Client session ID takes precedence over agent session ID", func(t *testing.T) {
		agent := &ACPAgent{
			SessionID: "agent-session",
		}
		client := &acpClient{
			agent:     agent,
			sessionID: "client-session",
		}

		require.NoError(t, client.validateSessionID(acp.SessionId("client-session")), "Expected client session ID to validate")

		err := client.validateSessionID(acp.SessionId("agent-session"))
		require.Error(t, err, "Expected mismatch when request matches agent session but not client session")

		if !strings.Contains(err.Error(), "expected client-session") {
			require.Failf(t, "unexpected error", "Expected error to use client session ID, got: %v", err)
		}
	})
}

func TestValidateAndResolvePathUsesClientRepoRootPrecedence(t *testing.T) {
	t.Parallel()

	clientRoot := t.TempDir()
	agentRoot := t.TempDir()

	clientFile := filepath.Join(clientRoot, "client.txt")
	if err := os.WriteFile(clientFile, []byte("ok"), 0o644); err != nil {
		require.NoError(t, err, "Failed to create client-root file: %v")
	}

	agent := &ACPAgent{
		repoRoot: agentRoot,
	}
	client := &acpClient{
		agent:    agent,
		repoRoot: clientRoot,
	}

	resolvedPath, err := client.validateAndResolvePath("client.txt", false)
	require.NoError(t, err, "Expected client repo root to be used, got error: %v")

	resolvedClientRoot, err := filepath.EvalSymlinks(clientRoot)
	require.NoError(t, err, "Failed to resolve client root: %v")

	resolvedAgentRoot, err := filepath.EvalSymlinks(agentRoot)
	require.NoError(t, err, "Failed to resolve agent root: %v")

	assert.True(t, pathWithinRoot(resolvedPath, resolvedClientRoot), "Expected resolved path %q to be under client repo root %q", resolvedPath, resolvedClientRoot)
	assert.False(t, pathWithinRoot(resolvedPath, resolvedAgentRoot), "Expected resolved path %q to avoid agent repo root %q", resolvedPath, resolvedAgentRoot)
}

func TestValidateConfiguredSessionCapabilities(t *testing.T) {
	t.Parallel()

	t.Run("Configured mode requires mode capability", func(t *testing.T) {
		err := validateConfiguredMode("plan", nil)
		require.Error(t, err, "Expected error when mode is configured but session mode capability is absent")

		if !strings.Contains(err.Error(), "does not support session modes") {
			require.Failf(t, "unexpected error", "Expected unsupported mode capability error, got: %v", err)
		}
	})

	t.Run("Configured model requires model capability", func(t *testing.T) {
		err := validateConfiguredModel("model-x", nil)
		require.Error(t, err, "Expected error when model is configured but session model capability is absent")

		if !strings.Contains(err.Error(), "does not support session models") {
			require.Failf(t, "unexpected error", "Expected unsupported model capability error, got: %v", err)
		}
	})

	t.Run("Mode and model validation succeeds when available", func(t *testing.T) {
		modeState := &acp.SessionModeState{
			AvailableModes: []acp.SessionMode{
				{Id: acp.SessionModeId("plan"), Name: "Plan"},
			},
			CurrentModeId: acp.SessionModeId("plan"),
		}
		modelState := &acp.SessionModelState{
			AvailableModels: []acp.ModelInfo{
				{ModelId: acp.ModelId("model-x"), Name: "Model X"},
			},
			CurrentModelId: acp.ModelId("model-x"),
		}

		if err := validateConfiguredMode("plan", modeState); err != nil {
			require.NoError(t, err, "Expected mode validation success, got: %v")
		}
		if err := validateConfiguredModel("model-x", modelState); err != nil {
			require.NoError(t, err, "Expected model validation success, got: %v")
		}
	})
}

func TestSessionUpdateValidatesSessionID(t *testing.T) {
	t.Parallel()

	client := &acpClient{
		agent: &ACPAgent{
			SessionID: "expected-session",
		},
		result: &bytes.Buffer{},
	}

	messageChunk := &acp.SessionUpdateAgentMessageChunk{
		Content:       acp.TextBlock("hello"),
		SessionUpdate: "agent_message_chunk",
	}

	err := client.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: acp.SessionId("wrong-session"),
		Update: acp.SessionUpdate{
			AgentMessageChunk: messageChunk,
		},
	})
	require.NoError(t, err, "SessionUpdate should not return error for mismatched session ID")

	if client.result.String() != "" {
		assert.Empty(t, client.result.String(), "Expected no output to be appended on session mismatch, got: %q", client.result.String())
	}

	if err := client.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: acp.SessionId("expected-session"),
		Update: acp.SessionUpdate{
			AgentMessageChunk: messageChunk,
		},
	}); err != nil {
		require.NoError(t, err, "Expected valid SessionUpdate to succeed, got: %v")
	}
	assert.Equal(t, "hello", client.result.String(), "Expected session output to be appended, got: %q", client.result.String())
}

// TestSessionUpdateRejectsBeforeSessionEstablished verifies that an early
// SessionUpdate notification cannot bind c.sessionID before NewSession has
// completed. A spoofed or stale notification arriving before the session is
// established must be rejected, not used to bootstrap the client's session ID.
func TestSessionUpdateRejectsBeforeSessionEstablished(t *testing.T) {
	t.Parallel()

	client := &acpClient{
		agent:  &ACPAgent{},
		result: &bytes.Buffer{},
	}

	messageChunk := &acp.SessionUpdateAgentMessageChunk{
		Content:       acp.TextBlock("attacker-content"),
		SessionUpdate: "agent_message_chunk",
	}

	err := client.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: acp.SessionId("attacker-session"),
		Update: acp.SessionUpdate{
			AgentMessageChunk: messageChunk,
		},
	})
	require.NoError(t, err, "SessionUpdate should not return an error when rejecting a notification")

	assert.Empty(t, client.sessionID, "SessionUpdate must not bootstrap c.sessionID from an incoming notification")
	assert.Empty(t, client.result.String(), "Rejected SessionUpdate must not append output")

	// After NewSession would set the session ID, subsequent updates with the
	// real session ID are accepted, while the earlier "attacker-session" ID
	// remains rejected.
	client.sessionID = "real-session"

	err = client.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: acp.SessionId("attacker-session"),
		Update: acp.SessionUpdate{
			AgentMessageChunk: messageChunk,
		},
	})
	require.NoError(t, err)
	assert.Empty(t, client.result.String(), "Stale notification for non-matching session must remain rejected")

	err = client.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: acp.SessionId("real-session"),
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content:       acp.TextBlock("real-content"),
				SessionUpdate: "agent_message_chunk",
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "real-content", client.result.String(), "Valid SessionUpdate after NewSession must be accepted")
}
