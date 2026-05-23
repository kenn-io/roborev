package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/testenv"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunIsolatedMain(m))
}

func TestAgentRegistry(t *testing.T) {
	// Check that all agents are registered
	agents := expectedAgents
	for _, name := range agents {
		a, err := Get(name)
		require.NoError(t, err, "Failed to get %s agent", name)
		assert.Equal(t, name, a.Name())
	}

	// Check unknown agent
	_, err := Get("unknown-agent")
	require.Error(t, err)
}

func TestCanonicalNameAliases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "claude", want: "claude-code"},
		{input: "agent", want: "cursor"},
		{input: "cursor", want: "cursor"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, CanonicalName(tt.input), "CanonicalName(%q)", tt.input)
	}
}

func TestGetSupportsAgentAlias(t *testing.T) {
	a, err := Get("agent")
	require.NoError(t, err)
	assert.Equal(t, "cursor", a.Name())
}

func TestAvailableAgents(t *testing.T) {
	agents := Available()
	assert.GreaterOrEqual(t, len(agents), len(expectedAgents), "agents=%v", agents)

	for _, name := range expectedAgents {
		assert.Contains(t, agents, name)
	}
}

func TestSyncWriter(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		sw := newSyncWriter(nil)
		assert.Nil(t, sw)
	})

	t.Run("wraps writer correctly", func(t *testing.T) {
		var buf bytes.Buffer
		sw := newSyncWriter(&buf)
		require.NotNil(t, sw)

		n, err := sw.Write([]byte("hello"))
		require.NoError(t, err)
		assert.Equal(t, 5, n)
		assert.Equal(t, "hello", buf.String())
	})

	t.Run("concurrent writes are safe", func(t *testing.T) {
		var buf bytes.Buffer
		sw := newSyncWriter(&buf)

		var wg sync.WaitGroup
		for i := range 100 {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				sw.Write([]byte("x"))
			}(i)
		}
		wg.Wait()

		assert.Equal(t, 100, buf.Len())
	})
}

func TestTestAgentStreaming(t *testing.T) {
	setup := func() *TestAgent {
		a := NewTestAgent()
		a.Delay = 1 * time.Millisecond
		a.Output = "test output"
		return a
	}

	t.Run("streams output to writer", func(t *testing.T) {
		agent := setup()

		var buf bytes.Buffer
		result, err := agent.Review(context.Background(), "/tmp", "abc1234567", "prompt", &buf)
		require.NoError(t, err)
		// The streamed output is the returned body prefixed by a synthetic
		// session JSONL line (consumed by SessionCaptureWriter). Verify the
		// body is present in the stream rather than asserting exact equality.
		assert.Contains(t, buf.String(), result, "streamed output should contain the returned body")
	})

	t.Run("nil output writer works", func(t *testing.T) {
		agent := setup()

		result, err := agent.Review(context.Background(), "/tmp", "abc1234567", "prompt", nil)
		require.NoError(t, err)
		assert.NotEmpty(t, result)
	})

	t.Run("returns write error", func(t *testing.T) {
		agent := setup()

		errWriter := &FailingWriter{Err: errors.New("write failed")}
		_, err := agent.Review(context.Background(), "/tmp", "abc1234567", "prompt", errWriter)
		require.Error(t, err)
		assert.ErrorIs(t, err, errWriter.Err)
	})
}

func TestParseReasoningLevel(t *testing.T) {
	tests := []struct {
		input string
		want  ReasoningLevel
	}{
		{"maximum", ReasoningMaximum},
		{"max", ReasoningMaximum},
		{"xhigh", ReasoningMaximum},
		{"thorough", ReasoningThorough},
		{"high", ReasoningThorough},
		{"fast", ReasoningFast},
		{"low", ReasoningFast},
		{"medium", ReasoningMedium},
		{"standard", ReasoningStandard},
		{"", ReasoningStandard},
		{"unknown", ReasoningStandard},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, ParseReasoningLevel(tt.input), "ParseReasoningLevel(%q)", tt.input)
	}
}

