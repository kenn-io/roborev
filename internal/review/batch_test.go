package review

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

// mockAgent implements agent.Agent for testing.
type mockAgent struct {
	name   string
	model  string
	output string
	err    error
}

func (m *mockAgent) Name() string { return m.name }
func (m *mockAgent) Review(
	_ context.Context, _, _, _ string, _ io.Writer,
) (string, error) {
	out := m.output
	if m.model != "" {
		out += " model=" + m.model
	}
	return out, m.err
}

func (m *mockAgent) WithReasoning(
	_ agent.ReasoningLevel,
) agent.Agent {
	return m
}

func (m *mockAgent) WithAgentic(_ bool) agent.Agent {
	return m
}

func (m *mockAgent) WithModel(model string) agent.Agent {
	c := *m
	c.model = model
	return &c
}

func (m *mockAgent) CommandLine() string {
	return m.name
}

// getResultByType is a helper to find a ReviewResult by its ReviewType
func getResultByType(t *testing.T, results []ReviewResult, rType string) ReviewResult {
	t.Helper()
	for _, r := range results {
		if r.ReviewType == rType {
			return r
		}
	}
	require.Condition(t, func() bool { return false }, "missing result for type %q", rType)
	return ReviewResult{}
}

func TestRunBatch(t *testing.T) {
	t.Parallel()

	type resultCheck struct {
		agent      string
		reviewType string
		status     string
		errContain string
		outContain string
	}

	tests := []struct {
		name           string
		agents         []string
		reviewTypes    []string
		registry       map[string]agent.Agent
		useInvalidRepo bool // use a non-existent repo to test prompt-build failures
		checks         []resultCheck
	}{
		{
			name:        "SingleJob",
			agents:      []string{"test"},
			reviewTypes: []string{"security"},
			registry: map[string]agent.Agent{
				"test": &mockAgent{name: "test", output: "looks good"},
			},
			checks: []resultCheck{
				{agent: "test", reviewType: "security", status: ResultDone, outContain: "looks good"},
			},
		},
		{
			name:        "Matrix",
			agents:      []string{"test"},
			reviewTypes: []string{"security", "default"},
			registry: map[string]agent.Agent{
				"test": &mockAgent{name: "test", output: "ok"},
			},
			checks: []resultCheck{
				{agent: "test", reviewType: "security", status: ResultDone},
				{agent: "test", reviewType: "default", status: ResultDone},
			},
		},
		{
			name:           "AgentNotFound",
			agents:         []string{"nonexistent-agent-xyz"},
			reviewTypes:    []string{"security"},
			registry:       map[string]agent.Agent{},
			useInvalidRepo: true,
			checks: []resultCheck{
				{status: ResultFailed, errContain: "no agents available (mock registry)"},
			},
		},
		{
			name:        "AgentFailure",
			agents:      []string{"fail-agent"},
			reviewTypes: []string{"security"},
			registry: map[string]agent.Agent{
				"fail-agent": &mockAgent{name: "fail-agent", err: fmt.Errorf("agent exploded")},
			},
			checks: []resultCheck{
				{status: ResultFailed, errContain: "agent exploded"},
			},
		},
		{
			name:        "PromptBuildFailure",
			agents:      []string{"test"},
			reviewTypes: []string{"security"},
			registry: map[string]agent.Agent{
				"test": &mockAgent{name: "test", output: "should not run"},
			},
			useInvalidRepo: true,
			checks: []resultCheck{
				{status: ResultFailed, errContain: "build prompt"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repoPath := filepath.Join(t.TempDir(), "nonexistent")
			gitRef := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
			if !tc.useInvalidRepo {
				repo := testutil.NewTestRepoWithCommit(t)
				repoPath = repo.Root
				gitRef = repo.RevParse("HEAD")
			}

			cfg := BatchConfig{
				RepoPath:      repoPath,
				GitRef:        gitRef,
				Agents:        tc.agents,
				ReviewTypes:   tc.reviewTypes,
				AgentRegistry: tc.registry,
			}

			results := RunBatch(context.Background(), cfg)
			require.Len(t, results, len(tc.checks), "result count")

			for i, chk := range tc.checks {
				r := results[i]
				// For matrix tests, look up by review type since
				// goroutine ordering is not guaranteed.
				if chk.reviewType != "" && len(results) > 1 {
					r = getResultByType(t, results, chk.reviewType)
				}
				if chk.agent != "" {
					assert.Equal(t, chk.agent, r.Agent, "case %d agent", i)
				}
				if chk.reviewType != "" {
					assert.Equal(t, chk.reviewType, r.ReviewType, "case %d reviewType", i)
				}
				assert.Equal(t, chk.status, r.Status, "case %d status (err=%q)", i, r.Error)
				if chk.errContain != "" {
					assert.Contains(t, r.Error, chk.errContain, "case %d error", i)
				}
				if chk.outContain != "" {
					assert.Contains(t, r.Output, chk.outContain, "case %d output", i)
				}
			}
		})
	}
}

func TestRunBatch_WorkflowAwareResolution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	// Configure security_agent override so security reviews
	// resolve to "security-agent" while default reviews use
	// the base agent.
	globalCfg := &config.Config{
		DefaultAgent:  "base-agent",
		SecurityAgent: "security-agent",
	}

	cfg := BatchConfig{
		RepoPath:     t.TempDir(),
		GitRef:       "abc..def",
		Agents:       []string{""},
		ReviewTypes:  []string{"default", "security"},
		GlobalConfig: globalCfg,
		AgentRegistry: map[string]agent.Agent{
			"base-agent": &mockAgent{
				name:   "base-agent",
				output: "base",
			},
			"security-agent": &mockAgent{
				name:   "security-agent",
				output: "security",
			},
		},
	}

	results := RunBatch(context.Background(), cfg)
	require.Len(results, 2, "expected 2 results")

	// Both will fail at prompt building (no real git repo),
	// but we can verify the resolved agent names.
	defResult := getResultByType(t, results, "default")
	secResult := getResultByType(t, results, "security")

	assert.Equal("base-agent", defResult.Agent, "default type resolved to %q, want %q", defResult.Agent, "base-agent")
	assert.Equal("security-agent", secResult.Agent, "security type resolved to %q, want %q", secResult.Agent, "security-agent")
}

