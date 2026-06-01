package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

const (
	authAllowOnceOptionID    acp.PermissionOptionId = "allow-once-id"
	authAllowAlwaysOptionID  acp.PermissionOptionId = "allow-always-id"
	authRejectOnceOptionID   acp.PermissionOptionId = "reject-once-id"
	authRejectAlwaysOptionID acp.PermissionOptionId = "reject-always-id"
)

func authTestPermissionOptions() []acp.PermissionOption {
	return []acp.PermissionOption{
		{
			OptionId: authAllowOnceOptionID,
			Kind:     acp.PermissionOptionKindAllowOnce,
			Name:     "Allow once",
		},
		{
			OptionId: authAllowAlwaysOptionID,
			Kind:     acp.PermissionOptionKindAllowAlways,
			Name:     "Allow always",
		},
		{
			OptionId: authRejectOnceOptionID,
			Kind:     acp.PermissionOptionKindRejectOnce,
			Name:     "Reject once",
		},
		{
			OptionId: authRejectAlwaysOptionID,
			Kind:     acp.PermissionOptionKindRejectAlways,
			Name:     "Reject always",
		},
	}
}

// setupTestClient creates an acpClient with common defaults for auth tests.
// mode is the agent's Mode field. If repoRoot is non-empty, it is set on the
// agent and the client's terminals map is initialized.
func setupTestClient(mode string, repoRoot string) *acpClient {
	agent := &ACPAgent{
		agentName:       "test-acp",
		Command:         "test-command",
		ReadOnlyMode:    "plan",
		AutoApproveMode: "auto-approve",
		Mode:            mode,
		Model:           "test-model",
		Timeout:         30 * time.Second,
	}
	client := &acpClient{agent: agent}
	if repoRoot != "" {
		agent.repoRoot = repoRoot
		client.terminals = make(map[string]*acpTerminal)
	}
	return client
}

// assertPermissionOutcome calls RequestPermission with the given tool kind and
// asserts the selected outcome matches expectedOptionID. Pass nil for toolKind
// to test the nil-Kind path.
func assertPermissionOutcome(t *testing.T, client *acpClient, toolKind *acp.ToolKind, expectedOptionID acp.PermissionOptionId) {
	t.Helper()
	response, err := client.RequestPermission(context.Background(), acp.RequestPermissionRequest{
		Options: authTestPermissionOptions(),
		ToolCall: acp.ToolCallUpdate{
			Kind: toolKind,
		},
	})
	require.NoError(t, err, "RequestPermission should not error")
	require.NotNil(t, response.Outcome.Selected, "Expected a selected permission option")
	require.Equal(t, expectedOptionID, response.Outcome.Selected.OptionId, "Unexpected permission option")
}

// toolKindPtr returns a pointer to the given ToolKind string.
func toolKindPtr(kind string) *acp.ToolKind {
	k := acp.ToolKind(kind)
	return &k
}

// TestACPAuthPermissionModes tests RequestPermission behavior and mode switching
// This provides context for the H2 authorization bypass fix
func TestACPAuthPermissionModes(t *testing.T) {
	t.Parallel()

	t.Run("RequestPermission: ReadOnlyMode denies destructive operations", func(t *testing.T) {
		client := setupTestClient("plan", "")
		assertPermissionOutcome(t, client, toolKindPtr("edit"), authRejectAlwaysOptionID)
	})

	t.Run("RequestPermission: ReadOnlyMode allows non-destructive operations", func(t *testing.T) {
		client := setupTestClient("plan", "")
		assertPermissionOutcome(t, client, toolKindPtr("read"), authAllowAlwaysOptionID)
	})

	t.Run("RequestPermission: Empty mode defaults to read-only behavior for permissions", func(t *testing.T) {
		client := setupTestClient("", "")
		assertPermissionOutcome(t, client, toolKindPtr("read"), authAllowAlwaysOptionID)
		assertPermissionOutcome(t, client, toolKindPtr("edit"), authRejectAlwaysOptionID)
	})

	t.Run("RequestPermission: AutoApproveMode allows all known operations", func(t *testing.T) {
		client := setupTestClient("auto-approve", "")
		assertPermissionOutcome(t, client, toolKindPtr("edit"), authAllowAlwaysOptionID)
	})

	t.Run("RequestPermission: Unknown tool kinds are always denied", func(t *testing.T) {
		client := setupTestClient("auto-approve", "")
		assertPermissionOutcome(t, client, toolKindPtr("unknown-operation"), authRejectAlwaysOptionID)
	})

	t.Run("RequestPermission: Known destructive operations are denied outside explicit auto-approve mode", func(t *testing.T) {
		client := setupTestClient("custom-mode", "")
		assertPermissionOutcome(t, client, toolKindPtr("edit"), authRejectAlwaysOptionID)
	})

	t.Run("RequestPermission: Nil ToolCall.Kind is denied", func(t *testing.T) {
		client := setupTestClient("auto-approve", "")
		assertPermissionOutcome(t, client, nil, authRejectAlwaysOptionID)
	})

	t.Run("Mode switching: WithAgentic sets correct mode", func(t *testing.T) {
		baseAgent := &ACPAgent{
			agentName:       "test-acp",
			Command:         "test-command",
			ReadOnlyMode:    "plan",
			AutoApproveMode: "auto-approve",
			Mode:            "plan",
			Model:           "test-model",
			Timeout:         30 * time.Second,
		}

		nonAgenticAgent := baseAgent.WithAgentic(false).(*ACPAgent)
		require.Equal(t, "plan", nonAgenticAgent.Mode, "Non-agentic mode should be read-only mode")

		agenticAgent := baseAgent.WithAgentic(true).(*ACPAgent)
		require.Equal(t, "auto-approve", agenticAgent.Mode, "Agentic mode should be auto-approve mode")
	})
}

