package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/version"
)

// Security error for path traversal attempts
var ErrPathTraversal = errors.New("path traversal attempt detected")

const (
	defaultACPName            = "acp"
	defaultACPCommand         = "acp-agent"
	defaultACPReadOnlyMode    = "plan"
	defaultACPAutoApproveMode = "auto-approve"
	defaultACPTimeoutSeconds  = 600
	maxACPTextFileBytes       = 10_000_000
)

// ACPAgent runs code reviews using the Agent Client Protocol via acp-go-sdk
type ACPAgent struct {
	agentName       string   // Agent name (from configuration)
	Command         string   // ACP agent command (configured via TOML)
	Args            []string // Additional arguments for the agent
	Model           string   // Model to use
	Mode            string   // Mode to use
	ReadOnlyMode    string
	AutoApproveMode string
	Reasoning       ReasoningLevel // Reasoning level
	Agentic         bool           // Agentic mode
	Timeout         time.Duration  // Command timeout
	SessionID       string         // Current ACP session ID
	repoRoot        string         // Repository root for path validation
}

func NewACPAgent(command string) *ACPAgent {
	if command == "" {
		command = defaultACPCommand
	}

	return &ACPAgent{
		agentName:       defaultACPName,
		Command:         command,
		Args:            []string{},
		Model:           "",
		Mode:            defaultACPReadOnlyMode,
		ReadOnlyMode:    defaultACPReadOnlyMode,
		AutoApproveMode: defaultACPAutoApproveMode,
		Timeout:         time.Duration(defaultACPTimeoutSeconds) * time.Second,
		Reasoning:       ReasoningStandard,
		SessionID:       "", // Initialize with empty session ID
	}
}

func NewACPAgentFromConfig(config *config.ACPAgentConfig) *ACPAgent {
	if config == nil {
		return NewACPAgent("")
	}

	agent := NewACPAgent(config.Command)
	if agentName := strings.TrimSpace(config.Name); agentName != "" {
		agent.agentName = agentName
	}
	if len(config.Args) > 0 {
		agent.Args = append([]string(nil), config.Args...)
	}
	if model := strings.TrimSpace(config.Model); model != "" {
		agent.Model = model
	}
	if readOnlyMode := strings.TrimSpace(config.ReadOnlyMode); readOnlyMode != "" {
		agent.ReadOnlyMode = readOnlyMode
	}
	if autoApproveMode := strings.TrimSpace(config.AutoApproveMode); autoApproveMode != "" {
		agent.AutoApproveMode = autoApproveMode
	}
	if config.DisableModeNegotiation {
		agent.Mode = ""
	} else if mode := strings.TrimSpace(config.Mode); mode != "" {
		agent.Mode = mode
	} else {
		agent.Mode = agent.ReadOnlyMode
	}

	timeout := time.Duration(defaultACPTimeoutSeconds) * time.Second
	if config.Timeout > 0 {
		timeout = time.Duration(config.Timeout) * time.Second
	}
	agent.Timeout = timeout

	return agent
}

func (a *ACPAgent) Name() string {
	return a.agentName
}

func (a *ACPAgent) CommandName() string {
	return a.Command
}

func (a *ACPAgent) CommandLine() string {
	return a.Command + " " + strings.Join(a.Args, " ")
}

func (a *ACPAgent) WithReasoning(level ReasoningLevel) Agent {
	return &ACPAgent{
		agentName:       a.agentName,
		Command:         a.Command,
		Args:            a.Args,
		Model:           a.Model,
		ReadOnlyMode:    a.ReadOnlyMode,
		AutoApproveMode: a.AutoApproveMode,
		Mode:            a.Mode,
		Reasoning:       level,     // Use the provided level parameter
		Agentic:         a.Agentic, // Preserve Agentic field
		Timeout:         a.Timeout,
		SessionID:       a.SessionID, // Preserve SessionID
	}
}

func (a *ACPAgent) WithAgentic(agentic bool) Agent {
	// Set the appropriate mode based on agentic flag
	mode := a.ReadOnlyMode
	if agentic && a.AutoApproveMode != "" {
		mode = a.AutoApproveMode
	}
	if strings.TrimSpace(a.Mode) == "" {
		mode = ""
	}

	return &ACPAgent{
		agentName:       a.agentName,
		Command:         a.Command,
		Args:            a.Args,
		Model:           a.Model,
		ReadOnlyMode:    a.ReadOnlyMode,
		AutoApproveMode: a.AutoApproveMode,
		Mode:            mode,
		Reasoning:       a.Reasoning,
		Agentic:         agentic,
		Timeout:         a.Timeout,
		SessionID:       a.SessionID, // Preserve SessionID
	}
}

func (a *ACPAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}

	return &ACPAgent{
		agentName:       a.agentName,
		Command:         a.Command,
		Args:            a.Args,
		Model:           model,
		ReadOnlyMode:    a.ReadOnlyMode,
		AutoApproveMode: a.AutoApproveMode,
		Mode:            a.Mode,
		Reasoning:       a.Reasoning,
		Agentic:         a.Agentic, // Preserve Agentic field
		Timeout:         a.Timeout,
		SessionID:       a.SessionID, // Preserve SessionID
	}
}

