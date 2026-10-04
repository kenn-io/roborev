package goalreview

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"os"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/prompt"
)

var resultSchema = jsontext.Value(`{"type":"object","required":["findings"],"additionalProperties":false,"properties":{"findings":{"type":"array","items":{"type":"object","required":["severity","message","fix","location"],"additionalProperties":false,"properties":{"severity":{"type":"string","enum":["high","medium","low"]},"message":{"type":"string"},"fix":{"type":"string","minLength":1},"location":{"type":"object","additionalProperties":false,"properties":{"file":{"type":"string"},"line":{"type":"integer"},"kata_id":{"type":"string"}}}}}}}}`)

// ValidateAgent admits concrete adapters whose schema path disables tools.
// Name strings and SchemaAgent membership alone do not establish isolation.
func ValidateAgent(a agent.Agent) error {
	switch a.(type) {
	case *agent.PiAgent, *agent.ClaudeAgent:
	default:
		return fmt.Errorf("goal review requires a tool-disabled schema agent (pi or claude-code)")
	}
	if !agent.IsSchemaAgent(a) {
		return fmt.Errorf("goal review agent lacks structured output")
	}
	return nil
}

// ExecutionError identifies failures from the agent invocation, separately from
// malformed frozen evidence or result validation.
type ExecutionError struct{ Err error }

func (e *ExecutionError) Error() string { return e.Err.Error() }
func (e *ExecutionError) Unwrap() error { return e.Err }

func Run(ctx context.Context, a agent.Agent, root string, snapshot Snapshot, out io.Writer) ([]Finding, error) {
	return RunPrepared(ctx, a, root, snapshot, prompt.SnapshotResult{Prompt: BuildPrompt(snapshot)}, out)
}

// RunPrepared evaluates a frozen goal snapshot using the complete prompt
// prepared by the shared prompt builder. Schema agents accept prompt text, so
// file-backed prompts are read in full and passed through each adapter's
// existing prompt transport.
func RunPrepared(ctx context.Context, a agent.Agent, root string, snapshot Snapshot, prepared prompt.SnapshotResult, out io.Writer) ([]Finding, error) {
	if err := ValidateAgent(a); err != nil {
		return nil, err
	}
	return runPrepared(snapshot, prepared, func(reviewPrompt string) (jsontext.Value, error) {
		if claude, ok := a.(*agent.ClaudeAgent); ok {
			return claude.ClassifyGoalWithSchema(ctx, root, snapshot.ID(), reviewPrompt, resultSchema, out)
		}
		return a.(agent.SchemaAgent).ClassifyWithSchema(ctx, root, snapshot.ID(), reviewPrompt, resultSchema, out)
	})
}

// runPrepared keeps prompt materialization separate from agent selection so
// package tests can inject a schema adapter without admitting it in production.
func runPrepared(snapshot Snapshot, prepared prompt.SnapshotResult, classify func(string) (jsontext.Value, error)) ([]Finding, error) {
	reviewPrompt := prepared.Prompt
	if prepared.FilePath != "" {
		content, err := os.ReadFile(prepared.FilePath)
		if err != nil {
			return nil, fmt.Errorf("read complete prepared goal prompt: %w", err)
		}
		reviewPrompt = string(content)
	}
	raw, err := classify(reviewPrompt)
	if err != nil {
		return nil, &ExecutionError{Err: err}
	}
	return ParseResult(string(raw), snapshot)
}
