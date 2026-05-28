package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"
)

type acpTerminal struct {
	id              string
	cmd             *exec.Cmd
	output          *bytes.Buffer
	outputWriter    *boundedWriter
	context         context.Context
	cancel          context.CancelFunc
	outputByteLimit int
	truncated       bool
	done            chan struct{} // Channel to signal command completion
	stateMu         sync.RWMutex
	exitStatus      *acp.TerminalExitStatus
}

// threadSafeWriter is a thread-safe io.Writer that protects a bytes.Buffer
type threadSafeWriter struct {
	buf   *bytes.Buffer
	mutex *sync.Mutex
}

func (w *threadSafeWriter) Write(p []byte) (n int, err error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.buf.Write(p)
}

// boundedWriter wraps a threadSafeWriter and enforces output size limits
// while maintaining UTF-8 character boundaries
type boundedWriter struct {
	writer    *threadSafeWriter
	maxSize   int
	truncated bool
}

func (bw *boundedWriter) Write(p []byte) (n int, err error) {
	bw.writer.mutex.Lock()
	defer bw.writer.mutex.Unlock()

	if bw.maxSize <= 0 {
		if len(p) > 0 {
			bw.truncated = true
		}
		return len(p), nil
	}

	if _, err := bw.writer.buf.Write(p); err != nil {
		return 0, err
	}

	bw.trimToMaxSizeLocked()
	return len(p), nil
}

// acpClient implements the acp.Client interface to handle agent responses
type acpClient struct {
	agent               *ACPAgent
	output              io.Writer
	result              *bytes.Buffer
	resultMutex         sync.Mutex
	sessionID           string
	repoRoot            string
	terminals           map[string]*acpTerminal // Active terminals by ID
	terminalsMutex      sync.Mutex              // Mutex for concurrent access to terminals map
	nextTerminalID      int                     // Counter for generating terminal IDs
	nextTerminalIDMutex sync.Mutex              // Mutex for concurrent access to terminal ID counter
}

// validateSessionID validates that the session ID in the request matches the agent's session ID
func (c *acpClient) validateSessionID(sessionID acp.SessionId) error {
	expectedSessionID := c.sessionID
	if expectedSessionID == "" && c.agent != nil {
		expectedSessionID = c.agent.SessionID
	}
	if expectedSessionID == "" {
		if sessionID != "" {
			return fmt.Errorf("session ID mismatch: no active session, got %s", sessionID)
		}
		return nil
	}
	if expectedSessionID != "" && string(sessionID) != expectedSessionID {
		return fmt.Errorf("session ID mismatch: expected %s, got %s", expectedSessionID, sessionID)
	}
	return nil
}

// getTerminal retrieves a terminal by ID with proper locking.
// The returned terminal pointer remains valid even if it is later removed from the map.
func (c *acpClient) getTerminal(terminalID string) (*acpTerminal, bool) {
	c.terminalsMutex.Lock()
	defer c.terminalsMutex.Unlock()
	terminal, exists := c.terminals[terminalID]
	return terminal, exists
}

func (c *acpClient) effectiveRepoRoot() string {
	if c.repoRoot != "" {
		return c.repoRoot
	}
	if c.agent != nil {
		return c.agent.repoRoot
	}
	return ""
}

// addTerminal adds a terminal to the map with proper locking
func (c *acpClient) addTerminal(terminal *acpTerminal) {
	c.terminalsMutex.Lock()
	defer c.terminalsMutex.Unlock()
	c.terminals[terminal.id] = terminal
}

// removeTerminal removes a terminal from the map with proper locking
func (c *acpClient) removeTerminal(terminalID string) {
	c.terminalsMutex.Lock()
	defer c.terminalsMutex.Unlock()
	delete(c.terminals, terminalID)
}

// generateTerminalID generates a unique terminal ID with proper locking
func (c *acpClient) generateTerminalID() string {
	c.nextTerminalIDMutex.Lock()
	defer c.nextTerminalIDMutex.Unlock()
	id := fmt.Sprintf("term-%d", c.nextTerminalID)
	c.nextTerminalID++
	return id
}

