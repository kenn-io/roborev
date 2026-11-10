package testenv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv/gitfixture"
)

// The recorder stands in for real Git, after the native launcher has forwarded.
func gitFixtureRecorder() int {
	input, _ := io.ReadAll(os.Stdin)
	cwd, _ := os.Getwd()
	_ = json.NewEncoder(os.Stdout).Encode([]string{os.Getenv("GIT_CONFIG_GLOBAL"), os.Getenv("GIT_CONFIG_NOSYSTEM"), cwd, string(input), os.Getenv("GIT_DIR"), os.Getenv("GIT_AUTHOR_NAME"), os.Getenv("GIT_CONFIG_COUNT")})
	_ = json.NewEncoder(os.Stdout).Encode(os.Args[1:])
	fmt.Fprint(os.Stderr, "recorder stderr")
	return 23
}

func TestGitFixtureForwarding(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	launcher, err := exec.LookPath("git")
	require.NoError(t, err)
	metadata, err := gitfixture.ReadMetadata(launcher)
	require.NoError(t, err)
	metadata.RealGit = exe
	fixtureDir := t.TempDir()
	require.NoError(t, installGitLauncher(t.Context(), fixtureDir, metadata.Global, func(key, value string) error { t.Setenv(key, value); return nil }))
	launcher, err = exec.LookPath("git")
	require.NoError(t, err)
	encoded, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(launcher+gitfixture.MetadataSuffix, encoded, 0o600))
	t.Setenv("ROBOREV_TEST_FIXTURE_RECORDER", "1")
	cwd := t.TempDir()
	for _, alias := range []bool{false, true} {
		name := "direct"
		if alias {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) {
			target := launcher
			if alias {
				target = filepath.Join(t.TempDir(), filepath.Base(launcher))
				if err := os.Symlink(launcher, target); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			for _, tc := range []struct {
				name, global string
				changedHome  bool
			}{
				{name: "stripped"}, {name: "explicit", global: filepath.Join(t.TempDir(), "explicit")}, {name: "synthetic home", changedHome: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					assert := assert.New(t)
					if tc.changedHome {
						home := t.TempDir()
						t.Setenv("HOME", home)
						t.Setenv("USERPROFILE", home)
					}
					cmd := exec.CommandContext(t.Context(), target, "argument with spaces", "--literal=$value")
					// Model a runner stripping all GIT_* entries without touching fixture metadata.
					cmd.Env = withoutGitEnvironment()
					if tc.global != "" {
						cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL="+tc.global, "GIT_CONFIG_COUNT=0")
					}
					cmd.Dir = cwd
					cmd.Stdin = bytes.NewBufferString("stdin payload")
					var stdout, stderr bytes.Buffer
					cmd.Stdout, cmd.Stderr = &stdout, &stderr
					var exit *exec.ExitError
					require.ErrorAs(t, cmd.Run(), &exit)
					assert.Equal(23, exit.ExitCode())
					assert.Equal("recorder stderr", stderr.String())
					var got, args []string
					decoder := json.NewDecoder(&stdout)
					require.NoError(t, decoder.Decode(&got))
					require.NoError(t, decoder.Decode(&args))
					global := metadata.Global
					if tc.changedHome {
						global = ""
					}
					if tc.global != "" {
						global = tc.global
					}
					count := ""
					if tc.global != "" {
						count = "0"
					}
					resolved, err := filepath.EvalSymlinks(cwd)
					require.NoError(t, err)
					assert.Equal([]string{global, "1", resolved, "stdin payload", "", "", count}, got)
					assert.Equal([]string{"argument with spaces", "--literal=$value"}, args)
				})
			}
		})
	}
}

