package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitrepo "go.kenn.io/kit/git/repo"
)

func TestRunPlanIsolation(t *testing.T) {
	for _, action := range []string{"read", "edit", "commit", "branch", "empty", "no-output", "error", "cancel"} {
		t.Run(action, func(t *testing.T) {
			repoPath := newPlanTestRepo(t)
			before, err := gitrepo.Resolve(context.Background(), repoPath, "HEAD")
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var planningDir string
			a := &FakeAgent{NameStr: "planner", ReviewFn: func(ctx context.Context, dir, ref, p string, w io.Writer) (string, error) {
				planningDir = dir
				assert.NotEqual(t, repoPath, dir)
				assert.Equal(t, "plan the fix", p)
				switch action {
				case "edit", "commit":
					require.NoError(t, os.WriteFile(filepath.Join(dir, "source.go"), []byte("changed\n"), 0o600))
					if action == "commit" {
						planGit(t, dir, "add", "source.go")
						planGit(t, dir, "commit", "-m", "unexpected edit")
					}
				case "branch":
					planGit(t, dir, "checkout", "-b", "unexpected-branch")
				case "empty":
					return " \n", nil
				case "no-output":
					return "No review output generated", nil
				case "error":
					return "", errors.New("provider unavailable")
				case "cancel":
					cancel()
					return "", ctx.Err()
				}
				return "Update source.go and verify the fix", nil
			}}
			result, err := RunPlan(ctx, a, repoPath, before, "plan the fix", io.Discard, nil)
			if action == "read" {
				require.NoError(t, err)
				assert.Equal(t, "Update source.go and verify the fix", result)
			} else {
				require.Error(t, err)
				switch action {
				case "edit":
					require.ErrorContains(t, err, "uncommitted changes")
				case "commit":
					require.ErrorContains(t, err, "changed HEAD")
				case "branch":
					require.ErrorContains(t, err, "attached a branch")
				}
			}
			_, statErr := os.Stat(planningDir)
			require.ErrorIs(t, statErr, os.ErrNotExist)
			after, err := gitrepo.Resolve(context.Background(), repoPath, "HEAD")
			require.NoError(t, err)
			assert.Equal(t, before, after)
			data, err := os.ReadFile(filepath.Join(repoPath, "source.go"))
			require.NoError(t, err)
			assert.Equal(t, "original\n", string(data))
		})
	}
}

func TestRunPlanFreshSession(t *testing.T) {
	repoPath := newPlanTestRepo(t)
	a := NewTestAgent()
	_, err := RunPlan(context.Background(), a.WithSessionID("implementation-session"), repoPath, "HEAD", "plan", io.Discard, nil)
	require.NoError(t, err)
	require.Len(t, a.Calls(), 1)
	assert.Empty(t, a.Calls()[0].SessionID)
}

func TestPlanningOverridesUnsafeMode(t *testing.T) {
	SetAllowUnsafeAgents(true)
	t.Cleanup(func() { SetAllowUnsafeAgents(false) })
	for _, name := range []string{"cursor", "codex", "claude", "gemini", "copilot", "pi", "opencode", "kilo", "grok"} {
		t.Run(name, func(t *testing.T) {
			repoPath := newPlanTestRepo(t)
			argsPath := filepath.Join(t.TempDir(), "args")
			script := "#!/bin/sh\ncase \"$*\" in\n*--help*) echo '--sandbox --dangerously-skip-permissions --dangerously-bypass-approvals-and-sandbox --tools --stream --output-format --disable-builtin-mcps'; exit 0;;\nesac\nprintf '%s\\n' \"$@\" > '" + argsPath + "'\nexit 1\n"
			command := writeTempCommand(t, script)
			var a Agent
			switch name {
			case "cursor":
				a = NewCursorAgent(command)
			case "codex":
				a = NewCodexAgent(command)
			case "claude":
				a = NewClaudeAgent(command)
			case "gemini":
				a = NewGeminiAgent(command)
			case "copilot":
				a = NewCopilotAgent(command)
			case "pi":
				a = NewPiAgent(command)
			case "opencode":
				a = NewOpenCodeAgent(command)
			case "kilo":
				a = NewKiloAgent(command)
			case "grok":
				a = NewGrokAgent(command)
			}
			_, err := RunPlan(context.Background(), a.WithAgentic(true), repoPath, "HEAD", "plan", io.Discard, nil)
			require.Error(t, err) // The controlled CLI exits after recording its arguments.
			data, err := os.ReadFile(argsPath)
			require.NoError(t, err)
			args := string(data)
			assert.NotContains(t, args, "--dangerously-skip-permissions")
			assert.NotContains(t, args, "--dangerously-bypass-approvals-and-sandbox")
			assert.NotContains(t, args, "--force")
			assert.NotContains(t, args, "--yolo")
			switch name {
			case "cursor":
				assert.Contains(t, args, "--mode\nplan\n")
			case "codex":
				assert.Contains(t, args, "--sandbox\nread-only\n")
			case "pi":
				assert.Contains(t, args, "--tools\nread,grep,find,ls\n")
				assert.Equal(t, 1, strings.Count(args, "--tools\n"))
				assert.Contains(t, args, "--no-extensions")
			case "kilo":
				assert.NotContains(t, args, "--auto")
				assert.Contains(t, args, "--agent\nplan\n")
			case "opencode":
				assert.Contains(t, args, "--agent\nplan\n")
			case "grok":
				assert.Contains(t, args, "--sandbox\nread-only\n")
				assert.Contains(t, args, "--tools\nread_file,grep,list_dir\n")
				assert.NotContains(t, args, "--always-approve")
			}
			assert.True(t, AllowUnsafeAgents())
		})
	}
}