// truncateOutput ensures output stays within byte limits, truncating from the beginning
// while maintaining character boundaries as required by ACP spec
func truncateOutput(output *bytes.Buffer, limit int, outputMutex *sync.Mutex) (string, bool) {
	// Validate limit to prevent panics
	if limit < 0 {
		limit = 0
	}

	outputMutex.Lock()
	defer outputMutex.Unlock()

	currentOutput := output.Bytes()

	if len(currentOutput) <= limit {
		return string(currentOutput), false
	}

	start := len(currentOutput) - limit
	for start < len(currentOutput) && !utf8.RuneStart(currentOutput[start]) {
		start++
	}

	truncatedOutput := append([]byte(nil), currentOutput[start:]...)

	// Clear the buffer and write the truncated content
	output.Reset()
	_, _ = output.Write(truncatedOutput)

	return string(truncatedOutput), true
}

// getTerminalExitStatus extracts the exit status from a command's ProcessState
// Returns nil if the command hasn't exited yet
// Follows ACP spec: exitCode is nil when process terminated by signal
func getTerminalExitStatus(processState *os.ProcessState) *acp.TerminalExitStatus {
	if processState == nil {
		return nil
	}

	exitCodeValue := processState.ExitCode()
	exitCode := &exitCodeValue
	var signal *string

	// Check if process was terminated by a signal
	if exited := processState.Sys(); exited != nil {
		// For Unix systems, check for signal termination
		if ws, ok := exited.(syscall.WaitStatus); ok {
			if ws.Signaled() {
				signalName := ws.Signal().String()
				signal = &signalName
				// Per ACP spec, exitCode should be nil when terminated by signal
				exitCode = nil
			}
		}
	}

	return &acp.TerminalExitStatus{
		ExitCode: exitCode,
		Signal:   signal,
	}
}

func cloneTerminalExitStatus(status *acp.TerminalExitStatus) *acp.TerminalExitStatus {
	if status == nil {
		return nil
	}

	var exitCode *int
	if status.ExitCode != nil {
		value := *status.ExitCode
		exitCode = &value
	}

	var signal *string
	if status.Signal != nil {
		value := *status.Signal
		signal = &value
	}

	return &acp.TerminalExitStatus{
		ExitCode: exitCode,
		Signal:   signal,
	}
}

func (t *acpTerminal) setExitStatus(status *acp.TerminalExitStatus) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.exitStatus = cloneTerminalExitStatus(status)
}

func (t *acpTerminal) getExitStatus() *acp.TerminalExitStatus {
	t.stateMu.RLock()
	defer t.stateMu.RUnlock()
	return cloneTerminalExitStatus(t.exitStatus)
}

func (c *acpClient) resultString() string {
	c.resultMutex.Lock()
	defer c.resultMutex.Unlock()
	return c.result.String()
}

func (bw *boundedWriter) isTruncated() bool {
	bw.writer.mutex.Lock()
	defer bw.writer.mutex.Unlock()
	return bw.truncated
}

func (bw *boundedWriter) trimToMaxSizeLocked() {
	if bw.writer.buf.Len() <= bw.maxSize {
		return
	}

	bw.truncated = true

	bytesView := bw.writer.buf.Bytes()
	start := len(bytesView) - bw.maxSize
	for start < len(bytesView) && !utf8.RuneStart(bytesView[start]) {
		start++
	}

	tail := append([]byte(nil), bytesView[start:]...)
	bw.writer.buf.Reset()
	_, _ = bw.writer.buf.Write(tail)
}