// TestACPAuthDirectEnforcement tests H2 authorization bypass fix
// Direct authorization enforcement at operation entry points
func TestACPAuthDirectEnforcement(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()

	client := setupTestClient("plan", tempDir)

	t.Run("H2: WriteTextFile authorization in read-only mode", func(t *testing.T) {
		_, err := client.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
			Path:    "test.txt",
			Content: "test content",
		})
		require.Error(t, err, "Expected authorization error in read-only mode")
		require.Contains(t, err.Error(), "write operation not permitted in read-only mode")
	})

	t.Run("H2: CreateTerminal authorization in read-only mode", func(t *testing.T) {
		cmd, args := acpTestEchoCommand("test")
		_, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			Command: cmd,
			Args:    args,
		})
		require.Error(t, err, "Expected authorization error in read-only mode")
		require.Contains(t, err.Error(), "terminal creation not permitted in read-only mode")
	})

	t.Run("H2: Authorization bypass prevention - direct method calls", func(t *testing.T) {
		testCases := []struct {
			name string
			exec func() error
		}{
			{
				name: "WriteTextFile direct call",
				exec: func() error {
					_, err := client.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
						Path:    "malicious.txt",
						Content: "malicious content",
					})
					return err
				},
			},
			{
				name: "CreateTerminal direct call",
				exec: func() error {
					_, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
						Command: "rm",
						Args:    []string{"-rf", "/"},
					})
					return err
				},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				err := tc.exec()
				require.Error(t, err, "Expected authorization error for %s", tc.name)
				require.Contains(t, err.Error(), "not permitted in read-only mode")
			})
		}
	})

	t.Run("H2: Auto-approve mode allows mutating operations", func(t *testing.T) {
		autoApproveClient := setupTestClient("auto-approve", tempDir)

		// Test that WriteTextFile is allowed in auto-approve mode
		tempFile := filepath.Join(tempDir, "auto-approve-test.txt")
		_, err := autoApproveClient.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
			Path:    "auto-approve-test.txt",
			Content: "test content",
		})
		require.NoError(t, err, "Expected success in auto-approve mode")
		// Verify file was actually created
		defer os.Remove(tempFile)
		if _, err := os.Stat(tempFile); os.IsNotExist(err) {
			require.Error(t, err, "File was not created despite successful WriteTextFile call")
		}

		t.Run("WriteTextFile preserves existing executable bit", func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("POSIX permission bits are not consistent on Windows")
			}

			targetPath := filepath.Join(tempDir, "exec-mode.sh")
			if err := os.WriteFile(targetPath, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
				require.NoError(t, err, "Failed to create executable test file: %v", err)
			}
			defer os.Remove(targetPath)
			originalInfo, err := os.Stat(targetPath)
			require.NoError(t, err, "Failed to stat original file: %v", err)
			originalMode := originalInfo.Mode().Perm()

			if _, err := autoApproveClient.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
				Path:    "exec-mode.sh",
				Content: "#!/bin/sh\necho new\n",
			}); err != nil {
				require.NoError(t, err, "WriteTextFile failed: %v", err)
			}

			info, err := os.Stat(targetPath)
			require.NoError(t, err, "Failed to stat updated file: %v", err)
			require.Equal(t, originalMode, info.Mode().Perm(), "Expected mode to be preserved")
		})

		t.Run("WriteTextFile respects existing read-only file permissions", func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("POSIX permission bits are not consistent on Windows")
			}

			targetPath := filepath.Join(tempDir, "read-only.txt")
			if err := os.WriteFile(targetPath, []byte("old"), 0o444); err != nil {
				require.NoError(t, err, "Failed to create read-only test file: %v", err)
			}
			defer func() {
				_ = os.Chmod(targetPath, 0o644)
				_ = os.Remove(targetPath)
			}()

			// Skip this assertion in environments where read-only files remain writable
			// (for example privileged users with DAC override).
			probe, probeErr := os.OpenFile(targetPath, os.O_WRONLY, 0)
			if probeErr == nil {
				_ = probe.Close()
				t.Skip("environment allows writes to read-only files")
			}

			if _, err := autoApproveClient.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
				Path:    "read-only.txt",
				Content: "new",
			}); err == nil {
				require.Error(t, err, "Expected WriteTextFile to fail for read-only existing file")
			}
		})

		// Test that CreateTerminal is allowed in auto-approve mode
		cmd, args := acpTestEchoCommand("test")
		_, err = autoApproveClient.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			Command: cmd,
			Args:    args,
		})
		require.NoError(t, err, "Expected success in auto-approve mode")
	})

	t.Run("H2: Mode switching validation", func(t *testing.T) {
		switchableClient := setupTestClient("plan", tempDir)

		// Should be blocked in read-only mode
		_, err := switchableClient.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
			Path:    "test.txt",
			Content: "test",
		})
		require.Error(t, err, "Expected authorization error in read-only mode")

		// Switch to auto-approve mode
		switchableClient.agent.Mode = "auto-approve"

		// Should now be allowed
		tempFile := filepath.Join(tempDir, "switch-test.txt")
		_, err = switchableClient.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
			Path:    "switch-test.txt",
			Content: "test",
		})
		require.NoError(t, err, "Expected success after switching to auto-approve mode")
		defer os.Remove(tempFile)
	})

	t.Run("H2: Non-read-only custom mode still blocks mutating operations", func(t *testing.T) {
		customModeClient := setupTestClient("custom-mode", tempDir)

		_, err := customModeClient.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
			Path:    "custom-mode.txt",
			Content: "test",
		})
		require.Error(t, err, "Expected write to be blocked outside explicit auto-approve mode")
		require.Contains(t, err.Error(), "auto-approve mode is explicitly enabled")

		cmd, args := acpTestEchoCommand("test")
		_, err = customModeClient.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			Command: cmd,
			Args:    args,
		})
		require.Error(t, err, "Expected terminal creation to be blocked outside explicit auto-approve mode")
		require.Contains(t, err.Error(), "auto-approve mode is explicitly enabled")
	})
}

