package agent

import (
	"context"
	"fmt"
	"log"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

func (c *acpClient) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	// Validate session ID
	if err := c.validateSessionID(params.SessionId); err != nil {
		return acp.RequestPermissionResponse{}, err
	}

	// Default to deny for safety - unknown operations should be rejected
	var isDestructive bool
	var isKnownKind bool

	if params.ToolCall.Kind != nil {
		toolKind := string(*params.ToolCall.Kind)

		// Define destructive operations that modify state
		// Based on ACP protocol ToolKind constants
		destructiveKinds := map[string]bool{
			"edit":    true, // Modifying files or content
			"delete":  true, // Removing files or data
			"move":    true, // Moving or renaming files
			"execute": true, // Executing commands (potentially destructive)
		}

		// Non-destructive operations
		nonDestructiveKinds := map[string]bool{
			"read":   true, // Reading files or data
			"search": true, // Searching for files or data
			"think":  true, // Internal reasoning
			"fetch":  true, // Fetching data
		}

		// Explicitly validate tool kind
		if destructiveKinds[toolKind] {
			isDestructive = true
			isKnownKind = true
		} else if nonDestructiveKinds[toolKind] {
			isDestructive = false
			isKnownKind = true
		} else {
			// Unknown tool kind - explicitly deny
			return acp.RequestPermissionResponse{
				Outcome: selectPermissionOutcome(params.Options, false),
			}, nil
		}
	} else {
		// ToolCall.Kind is nil - invalid request
		return acp.RequestPermissionResponse{
			Outcome: selectPermissionOutcome(params.Options, false),
		}, nil
	}

	// Apply permission logic based on effective permission mode.
	// When session mode negotiation is disabled (Mode == ""), keep
	// permission behavior in read-only mode by default.
	effectiveMode := c.agent.effectivePermissionMode()

	// In read-only mode, deny all destructive operations.
	if effectiveMode == c.agent.ReadOnlyMode {
		if isDestructive {
			return acp.RequestPermissionResponse{
				Outcome: selectPermissionOutcome(params.Options, false),
			}, nil
		}
		// Allow non-destructive operations in read-only mode
		return acp.RequestPermissionResponse{
			Outcome: selectPermissionOutcome(params.Options, true),
		}, nil
	}

	// Only explicit auto-approve mode allows known operations.
	if c.agent.mutatingOperationsAllowed() && isKnownKind {
		return acp.RequestPermissionResponse{
			Outcome: selectPermissionOutcome(params.Options, true),
		}, nil
	}

	// This should not be reached due to earlier checks, but default to deny
	return acp.RequestPermissionResponse{
		Outcome: selectPermissionOutcome(params.Options, false),
	}, nil
}

func (c *acpClient) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	// Validate against the established session. Only NewSession may set
	// c.sessionID; an incoming notification must never bootstrap it, because a
	// stale or spoofed early notification could otherwise bind the client to
	// the wrong session and cause later legitimate updates to be rejected.
	if err := c.validateSessionID(params.SessionId); err != nil {
		log.Printf("ACP session update rejected: %v", err)
		return nil
	}

	// Handle streaming updates from the agent
	if params.Update.AgentMessageChunk != nil {
		if params.Update.AgentMessageChunk.Content.Text != nil {
			text := params.Update.AgentMessageChunk.Content.Text.Text
			c.resultMutex.Lock()
			if c.output != nil {
				if _, err := c.output.Write([]byte(text)); err != nil {
					c.resultMutex.Unlock()
					return err
				}
			}
			c.result.WriteString(text)
			c.resultMutex.Unlock()
		}
	}
	return nil
}

// validateAndResolvePath validates that a file path is within the repository root
// and resolves it to an absolute path. This prevents directory traversal attacks
// including symlink traversal.
// For write operations (forWrite=true), only validates parent directory since the file may not exist yet.