func TestCodexReasoningEffortMapping(t *testing.T) {
	tests := []struct {
		level ReasoningLevel
		want  string
	}{
		{ReasoningMaximum, "xhigh"},
		{ReasoningThorough, "high"},
		{ReasoningFast, "low"},
		{ReasoningStandard, ""},
	}

	for _, tt := range tests {
		a := NewCodexAgent("").WithReasoning(tt.level)
		codex, ok := a.(*CodexAgent)
		require.True(t, ok, "expected CodexAgent, got %T", a)
		assert.Equal(t, tt.want, codex.codexReasoningEffort(), "codexReasoningEffort(%q)", tt.level)
	}
}

func TestClaudeEffortMapping(t *testing.T) {
	tests := []struct {
		level ReasoningLevel
		want  string
	}{
		{ReasoningMaximum, "max"},
		{ReasoningThorough, "high"},
		{ReasoningMedium, "medium"},
		{ReasoningFast, "low"},
		{ReasoningStandard, ""},
	}

	for _, tt := range tests {
		a := NewClaudeAgent("").WithReasoning(tt.level)
		claude, ok := a.(*ClaudeAgent)
		require.True(t, ok, "expected ClaudeAgent, got %T", a)
		assert.Equal(t, tt.want, claude.claudeEffort(), "claudeEffort(%q)", tt.level)
	}
}

type agentTestDef struct {
	name                  string
	factory               func(string) Agent
	modelFlag             string
	defaultModel          string
	testModel             string
	supportsSmartReview   bool
	supportsPlainFlagEcho bool
}

var agentFixtures = []agentTestDef{
	{"codex", func(s string) Agent { return NewCodexAgent(s) }, "-m", "", "o3", true, false},
	{"claude", func(s string) Agent { return NewClaudeAgent(s) }, "--model", "", "opus", true, false},
	{"gemini", func(s string) Agent { return NewGeminiAgent(s) }, "-m", "gemini-3.1-pro-preview", "gemini-1.5-pro", true, false},
	{"copilot", func(s string) Agent { return NewCopilotAgent(s) }, "--model", "", "gpt-4o", false, true},
	{"opencode", func(s string) Agent { return NewOpenCodeAgent(s) }, "--model", "", "anthropic/claude-sonnet-4", false, false},
	{"cursor", func(s string) Agent { return NewCursorAgent(s) }, "--model", "auto", "claude-sonnet-4", false, false},
	{"kilo", func(s string) Agent { return NewKiloAgent(s) }, "--model", "", "anthropic/claude-sonnet-4-20250514", false, false},
}

func assertArgsNotContain(t *testing.T, cmdLine, flag string) {
	t.Helper()
	for token := range strings.FieldsSeq(cmdLine) {
		assert.Condition(t, func() bool { return token != flag && !strings.HasPrefix(token, flag+"=") }, "command line %q unexpectedly contained flag %q", cmdLine, flag)
	}
}

func TestAgentWithModelPersistence(t *testing.T) {
	for _, tt := range agentFixtures {
		t.Run(tt.name+"/WithModel sets model", func(t *testing.T) {
			a := tt.factory("").WithModel(tt.testModel)
			cmdLine := a.CommandLine()
			assertArgsContain(t, cmdLine, tt.modelFlag, tt.testModel)
		})

		t.Run(tt.name+"/model persists through WithReasoning", func(t *testing.T) {
			a := tt.factory("").WithModel(tt.testModel).WithReasoning(ReasoningThorough)
			cmdLine := a.CommandLine()
			assertArgsContain(t, cmdLine, tt.modelFlag, tt.testModel)
		})

		t.Run(tt.name+"/model persists through WithAgentic", func(t *testing.T) {
			a := tt.factory("").WithModel(tt.testModel).WithAgentic(true)
			cmdLine := a.CommandLine()
			assertArgsContain(t, cmdLine, tt.modelFlag, tt.testModel)
		})

		t.Run(tt.name+"/model persists through chained calls", func(t *testing.T) {
			a := tt.factory("").WithModel(tt.testModel).WithReasoning(ReasoningFast).WithAgentic(true)
			cmdLine := a.CommandLine()
			assertArgsContain(t, cmdLine, tt.modelFlag, tt.testModel)
		})
	}
}

func TestWithModelEmptyPreservesDefault(t *testing.T) {
	for _, tt := range agentFixtures {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.factory("")
			b := a.WithModel("")
			cmdLine := b.CommandLine()

			if tt.defaultModel == "" {
				assertArgsNotContain(t, cmdLine, tt.modelFlag)
			} else {
				assertArgsContain(t, cmdLine, tt.modelFlag, tt.defaultModel)
			}
		})

		t.Run(tt.name+"/explicit then empty preserves explicit", func(t *testing.T) {
			a := tt.factory("").WithModel("custom-model")
			b := a.WithModel("")
			cmdLine := b.CommandLine()

			assertArgsContain(t, cmdLine, tt.modelFlag, "custom-model")
		})
	}
}