// Review implements the main review functionality using ACP SDK
func (a *ACPAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	reviewPrompt := fmt.Sprintf("Review the code changes in commit %s.\n\nRepository: %s\n\nPrompt: %s",
		commitSHA, repoPath, prompt)
	return a.runPrompt(ctx, repoPath, reviewPrompt, output, true)
}

// Synthesize combines supplied review outputs without wrapping the prompt as a
// code review or advertising repository capabilities.
func (a *ACPAgent) Synthesize(ctx context.Context, prompt string, output io.Writer) (string, error) {
	return a.runPrompt(ctx, "", prompt, output, false)
}

func (a *ACPAgent) runPrompt(
	ctx context.Context, repoPath, prompt string, output io.Writer, exposeRepo bool,
) (string, error) {
	// Set timeout context
	var cancel context.CancelFunc
	var err error
	ctx, cancel = context.WithTimeout(ctx, a.Timeout)
	defer cancel()

	// Build the command with arguments
	cmd := exec.CommandContext(ctx, a.Command, a.Args...)

	// Set up stdio pipes for communication with the agent
	var stdinPipe io.WriteCloser
	var stdoutPipe io.ReadCloser
	var pipesCleanup func() error

	// Initialize pipes with proper cleanup
	pipeInit := func() error {
		var err error
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("failed to create stdin pipe: %w", err)
		}
		stdoutPipe, err = cmd.StdoutPipe()
		if err != nil {
			_ = stdinPipe.Close()
			return fmt.Errorf("failed to create stdout pipe: %w", err)
		}

		// Set up cleanup function that will be called in reverse order
		pipesCleanup = func() error {
			var pipeErrors []error
			if closeErr := stdoutPipe.Close(); closeErr != nil {
				pipeErrors = append(pipeErrors, closeErr)
			}
			if closeErr := stdinPipe.Close(); closeErr != nil {
				pipeErrors = append(pipeErrors, closeErr)
			}
			if len(pipeErrors) > 0 {
				return fmt.Errorf("pipe cleanup errors: %v", pipeErrors)
			}
			return nil
		}
		return nil
	}

	if err := pipeInit(); err != nil {
		return "", err
	}

	// Start the agent process
	if err := cmd.Start(); err != nil {
		_ = pipesCleanup()
		return "", fmt.Errorf("failed to start ACP agent: %w", err)
	}

	repoRoot := ""
	cwd := os.TempDir()
	clientCapabilities := acp.ClientCapabilities{}
	if exposeRepo {
		repoRoot = repoPath
		cwd = repoPath
		clientCapabilities = acp.ClientCapabilities{
			Fs: acp.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
			Terminal: true,
		}
	}

	// Defer cleanup in proper order: terminals -> pipes -> process
	// Create a client that handles agent responses
	client := &acpClient{
		agent:          a,
		output:         output,
		result:         &bytes.Buffer{},
		sessionID:      "",
		repoRoot:       repoRoot,
		terminals:      make(map[string]*acpTerminal),
		nextTerminalID: 1,
	}

	// Deferred cleanup to ensure no orphaned terminal processes
	defer func() {
		// Cancel all active terminals first
		client.terminalsMutex.Lock()
		for _, terminal := range client.terminals {
			terminal.cancel()
		}
		client.terminalsMutex.Unlock()

		// Then clean up pipes
		if pipesCleanup != nil {
			_ = pipesCleanup()
		}

		// Finally clean up process resources
		if cmd.Process != nil {
			if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}
	}()

	// Create the ACP connection
	conn := acp.NewClientSideConnection(client, stdinPipe, stdoutPipe)

	_, err = conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "roborev",
			Version: version.Version,
		},
		ClientCapabilities: clientCapabilities,
	})
	if err != nil {
		// Check process state to provide better error context
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return "", fmt.Errorf("failed to initialize ACP connection (agent exited with code %d): %w",
				cmd.ProcessState.ExitCode(), err)
		}
		return "", fmt.Errorf("failed to initialize ACP connection: %w", err)
	}

	// Create a new session
	sessionResp, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}

	// Store the session ID for request-scoped validation.
	client.sessionID = string(sessionResp.SessionId)

	if a.Mode != "" {
		if err := validateConfiguredMode(a.Mode, sessionResp.Modes); err != nil {
			return "", err
		}

		_, err = conn.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: sessionResp.SessionId, ModeId: acp.SessionModeId(a.Mode)})
		if err != nil {
			return "", fmt.Errorf("failed to set session mode: %w", err)
		}
	}

	if a.Model != "" {
		modelConfigID, err := validateConfiguredModel(a.Model, sessionResp.ConfigOptions)
		if err != nil {
			return "", err
		}

		_, err = conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: sessionResp.SessionId,
			ConfigId:  modelConfigID,
			Value:     acp.SessionConfigValueId(a.Model),
		}})
		if err != nil {
			return "", fmt.Errorf("failed to set session model: %w", err)
		}
	}

	// Send the prompt request
	promptRequest := acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt: []acp.ContentBlock{
			acp.TextBlock(prompt),
		},
	}

	promptResponse, err := conn.Prompt(ctx, promptRequest)
	if err != nil {
		return "", fmt.Errorf("failed to send prompt: %w", err)
	}

	// Wait for the agent to finish processing
	if promptResponse.StopReason != acp.StopReasonEndTurn {
		return "", fmt.Errorf("agent did not complete processing: %s", promptResponse.StopReason)
	}

	return client.resultString(), nil
}
