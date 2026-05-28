//go:build integration

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeReviewModelFlag(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)
	tests := []struct {
		name      string
		model     string
		wantFlag  bool
		wantModel string
	}{
		{
			name:     "no model omits --model from args",
			model:    "",
			wantFlag: false,
		},
		{
			name:      "explicit model passes --model to subprocess",
			model:     "openai/gpt-4o",
			wantFlag:  true,
			wantModel: "openai/gpt-4o",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, args, _ := runMockOpenCodeReview(
				t, tt.model, "review this", nil,
			)
			args = strings.TrimSpace(args)

			assertContains(t, args, "--format json")
			if tt.wantFlag {
				assertContains(t, args, "--model")
				assertContains(t, args, tt.wantModel)
			} else {
				assertNotContains(t, args, "--model")
			}
		})
	}
}

func TestOpenCodeReviewPipesPromptViaStdin(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	prompt := "Review this commit carefully"
	_, args, stdin := runMockOpenCodeReview(t, "", prompt, nil)

	assert.Equal(t, strings.TrimSpace(stdin), prompt)

	assertNotContains(t, args, prompt)
}

func TestOpenCodeReviewParsesJSONStream(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	stdoutLines := []string{
		makeOpenCodeEvent("step_start", map[string]any{
			"type": "step-start",
		}),
		makeTextEvent("**Review:** Fix the typo."),
		makeOpenCodeEvent("tool", map[string]any{
			"type": "tool",
			"tool": "Read",
			"state": map[string]any{
				"status": "running",
				"input": map[string]any{
					"file_path": "/foo/bar.go",
				},
			},
		}),
		makeTextEvent(" Done."),
		makeOpenCodeEvent("step_finish", map[string]any{
			"type":   "step-finish",
			"reason": "stop",
		}),
	}

	result, _, _ := runMockOpenCodeReview(t, "", "prompt", stdoutLines)

	assertEqual(t, result, " Done.")

	assertNotContains(t, result, "Read")
	assertNotContains(t, result, "file_path")
}

func TestOpenCodeReviewPrefersFinalPostToolSegment(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	stdoutLines := []string{
		makeTextEvent("## Review Findings\n- **Severity**: Low; **Problem**: Earlier provisional finding."),
		makeOpenCodeEvent("tool", map[string]any{
			"type": "tool",
			"tool": "Read",
		}),
		makeTextEvent("## Review Findings\n- **Severity**: Medium; **Problem**: Final persisted finding."),
	}

	result, _, _ := runMockOpenCodeReview(t, "", "prompt", stdoutLines)

	assertEqual(t, result, "## Review Findings\n- **Severity**: Medium; **Problem**: Final persisted finding.")
}

func TestOpenCodeReviewStreamsToOutput(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	stdoutLines := []string{
		makeTextEvent("Hello world"),
	}

	var outputBuf bytes.Buffer
	result, _, _, err := executeReviewTest(t, reviewTestOpts{
		MockOpts: MockCLIOpts{
			CaptureStdin: true,
			StdoutLines:  stdoutLines,
		},
		Prompt: "prompt",
		Writer: &outputBuf,
	})
	require.NoError(t, err, "Review failed: %v")

	assertEqual(t, result, "Hello world")

	outStr := outputBuf.String()
	assertContains(t, outStr, `"type":"text"`)
}

func TestOpenCodeReviewPartialOnError(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	stdoutLines := []string{
		makeTextEvent("Partial review text"),
	}

	_, _, _, err := executeReviewTest(t, reviewTestOpts{
		MockOpts: MockCLIOpts{
			CaptureStdin: true,
			StdoutLines:  stdoutLines,
			ExitCode:     1,
		},
		Prompt: "prompt",
	})
	require.Error(t, err)

	assertContains(t, err.Error(), "Partial review text")
}

func TestOpenCodeReviewNoOutput(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	stdoutLines := []string{
		makeOpenCodeEvent("step_start", map[string]any{
			"type": "step-start",
		}),
		makeOpenCodeEvent("step_finish", map[string]any{
			"type":   "step-finish",
			"reason": "stop",
		}),
	}

	result, _, _ := runMockOpenCodeReview(t, "", "prompt", stdoutLines)

	assertEqual(t, result, "No review output generated")
}

func TestOpenCodeReviewStderrNotStreamedToOutput(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	stdoutLines := []string{
		makeTextEvent("Review done"),
	}

	var outputBuf bytes.Buffer
	result, _, _, err := executeReviewTest(t, reviewTestOpts{
		MockOpts: MockCLIOpts{
			CaptureStdin: true,
			StdoutLines:  stdoutLines,
			StderrLines: []string{
				"Performing one time database migration, may take a few minutes...",
				"sqlite-migration:0",
				"sqlite-migration:done",
				"Database migration complete.",
				"warning: something",
			},
		},
		Prompt: "prompt",
		Writer: &outputBuf,
	})
	require.NoError(t, err, "Review failed: %v")

	assertEqual(t, result, "Review done")

	// opencode's stderr is intentionally suppressed from the live
	// log because it is dominated by sqlite-migration noise. Real
	// errors still surface via formatDetailedCLIWaitError on
	// non-zero exit (covered by TestOpenCodeReviewPartialOnError).
	outStr := outputBuf.String()
	assertContains(t, outStr, `"type":"text"`)
	assertNotContains(t, outStr, "sqlite-migration")
	assertNotContains(t, outStr, "Performing one time database migration")
	assertNotContains(t, outStr, "Database migration complete")
	assertNotContains(t, outStr, "warning: something")
}