func TestClaudeBuildArgsEffort(t *testing.T) {
	a := NewClaudeAgent("").WithModel("opus").WithReasoning(ReasoningMaximum)
	cmdLine := a.CommandLine()

	assert.Contains(t, cmdLine, "--effort max")
	assert.Contains(t, cmdLine, "--model opus")
}

func TestClaudeBuildArgsNoEffortForStandard(t *testing.T) {
	a := NewClaudeAgent("").WithReasoning(ReasoningStandard)
	cmdLine := a.CommandLine()

	assert.NotContains(t, cmdLine, "--effort")
}

func TestAgentBuildArgsWithModel(t *testing.T) {
	for _, tt := range agentFixtures {
		t.Run(tt.name+" with explicit model", func(t *testing.T) {
			agent := tt.factory("").WithModel(tt.testModel)
			cmdLine := agent.CommandLine()
			assertArgsContain(t, cmdLine, tt.modelFlag, tt.testModel)
		})

		t.Run(tt.name+" without model", func(t *testing.T) {
			agent := tt.factory("")
			cmdLine := agent.CommandLine()

			if tt.defaultModel == "" {
				assertArgsNotContain(t, cmdLine, tt.modelFlag)
			} else {
				assertArgsContain(t, cmdLine, tt.modelFlag, tt.defaultModel)
			}
		})
	}
}

func TestCodexBuildArgsModelWithReasoning(t *testing.T) {
	a := NewCodexAgent("").WithModel("o4-mini").WithReasoning(ReasoningThorough)
	cmdLine := a.CommandLine()

	assert.Contains(t, cmdLine, "-m o4-mini")
	assert.Contains(t, cmdLine, `-c model_reasoning_effort="high"`)
}

func TestSessionAgentsPreserveStateAcrossCloneMethods(t *testing.T) {
	tests := []struct {
		name   string
		agent  Agent
		verify func(*testing.T, Agent)
	}{
		{
			name: "codex",
			agent: NewCodexAgent("codex").
				WithSessionID("session-123").
				WithModel("o4-mini").
				WithReasoning(ReasoningThorough).
				WithAgentic(true),
			verify: func(t *testing.T, a Agent) {
				codex, ok := a.(*CodexAgent)
				require.True(t, ok)
				assert.Equal(t, "session-123", codex.SessionID)
				assert.Equal(t, "o4-mini", codex.Model)
				assert.Equal(t, ReasoningThorough, codex.Reasoning)
				assert.True(t, codex.Agentic)
			},
		},
		{
			name: "claude",
			agent: NewClaudeAgent("claude").
				WithSessionID("session-123").
				WithModel("opus").
				WithReasoning(ReasoningThorough).
				WithAgentic(true),
			verify: func(t *testing.T, a Agent) {
				claude, ok := a.(*ClaudeAgent)
				require.True(t, ok)
				assert.Equal(t, "session-123", claude.SessionID)
				assert.Equal(t, "opus", claude.Model)
				assert.Equal(t, ReasoningThorough, claude.Reasoning)
				assert.True(t, claude.Agentic)
			},
		},
		{
			name: "opencode",
			agent: NewOpenCodeAgent("opencode").
				WithSessionID("session-123").
				WithModel("anthropic/claude-sonnet-4").
				WithReasoning(ReasoningThorough).
				WithAgentic(true),
			verify: func(t *testing.T, a Agent) {
				opencode, ok := a.(*OpenCodeAgent)
				require.True(t, ok)
				assert.Equal(t, "session-123", opencode.SessionID)
				assert.Equal(t, "anthropic/claude-sonnet-4", opencode.Model)
				assert.Equal(t, ReasoningThorough, opencode.Reasoning)
				assert.True(t, opencode.Agentic)
			},
		},
		{
			name: "kilo",
			agent: NewKiloAgent("kilo").
				WithSessionID("session-123").
				WithModel("anthropic/claude-sonnet-4").
				WithReasoning(ReasoningThorough).
				WithAgentic(true),
			verify: func(t *testing.T, a Agent) {
				kilo, ok := a.(*KiloAgent)
				require.True(t, ok)
				assert.Equal(t, "session-123", kilo.SessionID)
				assert.Equal(t, "anthropic/claude-sonnet-4", kilo.Model)
				assert.Equal(t, ReasoningThorough, kilo.Reasoning)
				assert.True(t, kilo.Agentic)
			},
		},
		{
			name: "pi",
			agent: func() Agent {
				pi := NewPiAgent("pi").WithProvider("anthropic").(*PiAgent)
				return pi.
					WithSessionID("session-123").
					WithModel("claude-sonnet-4").
					WithReasoning(ReasoningThorough).
					WithAgentic(true)
			}(),
			verify: func(t *testing.T, a Agent) {
				pi, ok := a.(*PiAgent)
				require.True(t, ok)
				assert.Equal(t, "anthropic", pi.Provider)
				assert.Equal(t, "session-123", pi.SessionID)
				assert.Equal(t, "claude-sonnet-4", pi.Model)
				assert.Equal(t, ReasoningThorough, pi.Reasoning)
				assert.True(t, pi.Agentic)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.verify(t, tt.agent)
		})
	}
}