func TestGrokPlanningOverridesWritableSandboxProfiles(t *testing.T) {
	withUnsafeAgents(t, true)
	for _, sandbox := range []string{"workspace", "off"} {
		t.Run(sandbox, func(t *testing.T) {
			repoPath := newPlanTestRepo(t)
			argsPath := filepath.Join(t.TempDir(), "args")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + filepath.ToSlash(argsPath) + "'\n" +
				"printf '%s\\n' '{\"type\":\"text\",\"data\":\"plan\"}'\n" +
				"printf '%s\\n' '{\"type\":\"end\",\"stopReason\":\"end_turn\"}'\n"
			command := writeTempCommand(t, script)
			a := NewGrokAgent(command).WithAgentic(true).(*GrokAgent)
			a.Sandbox = sandbox

			result, err := RunPlan(context.Background(), a, repoPath, "HEAD", "plan", io.Discard, nil)
			require.NoError(t, err)
			assert.Equal(t, "plan", result)

			data, err := os.ReadFile(argsPath)
			require.NoError(t, err)
			args := strings.Split(strings.TrimSpace(string(data)), "\n")
			assertGrokPlanReadOnlyArgs(t, args)
			assert.NotContains(t, args, "--always-approve")
			assert.True(t, AllowUnsafeAgents())
		})
	}
}

func TestGrokPlanningSandboxRefusalFallsBackWhenUnsafeEnabled(t *testing.T) {
	withUnsafeAgents(t, true)
	repoPath := newPlanTestRepo(t)
	argsPath := filepath.Join(t.TempDir(), "args")
	refusedPath := filepath.Join(t.TempDir(), "refused")
	script := "#!/bin/sh\ncase \"$1\" in *etxtbsy*) exit 0;; esac\n" +
		"printf '%s\\n' '---attempt---' >> '" + filepath.ToSlash(argsPath) + "'\n" +
		"printf '%s\\n' \"$@\" >> '" + filepath.ToSlash(argsPath) + "'\n" +
		"if [ ! -f '" + filepath.ToSlash(refusedPath) + "' ]; then\n" +
		"  : > '" + filepath.ToSlash(refusedPath) + "'\n" +
		"  printf '%s\\n' 'sandbox: Refusing to start' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf '%s\\n' '{\"type\":\"text\",\"data\":\"plan\"}'\n" +
		"printf '%s\\n' '{\"type\":\"end\",\"stopReason\":\"end_turn\"}'\n"
	command := writeTempCommand(t, script)
	a := NewGrokAgent(command).WithAgentic(true)

	result, err := RunPlan(context.Background(), a, repoPath, "HEAD", "plan", io.Discard, nil)
	require.NoError(t, err)
	assert.Equal(t, "plan", result)

	data, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	attempts := strings.Split(string(data), "---attempt---\n")
	require.Len(t, attempts, 3)
	firstAttempt, secondAttempt := attempts[1], attempts[2]
	assert.Contains(t, firstAttempt, "--sandbox\nread-only\n")
	assert.Contains(t, secondAttempt, "--sandbox\nworkspace\n")
	assert.Contains(t, firstAttempt, "--tools\nread_file,grep,list_dir\n")
	assert.Contains(t, secondAttempt, "--tools\nread_file,grep,list_dir\n")
	assert.NotContains(t, firstAttempt, "--always-approve")
	assert.NotContains(t, secondAttempt, "--always-approve")
	assert.True(t, AllowUnsafeAgents())
}

func assertGrokPlanReadOnlyArgs(t *testing.T, args []string) {
	t.Helper()
	assertArgsContainContiguous(t, args, []string{"--sandbox", "read-only"})
	assertArgsContainContiguous(t, args, []string{"--tools", "read_file,grep,list_dir"})
	assertArgsContainContiguous(t, args, []string{"--no-subagents"})
	assertArgsContainContiguous(t, args, []string{"--disable-web-search"})
	assert.Contains(t, args, "--disallowed-tools")
	disallowedIndex := slices.Index(args, "--disallowed-tools")
	require.Less(t, disallowedIndex+1, len(args))
	disallowed := strings.Split(args[disallowedIndex+1], ",")
	for _, name := range []string{"bash", "run_terminal_cmd", "write", "edit", "search_tool", "use_tool"} {
		assert.Contains(t, disallowed, name)
	}
}