func TestOpenCodeReviewStderrPreservedInError(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	// StreamStderr is disabled for opencode, so stderr does not
	// appear in the live log on success. On non-zero exit the
	// captured stderr must still reach the user via the returned
	// error, otherwise failure diagnostics would be lost.
	stdoutLines := []string{makeTextEvent("Partial review")}
	stderrLines := []string{
		"Performing one time database migration, may take a few minutes...",
		"sqlite-migration:done",
		"Error: authentication token expired",
	}

	_, _, _, err := executeReviewTest(t, reviewTestOpts{
		MockOpts: MockCLIOpts{
			CaptureStdin: true,
			StdoutLines:  stdoutLines,
			StderrLines:  stderrLines,
			ExitCode:     1,
		},
		Prompt: "prompt",
	})
	require.Error(t, err)

	msg := err.Error()
	assertContains(t, msg, "Error: authentication token expired")
}

func TestOpenCodeReviewNilOutput(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	stdoutLines := []string{
		makeTextEvent("Review content"),
	}

	result, _, _, err := executeReviewTest(t, reviewTestOpts{
		MockOpts: MockCLIOpts{
			CaptureStdin: true,
			StdoutLines:  stdoutLines,
		},
		Prompt: "prompt",
	})
	require.NoError(t, err, "Review with nil output: %v")

	assertEqual(t, result, "Review content")
}

type reviewTestOpts struct {
	MockOpts  MockCLIOpts
	Model     string
	Prompt    string
	Writer    io.Writer
	SessionID string
}

func executeReviewTest(t *testing.T, opts reviewTestOpts) (string, string, string, error) {
	t.Helper()

	if opts.Prompt == "" {
		require.NotEmpty(t, opts.Prompt, "executeReviewTest requires an explicit Prompt")
	}

	mock := mockAgentCLI(t, opts.MockOpts)

	a := NewOpenCodeAgent(mock.CmdPath)
	if opts.Model != "" {
		a.Model = opts.Model
	}
	if opts.SessionID != "" {
		a = a.WithSessionID(opts.SessionID).(*OpenCodeAgent)
	}

	out, err := a.Review(
		context.Background(), t.TempDir(),
		"HEAD", opts.Prompt, opts.Writer,
	)

	var argsBytes, stdinBytes []byte
	if opts.MockOpts.CaptureArgs {
		argsBytes = readFileOrFatal(t, mock.ArgsFile)
	}
	if opts.MockOpts.CaptureStdin {
		stdinBytes = readFileOrFatal(t, mock.StdinFile)
	}

	return out, string(argsBytes), string(stdinBytes), err
}

func runMockOpenCodeReview(
	t *testing.T, model, prompt string,
	stdoutLines []string,
) (output, args, stdin string) {
	t.Helper()

	if stdoutLines == nil {
		stdoutLines = []string{
			makeTextEvent("ok"),
		}
	}

	out, argsStr, stdinStr, err := executeReviewTest(t, reviewTestOpts{
		MockOpts: MockCLIOpts{
			CaptureArgs:  true,
			CaptureStdin: true,
			StdoutLines:  stdoutLines,
		},
		Model:  model,
		Prompt: prompt,
	})
	require.NoError(t, err, "Review failed: %v")

	return out, argsStr, stdinStr
}

func TestOpenCodeReviewSessionFlag(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	_, args, _ := func() (string, string, string) {
		out, argsStr, stdinStr, err := executeReviewTest(t, reviewTestOpts{
			MockOpts: MockCLIOpts{
				CaptureArgs:  true,
				CaptureStdin: true,
				StdoutLines:  []string{makeTextEvent("ok")},
			},
			Prompt:    "prompt",
			SessionID: "ses_123",
		})
		require.NoError(t, err)
		return out, argsStr, stdinStr
	}()

	assertContains(t, args, "--session")
	assertContains(t, args, "ses_123")
}

func readFileOrFatal(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err, "failed to read file %s: %v", path)

	return data
}

func makeOpenCodeEvent(eventType string, part map[string]any) string {
	ev := map[string]any{
		"type": eventType,
		"part": part,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		panic("makeOpenCodeEvent: " + err.Error())
	}
	return string(b)
}

func makeTextEvent(text string) string {
	return makeOpenCodeEvent("text", map[string]any{
		"type": "text",
		"text": text,
	})
}