func (c *acpClient) CreateTerminal(ctx context.Context, params acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	// Validate session ID
	if err := c.validateSessionID(params.SessionId); err != nil {
		return acp.CreateTerminalResponse{}, err
	}

	// Enforce authorization at point of operation.
	if !c.agent.mutatingOperationsAllowed() {
		if c.agent.effectivePermissionMode() == c.agent.ReadOnlyMode {
			return acp.CreateTerminalResponse{}, fmt.Errorf("terminal creation not permitted in read-only mode")
		}
		return acp.CreateTerminalResponse{}, fmt.Errorf("terminal creation not permitted unless auto-approve mode is explicitly enabled")
	}
	repoRoot := c.effectiveRepoRoot()

	// Set working directory (default to repository root if available).
	cwd := repoRoot
	if strings.TrimSpace(cwd) == "" {
		cwd = "."
	}
	if params.Cwd != nil {
		cwd = strings.TrimSpace(*params.Cwd)
		if cwd == "" {
			cwd = repoRoot
			if cwd == "" {
				cwd = "."
			}
		} else if !filepath.IsAbs(cwd) {
			base := repoRoot
			if base == "" {
				base = "."
			}
			cwd = filepath.Join(base, cwd)
		}
	}
	cwd = filepath.Clean(cwd)

	if repoRoot != "" {
		repoRootAbs, err := filepath.Abs(repoRoot)
		if err != nil {
			return acp.CreateTerminalResponse{}, fmt.Errorf("failed to resolve repository root path: %w", err)
		}

		resolvedRepoRoot, err := filepath.EvalSymlinks(repoRootAbs)
		if err != nil {
			return acp.CreateTerminalResponse{}, fmt.Errorf("failed to resolve repository root symlinks: %w", err)
		}
		resolvedRepoRoot = filepath.Clean(resolvedRepoRoot)

		cwdAbs := cwd
		if !filepath.IsAbs(cwdAbs) {
			cwdAbs, err = filepath.Abs(cwdAbs)
			if err != nil {
				return acp.CreateTerminalResponse{}, fmt.Errorf("failed to resolve terminal cwd path %q: %w", cwd, err)
			}
		}

		resolvedCwd, err := filepath.EvalSymlinks(cwdAbs)
		if err != nil {
			return acp.CreateTerminalResponse{}, fmt.Errorf("failed to resolve terminal cwd symlinks for path %q: %w", cwd, err)
		}
		resolvedCwd = filepath.Clean(resolvedCwd)

		if !pathWithinRoot(resolvedCwd, resolvedRepoRoot) {
			return acp.CreateTerminalResponse{}, fmt.Errorf("%w: terminal cwd %s (resolved to %s) is outside repository root %s", ErrPathTraversal, cwd, resolvedCwd, repoRoot)
		}

		cwd = resolvedCwd
	}

	// Terminal lifetime should not be tied to a single CreateTerminal RPC context.
	terminalCtx, cancelFunc := context.WithCancel(context.Background())

	// Build the command with the terminal-specific context
	cmd := exec.CommandContext(terminalCtx, params.Command, params.Args...)
	cmd.Dir = cwd

	// Set environment variables if specified
	if len(params.Env) > 0 {
		env := os.Environ()
		for _, envVar := range params.Env {
			env = append(env, fmt.Sprintf("%s=%s", envVar.Name, envVar.Value))
		}
		cmd.Env = env
	}

	// Create output buffer with mutex for thread safety
	output := &bytes.Buffer{}
	outputMutex := &sync.Mutex{}

	// Create thread-safe writers for stdout and stderr
	threadSafeOutput := &threadSafeWriter{
		buf:   output,
		mutex: outputMutex,
	}

	// Create bounded writer to enforce output limits
	outputLimit := 1024 * 1024 // Default 1MB limit
	if params.OutputByteLimit != nil {
		outputLimit = max(0, *params.OutputByteLimit) // Clamp negative values to 0
	}

	boundedOutput := &boundedWriter{
		writer:    threadSafeOutput,
		maxSize:   outputLimit,
		truncated: false,
	}

	// Set up output capture with bounded writers
	cmd.Stdout = boundedOutput
	cmd.Stderr = boundedOutput

	// Generate terminal ID first (needed for goroutine)
	terminalID := c.generateTerminalID()

	// Create done channel for command completion signaling
	doneChan := make(chan struct{})

	// Start the command
	if err := cmd.Start(); err != nil {
		// Clean up the context if command fails to start
		cancelFunc()
		close(doneChan)
		return acp.CreateTerminalResponse{}, fmt.Errorf("failed to start terminal command: %w", err)
	}

	// Register terminal before starting cleanup goroutine to prevent race conditions
	terminal := &acpTerminal{
		id:              terminalID,
		cmd:             cmd,
		output:          output,
		outputWriter:    boundedOutput,
		context:         terminalCtx,
		cancel:          cancelFunc,
		outputByteLimit: outputLimit,
		truncated:       false,
		done:            doneChan,
	}
	c.addTerminal(terminal)

	// Start a goroutine to wait for the command to finish.
	// Terminal entries stay available until explicit ReleaseTerminal.
	go func() {
		_ = cmd.Wait()
		terminal.setExitStatus(getTerminalExitStatus(cmd.ProcessState))
		// Signal that the command has completed.
		close(doneChan)
	}()

	return acp.CreateTerminalResponse{
		TerminalId: terminalID,
	}, nil
}