func assertArgsContain(t *testing.T, cmdLine, flag, value string) {
	t.Helper()
	tokens := strings.Fields(cmdLine)
	found := false
	for i := 0; i < len(tokens)-1; i++ {
		if tokens[i] == flag && tokens[i+1] == value {
			found = true
			break
		}
	}
	assert.Condition(t, func() bool { return found }, "command line %q expected to contain flag %q followed by value %q", cmdLine, flag, value)
}

func TestSmartAgentReviewPassesModelFlag(t *testing.T) {
	skipIfWindows(t)

	for _, tt := range agentFixtures {
		if !tt.supportsSmartReview {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			helpOutput := ""
			jsonOutput := `{"type": "result", "result": "review result"}`
			if tt.name == "codex" {
				helpOutput = "--sandbox"
				jsonOutput = `{"type": "item.completed", "item": {"type": "agent_message", "text": "review result"}}`
			}

			opts := MockCLIOpts{
				HelpOutput:  helpOutput,
				CaptureArgs: true,
				StdoutLines: []string{jsonOutput},
			}
			mock := mockAgentCLI(t, opts)

			agent := tt.factory(mock.CmdPath).WithModel(tt.testModel)
			_, err := agent.Review(context.Background(), t.TempDir(), "head", "prompt", nil)
			require.NoError(t, err)

			argsBytes, err := os.ReadFile(mock.ArgsFile)
			require.NoError(t, err)
			args := string(argsBytes)

			assertArgsContain(t, args, tt.modelFlag, tt.testModel)
		})
	}
}

func TestAgentReviewPassesModelFlag(t *testing.T) {
	for _, tt := range agentFixtures {
		// opencode uses JSON streaming so verifyAgentPassesFlag (plain text echo)
		// doesn't work; model flag is verified in TestOpenCodeReviewModelFlag.
		if !tt.supportsPlainFlagEcho {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			verifyAgentPassesFlag(t, func(cmdPath string) Agent {
				return tt.factory(cmdPath).WithModel(tt.testModel)
			}, tt.modelFlag, tt.testModel)
		})
	}
}

func TestGetAvailableRejectsUnknownAgent(t *testing.T) {
	_, err := GetAvailable("typo-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown agent")
}

