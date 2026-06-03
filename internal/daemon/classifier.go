package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/review/autotype"
)

// classifyReasonMaxLen caps the classifier reason at storage time. Even
// with file-tool access disabled, a tightly bounded reason field limits
// how much an unexpected exfiltration vector could leak.
const classifyReasonMaxLen = 200

// classifySchema is embedded in every classify call.
var classifySchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["design_review", "reason"],
  "properties": {
    "design_review": {"type": "boolean"},
    "reason":        {"type": "string"}
  }
}`)

// classifierAdapter bridges autotype.Classifier to a SchemaAgent.
type classifierAdapter struct {
	agent    agent.SchemaAgent
	maxBytes int
	output   io.Writer
}

func newClassifierAdapter(a agent.SchemaAgent, maxBytes int, output io.Writer) *classifierAdapter {
	if maxBytes <= 0 {
		maxBytes = 20 * 1024
	}
	return &classifierAdapter{agent: a, maxBytes: maxBytes, output: output}
}

type classifyResult struct {
	DesignReview bool   `json:"design_review"`
	Reason       string `json:"reason"`
}

// Decide implements autotype.Classifier.
func (c *classifierAdapter) Decide(ctx context.Context, in autotype.Input) (bool, string, error) {
	subject, body := splitMessage(in.Message)
	p, err := prompt.BuildClassifyPrompt(prompt.ClassifyInput{
		Subject:  subject,
		Body:     body,
		DiffStat: deriveStat(in.Diff, in.ChangedFiles),
		Diff:     in.Diff,
		MaxBytes: c.maxBytes,
	})
	if err != nil {
		return false, "", fmt.Errorf("build prompt: %w", err)
	}

	logOutput := c.output
	if logOutput == nil {
		logOutput = io.Discard
	}
	raw, err := c.agent.ClassifyWithSchema(ctx, in.RepoPath, in.GitRef, p, classifySchema, logOutput)
	if err != nil {
		return false, "", fmt.Errorf("classifier agent: %w", err)
	}

	var out classifyResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, "", fmt.Errorf("invalid classifier output: %w (%q)", err, string(raw))
	}
	return out.DesignReview, sanitizeClassifierReason(out.Reason), nil
}

// sanitizeClassifierReason caps length and strips control characters from
// the reason field. The classifier ingests untrusted commit text and the
// result flows into PR comments and TUI output; treat it as untrusted
// regardless of the model's compliance with the schema.
func sanitizeClassifierReason(reason string) string {
	var b strings.Builder
	b.Grow(len(reason))
	for _, r := range reason {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
			// Drop other control characters entirely.
		default:
			b.WriteRune(r)
		}
	}
	cleaned := strings.TrimSpace(b.String())
	if utf8RuneCount(cleaned) > classifyReasonMaxLen {
		cleaned = truncateRunes(cleaned, classifyReasonMaxLen)
	}
	return cleaned
}

func utf8RuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// splitMessage returns (subject, body) from a commit message.
func splitMessage(msg string) (string, string) {
	for i, c := range msg {
		if c == '\n' {
			rest := msg[i+1:]
			for len(rest) > 0 && (rest[0] == '\n' || rest[0] == '\r') {
				rest = rest[1:]
			}
			return msg[:i], rest
		}
	}
	return msg, ""
}

// deriveStat renders a minimal stat from changed files when the real
// git --stat output isn't available.
func deriveStat(diff string, files []string) string {
	if len(files) == 0 {
		return ""
	}
	lines := autotype.CountChangedLines(diff)
	return fmt.Sprintf(" %d files changed, ~%d line changes", len(files), lines)
}