func (c *acpClient) KillTerminal(ctx context.Context, params acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	// Validate session ID
	if err := c.validateSessionID(params.SessionId); err != nil {
		return acp.KillTerminalResponse{}, err
	}

	// Find and cancel the terminal
	terminal, exists := c.getTerminal(params.TerminalId)

	if !exists {
		return acp.KillTerminalResponse{}, fmt.Errorf("terminal %s not found", params.TerminalId)
	}

	// Use context cancellation for graceful termination
	terminal.cancel()

	return acp.KillTerminalResponse{}, nil
}

func (c *acpClient) TerminalOutput(ctx context.Context, params acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	// Validate session ID
	if err := c.validateSessionID(params.SessionId); err != nil {
		return acp.TerminalOutputResponse{}, err
	}

	// Find the terminal and get output
	terminal, exists := c.getTerminal(params.TerminalId)

	if !exists {
		return acp.TerminalOutputResponse{}, fmt.Errorf("terminal %s not found", params.TerminalId)
	}

	// Apply output truncation if needed
	output, truncated := truncateOutput(terminal.output, terminal.outputByteLimit, terminal.outputWriter.writer.mutex)
	truncated = truncated || terminal.outputWriter.isTruncated()

	// Return exit status only once command completion is fully observed.
	var exitStatus *acp.TerminalExitStatus
	select {
	case <-terminal.done:
		exitStatus = terminal.getExitStatus()
	default:
		exitStatus = nil
	}

	return acp.TerminalOutputResponse{
		Output:     output,
		Truncated:  truncated,
		ExitStatus: exitStatus,
	}, nil
}

func (c *acpClient) ReleaseTerminal(ctx context.Context, params acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	// Validate session ID
	if err := c.validateSessionID(params.SessionId); err != nil {
		return acp.ReleaseTerminalResponse{}, err
	}

	// Get the terminal to access its cancel function
	terminal, exists := c.getTerminal(params.TerminalId)

	if !exists {
		return acp.ReleaseTerminalResponse{}, fmt.Errorf("terminal %s not found", params.TerminalId)
	}

	// Cancel the context to terminate the command gracefully
	terminal.cancel()
	c.removeTerminal(params.TerminalId)

	return acp.ReleaseTerminalResponse{}, nil
}

func (c *acpClient) WaitForTerminalExit(ctx context.Context, params acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	// Validate session ID
	if err := c.validateSessionID(params.SessionId); err != nil {
		return acp.WaitForTerminalExitResponse{}, err
	}

	// Find the terminal and get reference
	terminal, exists := c.getTerminal(params.TerminalId)

	if !exists {
		return acp.WaitForTerminalExitResponse{},
			fmt.Errorf("terminal %s not found", params.TerminalId)
	}

	// Wait for the command to finish
	select {
	case <-terminal.done:
		// Command has finished
		exitStatus := terminal.getExitStatus()
		if exitStatus == nil {
			// Command hasn't exited yet (shouldn't happen since we waited for done channel)
			return acp.WaitForTerminalExitResponse{
				ExitCode: nil,
				Signal:   nil,
			}, nil
		}

		return acp.WaitForTerminalExitResponse{
			ExitCode: exitStatus.ExitCode,
			Signal:   exitStatus.Signal,
		}, nil
	case <-ctx.Done():
		// Context was canceled
		return acp.WaitForTerminalExitResponse{}, fmt.Errorf("wait interrupted: %w", ctx.Err())
	}
}