func TestGitFixtureRestoration(t *testing.T) {
	assert := assert.New(t)
	keys := []string{"PATH", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME", "GIT_TERMINAL_PROMPT"}
	type value struct {
		text   string
		exists bool
	}
	before := map[string]value{}
	for _, key := range keys {
		text, exists := os.LookupEnv(key)
		before[key] = value{text, exists}
	}
	dir, cleanup, err := createIsolatedDataDir()
	require.NoError(t, err)
	defer cleanup()
	restore, err := configureGitForTests(t.Context(), dir)
	require.NoError(t, err)
	defer restore()
	launcher, err := exec.LookPath("git")
	require.NoError(t, err)
	assert.FileExists(launcher)
	metadata, err := gitfixture.ReadMetadata(launcher)
	require.NoError(t, err)
	previousLauncher := filepath.Join(filepath.SplitList(before["PATH"].text)[0], filepath.Base(launcher))
	previous, err := gitfixture.ReadMetadata(previousLauncher)
	require.NoError(t, err)
	assert.Equal(previous.RealGit, metadata.RealGit, "nested setup retains real Git")
	restore()
	for _, key := range keys {
		text, exists := os.LookupEnv(key)
		restored := value{text, exists}
		assert.Equal(before[key], restored, key)
	}
	cleanup()
	assert.NoDirExists(dir)
}

func TestGitFixtureMalformedMetadata(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, installGitLauncher(t.Context(), dir, os.Getenv("GIT_CONFIG_GLOBAL"), func(key, value string) error { t.Setenv(key, value); return nil }))
	launcher, err := exec.LookPath("git")
	require.NoError(t, err)
	for _, data := range []string{"{", "{}"} {
		require.NoError(t, os.WriteFile(launcher+gitfixture.MetadataSuffix, []byte(data), 0o600))
		cmd := exec.CommandContext(t.Context(), launcher, "--version")
		output, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		assert.Equal(t, 1, exit.ExitCode())
		assert.Contains(t, string(output), "read fixture Git metadata:")
	}
}

// A controlled external child holds both output pipes until its stdin gate opens.
func gitFixtureWaitingChild(dir string) int {
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		return 1
	}
	var gate [1]byte
	if _, err := io.ReadFull(os.Stdin, gate[:]); err != nil {
		return 0
	}
	if err := os.WriteFile(filepath.Join(dir, "mutation"), nil, 0o600); err != nil {
		return 1
	}
	return 0
}

func TestGitFixtureCancellation(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, installGitLauncher(t.Context(), dir, os.Getenv("GIT_CONFIG_GLOBAL"), func(key, value string) error { t.Setenv(key, value); return nil }))
	launcher, err := exec.LookPath("git")
	require.NoError(t, err)
	metadata, err := gitfixture.ReadMetadata(launcher)
	require.NoError(t, err)
	metadata.RealGit, err = os.Executable()
	require.NoError(t, err)
	encoded, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(launcher+gitfixture.MetadataSuffix, encoded, 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, launcher, "fixture-waiting-child", dir)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	gate, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	waited := make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(waited)
	}()
	t.Cleanup(func() {
		// EOF also releases a child if cancellation dispatch regresses before readiness.
		_ = gate.Close()
		cancel()
		<-waited
	})
	// External process readiness and pipe closure cannot use synctest virtual time.
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "ready"))
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	cancel()
	// This watchdog detects a surviving child retaining the command's pipes.
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		require.NoError(t, context.DeadlineExceeded, "fixture Git child retained pipes after cancellation")
	}
	_, err = gate.Write([]byte{1})
	require.Error(t, err, "terminated child must close its stdin gate")
	// Give an incorrectly surviving external child an opportunity to open its gate.
	assert.Never(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "mutation"))
		return err == nil
	}, 200*time.Millisecond, 10*time.Millisecond)
}

// A fresh process must reuse the launcher without a Go tool or module CWD.
func TestGitFixtureNestedSetupWithoutGo(t *testing.T) {
	if os.Getenv("ROBOREV_TEST_NESTED_GIT_FIXTURE") == "1" {
		global := os.Getenv("GIT_CONFIG_GLOBAL")
		require.NoError(t, os.WriteFile(global, []byte("[fixture]\nmarker = nested\n"), 0o600))
		cmd := exec.CommandContext(t.Context(), "git", "config", "--global", "--get", "fixture.marker")
		cmd.Env = withoutGitEnvironment()
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		assert.Equal(t, "nested\n", string(output))
		return
	}
	launcher, err := exec.LookPath("git")
	require.NoError(t, err)
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("PATH", filepath.Dir(launcher))
	t.Setenv("ROBOREV_TEST_NESTED_GIT_FIXTURE", "1")
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestGitFixtureNestedSetupWithoutGo$")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func withoutGitEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		if len(entry) < 4 || entry[:4] != "GIT_" {
			env = append(env, entry)
		}
	}
	return env
}