// TestACPAuthEdgeCases tests security edge cases and validation
func TestACPAuthEdgeCases(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	// On Windows, t.TempDir() may return an 8.3 short path (e.g.
	// RUNNER~1) while filepath.EvalSymlinks resolves to the long
	// form (runneradmin). Normalize so substring checks match.
	if resolved, err := filepath.EvalSymlinks(tempDir); err == nil {
		tempDir = resolved
	}

	client := setupTestClient("plan", tempDir)

	t.Run("Path validation prevents directory traversal", func(t *testing.T) {
		_, err := client.validateAndResolvePath("../../../etc/passwd", false)
		require.Error(t, err, "Expected error for path traversal, got nil")
	})

	t.Run("Path validation prevents symlink traversal", func(t *testing.T) {
		symlinkPath := filepath.Join(tempDir, "symlink")
		targetPath := "/etc/passwd"
		err := os.Symlink(targetPath, symlinkPath)
		if err != nil {
			t.Skip("Could not create symlink for test")
		}
		defer os.Remove(symlinkPath)

		_, err = client.validateAndResolvePath("symlink", false)
		require.Error(t, err, "Expected error for symlink traversal, got nil")
	})

	t.Run("Path validation blocks writes to symlinks escaping repo root", func(t *testing.T) {
		externalDir := t.TempDir()
		externalTarget := filepath.Join(externalDir, "outside.txt")
		if err := os.WriteFile(externalTarget, []byte("outside"), 0o644); err != nil {
			require.NoError(t, err, "Failed to create external target: %v", err)
		}

		symlinkPath := filepath.Join(tempDir, "write-link-outside")
		if err := os.Symlink(externalTarget, symlinkPath); err != nil {
			t.Skip("Could not create symlink for write traversal test")
		}
		defer os.Remove(symlinkPath)

		_, err := client.validateAndResolvePath("write-link-outside", true)
		require.Error(t, err, "Expected error for write symlink traversal, got nil")
	})

	t.Run("Path validation allows writes to symlinks that resolve inside repo root", func(t *testing.T) {
		internalTarget := filepath.Join(tempDir, "inside.txt")
		if err := os.WriteFile(internalTarget, []byte("inside"), 0o644); err != nil {
			require.NoError(t, err, "Failed to create internal target: %v", err)
		}
		defer os.Remove(internalTarget)

		symlinkPath := filepath.Join(tempDir, "write-link-inside")
		if err := os.Symlink(internalTarget, symlinkPath); err != nil {
			t.Skip("Could not create symlink for write validation test")
		}
		defer os.Remove(symlinkPath)

		resolvedPath, err := client.validateAndResolvePath("write-link-inside", true)
		require.NoError(t, err, "Unexpected error for in-repo symlink write target: %v", err)
		require.True(t, strings.HasSuffix(resolvedPath, "inside.txt"), "Expected resolved path to use resolved target path")
	})

	t.Run("Path validation allows valid paths", func(t *testing.T) {
		validFile := filepath.Join(tempDir, "valid.txt")
		err := os.WriteFile(validFile, []byte("test"), 0o644)
		require.NoError(t, err, "Failed to create test file: %v", err)
		defer os.Remove(validFile)

		resolvedPath, err := client.validateAndResolvePath("valid.txt", false)
		require.NoError(t, err, "Unexpected error for valid path: %v", err)
		require.True(t, strings.HasSuffix(resolvedPath, "valid.txt"), "Expected resolved path to end with 'valid.txt'")
		require.Contains(t, resolvedPath, tempDir, "Expected resolved path to contain temp dir")
	})

	t.Run("Numeric parameter validation", func(t *testing.T) {
		_, err := client.ReadTextFile(context.Background(), acp.ReadTextFileRequest{
			Path: "test.txt",
			Line: new(-1),
		})
		require.Error(t, err, "Expected error for negative line number, got nil")

		_, err = client.ReadTextFile(context.Background(), acp.ReadTextFileRequest{
			Path:  "test.txt",
			Limit: new(-1),
		})
		require.Error(t, err, "Expected error for negative limit, got nil")

		_, err = client.ReadTextFile(context.Background(), acp.ReadTextFileRequest{
			Path: "",
		})
		require.Error(t, err, "Expected error for empty path, got nil")

		_, err = client.ReadTextFile(context.Background(), acp.ReadTextFileRequest{
			Path: "test.txt",
			Line: new(2000000),
		})
		require.Error(t, err, "Expected error for excessively large line number, got nil")

		_, err = client.ReadTextFile(context.Background(), acp.ReadTextFileRequest{
			Path:  "test.txt",
			Limit: new(2000000),
		})
		require.Error(t, err, "Expected error for excessively large limit, got nil")
	})

	t.Run("WriteTextFile input validation", func(t *testing.T) {
		validationClient := setupTestClient("auto-approve", "")

		_, err := validationClient.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
			Path:    "",
			Content: "test content",
		})
		require.Error(t, err, "Expected error for empty path, got nil")

		largeContent := string(make([]byte, 11000000)) // 11MB > 10MB limit
		_, err = validationClient.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
			Path:    "test.txt",
			Content: largeContent,
		})
		require.Error(t, err, "Expected error for excessively large content, got nil")
	})

	t.Run("Permission logic defaults to deny", func(t *testing.T) {
		assertPermissionOutcome(t, client, toolKindPtr("unknown"), authRejectAlwaysOptionID)
	})

	t.Run("Read-only mode denies destructive operations", func(t *testing.T) {
		assertPermissionOutcome(t, client, toolKindPtr("edit"), authRejectAlwaysOptionID)
	})

	t.Run("Read-only mode allows non-destructive operations", func(t *testing.T) {
		assertPermissionOutcome(t, client, toolKindPtr("read"), authAllowAlwaysOptionID)
	})
}