func TestRunBatch_WorkflowModelResolution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	// Configure a security-specific model override.
	globalCfg := &config.Config{
		DefaultAgent:  "model-test-agent",
		SecurityModel: "sec-model-v2",
	}

	// Use a real git repo so prompt building succeeds
	// and Review() is called, making model observable.
	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.RevParse("HEAD")

	cfg := BatchConfig{
		RepoPath:     repo.Root,
		GitRef:       sha,
		Agents:       []string{""},
		ReviewTypes:  []string{"default", "security"},
		GlobalConfig: globalCfg,
		AgentRegistry: map[string]agent.Agent{
			"model-test-agent": &mockAgent{
				name:   "model-test-agent",
				output: "ok",
			},
		},
	}

	results := RunBatch(context.Background(), cfg)
	require.Len(results, 2, "expected 2 results")

	for _, r := range results {
		require.Equal(ResultDone, r.Status, "type %s: status=%q err=%q", r.ReviewType, r.Status, r.Error)
	}

	secOut := getResultByType(t, results, "security").Output
	defOut := getResultByType(t, results, "default").Output

	// Security review should have the model applied.
	assert.Contains(secOut, "model=sec-model-v2", "security output missing model, got %q", secOut)
	// Default review should have no model override.
	assert.NotContains(defOut, "model=", "default output should have no model, got %q", defOut)
}