func TestGetAvailableFallsBackForKnownUnavailable(t *testing.T) {
	// Isolate registry: "codex" has a missing binary, "claude-code"
	// has its binary on PATH. Request "codex" and verify fallback
	// returns "claude-code" without an "unknown agent" error.
	fakeBin := t.TempDir()
	binName := "claude"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	claudeBin := filepath.Join(fakeBin, binName)
	require.NoError(t, os.WriteFile(claudeBin, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"codex":       NewCodexAgent("definitely-not-on-path"),
		"claude-code": NewClaudeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	resolved, err := GetAvailable("codex")
	require.NoError(t, err)
	assert.Equal(t, "claude-code", resolved.Name())
}

func TestGetAvailableTriesBackupBeforeChain(t *testing.T) {
	// Setup: codex unavailable, gemini available, claude-code available.
	// Without backup: GetAvailable("codex") → claude-code (first in chain).
	// With backup "gemini": GetAvailable("codex", "gemini") → gemini.
	fakeBin := t.TempDir()
	for _, bin := range []string{"gemini", "claude"} {
		name := bin
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		p := filepath.Join(fakeBin, name)
		require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	}
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"codex":       NewCodexAgent("definitely-not-on-path"),
		"gemini":      NewGeminiAgent(""),
		"claude-code": NewClaudeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	// With backup, should pick gemini (not claude-code from chain)
	resolved, err := GetAvailable("codex", "gemini")
	require.NoError(t, err)
	assert.Equal(t, "gemini", resolved.Name())

	// Without backup, should still fall through to claude-code
	resolved, err = GetAvailable("codex")
	require.NoError(t, err)
	assert.Equal(t, "claude-code", resolved.Name())
}

func TestGetAvailablePrefersAntigravityForGemini(t *testing.T) {
	fakeBin := t.TempDir()
	for _, bin := range []string{"agy", "gemini"} {
		name := bin
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		p := filepath.Join(fakeBin, name)
		require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	}
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"gemini": NewGeminiAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	resolved, err := GetAvailable("gemini")
	require.NoError(t, err)
	require.IsType(t, &GeminiAgent{}, resolved)
	gemini := resolved.(*GeminiAgent)
	assert.Equal(t, "gemini", gemini.Name())
	assert.Equal(t, "agy", gemini.CommandName())
	assert.True(t, gemini.CommandAuto)
}

func TestGeminiWithModelUsesLegacyCommandWhenAutoSelectedAntigravity(t *testing.T) {
	fakeBin := t.TempDir()
	for _, bin := range []string{"agy", "gemini"} {
		name := bin
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		p := filepath.Join(fakeBin, name)
		require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	}
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"gemini": NewGeminiAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	resolved, err := GetAvailable("gemini")
	require.NoError(t, err)

	withModel := resolved.WithModel("gemini-1.5-pro")
	require.IsType(t, &GeminiAgent{}, withModel)
	gemini := withModel.(*GeminiAgent)
	assert.Equal(t, "gemini", gemini.CommandName())
	assert.True(t, gemini.ModelExplicit)
	assert.False(t, gemini.CommandAuto)
}

func TestGetAvailableFallsBackToGeminiWhenAntigravityMissing(t *testing.T) {
	fakeBin := t.TempDir()
	name := "gemini"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(fakeBin, name)
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"gemini": NewGeminiAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	resolved, err := GetAvailable("gemini")
	require.NoError(t, err)
	require.IsType(t, &GeminiAgent{}, resolved)
	gemini := resolved.(*GeminiAgent)
	assert.Equal(t, "gemini", gemini.CommandName())
}

func TestGetAvailableBackupSkipsDuplicateAndEmpty(t *testing.T) {
	// Backup that matches preferred or is empty should be skipped.
	fakeBin := t.TempDir()
	binName := "claude"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	p := filepath.Join(fakeBin, binName)
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"codex":       NewCodexAgent("definitely-not-on-path"),
		"claude-code": NewClaudeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	// Backup same as preferred → skipped, falls through to chain
	resolved, err := GetAvailable("codex", "codex", "")
	require.NoError(t, err)
	assert.Equal(t, "claude-code", resolved.Name())
}

func TestGetAvailableBackupResolvesAliases(t *testing.T) {
	// Backup "claude" should resolve to "claude-code" via alias.
	fakeBin := t.TempDir()
	binName := "claude"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	p := filepath.Join(fakeBin, binName)
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"codex":       NewCodexAgent("definitely-not-on-path"),
		"claude-code": NewClaudeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	resolved, err := GetAvailable("codex", "claude")
	require.NoError(t, err)
	assert.Equal(t, "claude-code", resolved.Name())
}

func TestGetAvailableBackupUnavailableFallsToChain(t *testing.T) {
	// Backup agent that is unavailable should be skipped, chain used.
	fakeBin := t.TempDir()
	binName := "claude"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	p := filepath.Join(fakeBin, binName)
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"codex":       NewCodexAgent("definitely-not-on-path"),
		"gemini":      NewGeminiAgent("also-not-on-path"),
		"claude-code": NewClaudeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	// Backup gemini is registered but unavailable → falls to chain
	resolved, err := GetAvailable("codex", "gemini")
	require.NoError(t, err)
	assert.Equal(t, "claude-code", resolved.Name())
}