func selectPermissionOptionID(options []acp.PermissionOption, preferredKinds ...acp.PermissionOptionKind) (acp.PermissionOptionId, bool) {
	for _, preferredKind := range preferredKinds {
		for _, option := range options {
			if option.Kind == preferredKind {
				return option.OptionId, true
			}
		}
	}
	return "", false
}

func selectPermissionOutcome(options []acp.PermissionOption, allow bool) acp.RequestPermissionOutcome {
	if allow {
		if optionID, ok := selectPermissionOptionID(options, acp.PermissionOptionKindAllowAlways, acp.PermissionOptionKindAllowOnce); ok {
			return acp.NewRequestPermissionOutcomeSelected(optionID)
		}
	} else {
		if optionID, ok := selectPermissionOptionID(options, acp.PermissionOptionKindRejectAlways, acp.PermissionOptionKindRejectOnce); ok {
			return acp.NewRequestPermissionOutcomeSelected(optionID)
		}
	}

	// Safe fallback when the request does not offer an expected option kind.
	return acp.NewRequestPermissionOutcomeCancelled()
}

// configuredModeIsAvailable checks if the configured mode is available in the list of available modes
// from the ACP agent session response.
func configuredModeIsAvailable(configuredMode string, availableModes []acp.SessionMode) bool {
	for _, mode := range availableModes {
		if string(mode.Id) == configuredMode {
			return true
		}
	}
	return false
}

func validateConfiguredMode(configuredMode string, modes *acp.SessionModeState) error {
	if configuredMode == "" {
		return nil
	}
	if modes == nil {
		return fmt.Errorf("agent does not support session modes (configured mode: %s)", configuredMode)
	}
	if !configuredModeIsAvailable(configuredMode, modes.AvailableModes) {
		return fmt.Errorf("mode %s is not available", configuredMode)
	}
	return nil
}

func validateConfiguredModel(configuredModel string, configOptions []acp.SessionConfigOption) (acp.SessionConfigId, error) {
	if configuredModel == "" {
		return "", nil
	}

	modelOption, ok := findModelConfigOption(configOptions)
	if !ok {
		return "", fmt.Errorf("agent does not support session models (configured model: %s)", configuredModel)
	}
	if !configuredModelIsAvailable(configuredModel, modelOption.Options) {
		return "", fmt.Errorf("model %s is not available", configuredModel)
	}
	return modelOption.Id, nil
}

func findModelConfigOption(configOptions []acp.SessionConfigOption) (acp.SessionConfigOptionSelect, bool) {
	for _, option := range configOptions {
		if option.Select == nil {
			continue
		}
		if option.Select.Category != nil {
			if *option.Select.Category == acp.SessionConfigOptionCategoryModel {
				return *option.Select, true
			}
			continue
		}
		if strings.EqualFold(string(option.Select.Id), "model") {
			return *option.Select, true
		}
	}
	return acp.SessionConfigOptionSelect{}, false
}

// configuredModelIsAvailable checks if the configured model value is available
// in the ACP model config option advertised by the agent session response.
func configuredModelIsAvailable(modelID string, options acp.SessionConfigSelectOptions) bool {
	configuredValue := acp.SessionConfigValueId(modelID)
	if options.Ungrouped != nil {
		for _, option := range *options.Ungrouped {
			if option.Value == configuredValue {
				return true
			}
		}
	}
	if options.Grouped != nil {
		for _, group := range *options.Grouped {
			for _, option := range group.Options {
				if option.Value == configuredValue {
					return true
				}
			}
		}
	}
	return false
}

func (a *ACPAgent) mutatingOperationsAllowed() bool {
	return a.AutoApproveMode != "" && a.effectivePermissionMode() == a.AutoApproveMode
}

func (a *ACPAgent) effectivePermissionMode() string {
	if strings.TrimSpace(a.Mode) != "" {
		return a.Mode
	}
	if a.Agentic && strings.TrimSpace(a.AutoApproveMode) != "" {
		return a.AutoApproveMode
	}
	return a.ReadOnlyMode
}