func TestPlanningRejectsDisabledCodexSandbox(t *testing.T) {
	SetCodexSandboxDisabled(true)
	t.Cleanup(func() { SetCodexSandboxDisabled(false) })
	repoPath := newPlanTestRepo(t)
	_, err := RunPlan(context.Background(), NewCodexAgent("missing-codex"), repoPath, "HEAD", "plan", io.Discard, nil)
	require.ErrorContains(t, err, "read-only sandbox")
}

func newPlanTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	planGit(t, dir, "init")
	planGit(t, dir, "config", "core.autocrlf", "false")
	planGit(t, dir, "config", "user.name", "Test User")
	planGit(t, dir, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.go -text\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "source.go"), []byte("original\n"), 0o600))
	planGit(t, dir, "add", ".gitattributes")
	planGit(t, dir, "add", "source.go")
	planGit(t, dir, "commit", "-m", "initial")
	return dir
}

func planGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestRunPlanSubmoduleConfigAndHooks(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("HOME", configDir)
	t.Setenv("USERPROFILE", configDir)
	source := newPlanTestRepo(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	t.Setenv("ROBOREV_PLAN_TEST_MARKER", filepath.ToSlash(marker))
	hook := filepath.Join(source, ".githooks", "post-checkout")
	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o755))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nprintf 'ran' > \"$ROBOREV_PLAN_TEST_MARKER\"\n"), 0o755))
	planGit(t, source, "add", "--chmod=+x", ".githooks/post-checkout")
	planGit(t, source, "commit", "-m", "Add submodule hook fixture")
	sha, err := gitrepo.Resolve(t.Context(), source, "HEAD")
	require.NoError(t, err)
	bare := filepath.Join(t.TempDir(), "sub.git")
	planGit(t, source, "clone", "--bare", source, bare)
	repoPath := newPlanTestRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, ".gitmodules"), []byte("[submodule \"vendor/sub\"]\n\tpath = vendor/sub\n\turl = file:///roborev-plan-fixture/sub.git\n"), 0o600))
	planGit(t, repoPath, "add", ".gitmodules")
	planGit(t, repoPath, "update-index", "--add", "--cacheinfo", "160000,"+sha+",vendor/sub")
	planGit(t, repoPath, "commit", "-m", "Add submodule fixture")
	global := filepath.Join(configDir, ".gitconfig")
	require.NoError(t, os.WriteFile(global, []byte("[core]\n\thooksPath = .githooks\n"), 0o600))
	planGit(t, repoPath, "config", "--file", global, "url.file://"+filepath.ToSlash(bare)+".insteadOf", "file:///roborev-plan-fixture/sub.git")
	a := &FakeAgent{NameStr: "test", ReviewFn: func(_ context.Context, path, ref, p string, _ io.Writer) (string, error) {
		currentHead, err := gitrepo.Resolve(t.Context(), path, "HEAD")
		require.NoError(t, err)
		assert.Equal(t, ref, currentHead)
		assert.Empty(t, gitrepo.CurrentBranch(t.Context(), path))
		statusCmd := exec.CommandContext(t.Context(), "git", "status", "--porcelain")
		statusCmd.Dir = path
		status, err := statusCmd.Output()
		require.NoError(t, err)
		require.Empty(t, string(status))
		body, err := os.ReadFile(filepath.Join(path, "vendor/sub/source.go"))
		require.NoError(t, err)
		assert.Equal(t, "original\n", string(body))
		return "Check the submodule", nil
	}}
	_, err = RunPlan(t.Context(), a, repoPath, "HEAD", "Analyze findings", io.Discard, nil)
	require.NoError(t, err)
	assert.NoFileExists(t, marker)
}

func TestReviewPlanningCodexConfigReloadKeepsReadOnly(t *testing.T) {
	previous := CodexSandboxDisabled()
	SetCodexSandboxDisabled(false)
	t.Cleanup(func() { SetCodexSandboxDisabled(previous) })
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	released := filepath.Join(dir, "released")
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\ncase \"$1\" in *etxtbsy*) exit 0;; esac\ncase \"$*\" in\n*--help*) touch '" + started + "'; while [ ! -f '" + released + "' ]; do sleep 0.01; done; echo '--sandbox --thread-source'; exit 0;;\nesac\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\nexit 1\n"
	command := writeTempCommand(t, script)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), planningContextKey{}, true))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := NewCodexAgent(command).Review(ctx, dir, "HEAD", "Plan only", io.Discard)
		done <- err
	}()
	// Wait for the external CLI capability-probe subprocess, then simulate configuration reload.
	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, 5*time.Second, 10*time.Millisecond)
	SetCodexSandboxDisabled(true)
	require.NoError(t, os.WriteFile(released, []byte("release"), 0o600))
	require.Error(t, <-done)
	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Contains(t, string(args), "--sandbox\nread-only\n")
	assert.NotContains(t, string(args), codexDangerousFlag, "a planning call cannot acquire unsafe permissions after a config reload")
}