func TestRunBatch_GlobalExcludePatterns(t *testing.T) {
	require := require.New(t)

	t.Parallel()
	repo := testutil.NewTestRepoWithCommit(t)

	// Add both files in one commit so both appear in the diff
	require.NoError(os.WriteFile(
		filepath.Join(repo.Root, "keep.go"),
		[]byte("package main\n"), 0o644))
	require.NoError(os.WriteFile(
		filepath.Join(repo.Root, "generated.dat"),
		[]byte("generated\n"), 0o644))
	repo.RunGit("add", "-A")
	repo.RunGit("commit", "-m", "add files")
	sha := repo.RevParse("HEAD")

	// The mock agent captures the prompt it receives via output
	captureAgent := &promptCapture{name: "capture"}

	cfg := BatchConfig{
		RepoPath:    repo.Root,
		GitRef:      sha,
		Agents:      []string{"capture"},
		ReviewTypes: []string{"review"},
		GlobalConfig: &config.Config{
			ExcludePatterns: []string{"generated.dat"},
		},
		AgentRegistry: map[string]agent.Agent{
			"capture": captureAgent,
		},
	}

	results := RunBatch(context.Background(), cfg)
	require.Len(results, 1)
	require.Equal(ResultDone, results[0].Status,
		"status=%q err=%q", results[0].Status, results[0].Error)

	// The prompt passed to the agent should include keep.go
	// but not the excluded generated.dat
	assert.Contains(t, captureAgent.lastPrompt, "keep.go",
		"prompt should contain retained file")
	assert.NotContains(t, captureAgent.lastPrompt, "generated.dat",
		"prompt should not contain excluded file")
}

func TestRunBatch_CodexReviewSettings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping test that requires Unix shell scripts")
	}

	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.RevParse("HEAD")
	cmdPath, argsPath := writeFakeCodex(t)

	cfg := BatchConfig{
		RepoPath:    repo.Root,
		GitRef:      sha,
		Agents:      []string{"codex"},
		ReviewTypes: []string{"review"},
		AgentRegistry: map[string]agent.Agent{
			"codex": agent.NewCodexAgent(cmdPath),
		},
	}

	results := RunBatch(context.Background(), cfg)
	require.Len(t, results, 1)
	require.Equal(t, ResultDone, results[0].Status, "status=%q err=%q", results[0].Status, results[0].Error)

	argsBytes, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	args := string(argsBytes)
	assert.Contains(t, args, "--ignore-user-config")
	assert.Contains(t, args, "skills.include_instructions=false")
}

func writeFakeCodex(t *testing.T) (cmdPath string, argsPath string) {
	t.Helper()

	dir := t.TempDir()
	argsPath = filepath.Join(dir, "args.txt")
	script := strings.Join([]string{
		"#!/bin/sh",
		"case \"$*\" in *--help*) echo 'usage --sandbox --ignore-user-config'; exit 0;; esac",
		fmt.Sprintf("echo \"$@\" > %q", argsPath),
		"echo '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}'",
	}, "\n") + "\n"

	cmdPath = filepath.Join(dir, "codex")
	require.NoError(t, os.WriteFile(cmdPath, []byte(script), 0o755))
	return cmdPath, argsPath
}

// promptCapture is a mock agent that records the prompt it receives.
type promptCapture struct {
	name       string
	lastPrompt string
}

func (p *promptCapture) Name() string { return p.name }
func (p *promptCapture) Review(
	_ context.Context, _, _, prompt string, _ io.Writer,
) (string, error) {
	p.lastPrompt = prompt
	return "ok", nil
}

func (p *promptCapture) WithReasoning(
	_ agent.ReasoningLevel,
) agent.Agent {
	return p
}
func (p *promptCapture) WithAgentic(_ bool) agent.Agent { return p }
func (p *promptCapture) WithModel(_ string) agent.Agent { return p }
func (p *promptCapture) CommandLine() string            { return p.name }

func TestRunBatch_BackupKeepsOwnModelWhenBackupModelUnset(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.RevParse("HEAD")

	cfg := BatchConfig{
		RepoPath:    repo.Root,
		GitRef:      sha,
		Agents:      []string{""},
		ReviewTypes: []string{"review"},
		GlobalConfig: &config.Config{
			DefaultAgent:      "default-agent",
			ReviewAgent:       "primary-agent",
			ReviewBackupAgent: "backup-agent",
			ReviewModel:       "primary-model",
		},
		AgentRegistry: map[string]agent.Agent{
			// Simulate the runtime-selected backup agent while keeping
			// the configured preferred agent name distinct.
			"primary-agent": &mockAgent{
				name:   "backup-agent",
				output: "ok",
			},
		},
	}

	results := RunBatch(context.Background(), cfg)
	require.Len(results, 1, "expected 1 result")

	result := results[0]
	require.Equal(ResultDone, result.Status, "status=%q err=%q", result.Status, result.Error)
	assert.NotContains(result.Output, "model=", "backup agent should keep its default model, got %q", result.Output)
}

func TestRunBatchIgnoresMalformedRepoConfig(t *testing.T) {
	t.Parallel()

	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.RevParse("HEAD")
	err := os.WriteFile(
		filepath.Join(repo.Root, ".roborev.toml"),
		[]byte("review_agent = ["),
		0o644,
	)
	require.NoError(t, err)

	cfg := BatchConfig{
		RepoPath:     repo.Root,
		GitRef:       sha,
		Agents:       []string{""},
		ReviewTypes:  []string{"review"},
		GlobalConfig: &config.Config{DefaultAgent: "batch-agent"},
		AgentRegistry: map[string]agent.Agent{
			"batch-agent": &mockAgent{
				name:   "batch-agent",
				output: "ok",
			},
		},
	}

	results := RunBatch(context.Background(), cfg)
	require.Len(t, results, 1)
	require.Equal(t, ResultDone, results[0].Status, "status=%q err=%q", results[0].Status, results[0].Error)
	assert.Equal(t, "batch-agent", results[0].Agent)
}

// installFakeKata copies the test binary to dir as `kata` and points PATH and
// ROBOREV_TEST_FAKE_KATA at it, so any kata CLI invocation deterministically
// returns an open issue (see TestMain) instead of depending on whether the
// developer machine has kata installed.
func installFakeKata(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(self)
	require.NoError(t, err)
	dir := t.TempDir()
	name := "kata"
	if runtime.GOOS == "windows" {
		name = "kata.exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o755))
	t.Setenv("ROBOREV_TEST_FAKE_KATA", "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunBatch_NoKataContextFromUntrustedCheckout(t *testing.T) {
	require := require.New(t)

	// No t.Parallel: installFakeKata uses t.Setenv.
	installFakeKata(t)
	repo := testutil.NewTestRepoWithCommit(t)

	// An untrusted PR head controls the checkout: it can both enable
	// kata_context in .roborev.toml and reference kata issues in commit
	// messages. Daemon-free CI must never resolve kata context.
	require.NoError(os.WriteFile(
		filepath.Join(repo.Root, ".roborev.toml"),
		[]byte("[kata_context]\nmode = \"open\"\n"), 0o644))
	require.NoError(os.WriteFile(
		filepath.Join(repo.Root, ".kata.toml"),
		[]byte("[project]\nname = \"victim\"\n"), 0o644))
	repo.RunGit("add", "-A")
	repo.RunGit("commit", "-m", "Sneaky change\n\nCloses: kata#abc4")
	sha := repo.RevParse("HEAD")

	captureAgent := &promptCapture{name: "capture"}
	cfg := BatchConfig{
		RepoPath:     repo.Root,
		GitRef:       sha,
		Agents:       []string{"capture"},
		ReviewTypes:  []string{"review"},
		GlobalConfig: &config.Config{},
		AgentRegistry: map[string]agent.Agent{
			"capture": captureAgent,
		},
	}

	results := RunBatch(context.Background(), cfg)
	require.Len(results, 1)
	require.Equal(ResultDone, results[0].Status,
		"status=%q err=%q", results[0].Status, results[0].Error)
	assert.NotContains(t, captureAgent.lastPrompt, "Task Context (kata)",
		"daemon-free CI must not include kata context from an untrusted checkout")
	assert.NotContains(t, captureAgent.lastPrompt, "Secret kata body.",
		"fake kata output must never reach the prompt")
}
