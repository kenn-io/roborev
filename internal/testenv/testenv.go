// Package testenv provides environment isolation helpers for tests.
// This package intentionally has no dependencies on other internal packages
// to avoid import cycles.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// RunIsolatedMain provides a standardized TestMain execution wrapper that
// isolates tests from the production ~/.roborev directory. It safely manages
// environment variables and preserves the original test exit code.
func RunIsolatedMain(m *testing.M) int {
	// Snapshot prod log state BEFORE overriding ROBOREV_DATA_DIR.
	// Best-effort: if the home directory can't be resolved (e.g. CI
	// containers with no HOME), skip the barrier rather than aborting.
	var barrier *ProdLogBarrier
	if prodDataDir, err := DefaultProdDataDir(); err == nil {
		barrier = NewProdLogBarrier(prodDataDir)
	}

	tmpDir, cleanupTempDir, err := createIsolatedDataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		return 1
	}
	defer cleanupTempDir()

	restoreGit, err := configureGitForTests(tmpDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to configure git: %v\n", err)
		return 1
	}
	defer restoreGit()

	restoreDataDir := setDataDirEnv(tmpDir)
	defer restoreDataDir()

	restoreAgentConfigEnv := unsetAgentConfigDirEnv()
	defer restoreAgentConfigEnv()

	code := m.Run()

	// Hard barrier: fail if tests polluted production logs.
	if barrier != nil {
		if msg := barrier.Check(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
			if code == 0 {
				return 1
			}
		}
	}
	return code
}

// SetDataDir sets ROBOREV_DATA_DIR to a temp directory to isolate tests
// from production ~/.roborev. This is preferred over setting HOME because
// ROBOREV_DATA_DIR takes precedence in config.DataDir(). Returns the temp
// directory path. Cleanup is automatic via t.Setenv.
//
// Note: t.Setenv marks the test as incompatible with t.Parallel(), which is
// appropriate since environment variables are process-global state.
func SetDataDir(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	return tmpDir
}

func createIsolatedDataDir() (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "roborev-test-*")
	if err != nil {
		return "", nil, err
	}
	return tmpDir, func() {
		_ = os.RemoveAll(tmpDir)
	}, nil
}

func setDataDirEnv(dir string) func() {
	origEnv, hasEnv := os.LookupEnv("ROBOREV_DATA_DIR")
	_ = os.Setenv("ROBOREV_DATA_DIR", dir)
	return func() {
		if hasEnv {
			_ = os.Setenv("ROBOREV_DATA_DIR", origEnv)
			return
		}
		_ = os.Unsetenv("ROBOREV_DATA_DIR")
	}
}

// unsetAgentConfigDirEnv clears agent config-dir overrides (e.g.
// CLAUDE_CONFIG_DIR replacing ~/.claude) so tests that redirect HOME never
// resolve paths into a developer's real agent configuration.
func unsetAgentConfigDirEnv() func() {
	keys := []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "HERMES_HOME", "COPILOT_HOME", "GEMINI_CLI_HOME", "QWEN_HOME"}
	restores := make([]func(), 0, len(keys))
	for _, key := range keys {
		orig, has := os.LookupEnv(key)
		if !has {
			continue
		}
		_ = os.Unsetenv(key)
		restores = append(restores, func() {
			_ = os.Setenv(key, orig)
		})
	}
	return func() {
		for _, restore := range restores {
			restore()
		}
	}
}

// configureGitForTests restores every changed variable before fixture removal.
func configureGitForTests(tmpDir string) (restore func(), err error) {
	var restores []func()
	restore = func() {
		for _, restore := range slices.Backward(restores) {
			restore()
		}
	}
	defer func() {
		if err != nil {
			restore()
		}
	}()
	set := func(key, value string) error {
		old, exists := os.LookupEnv(key)
		if err := os.Setenv(key, value); err != nil {
			return err
		}
		restores = append(restores, func() {
			if exists {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
		return nil
	}
	// Git-for-Windows cannot read NUL as a config file. Use an empty regular
	// file on every platform, including commands whose runners strip GIT_*.
	global := filepath.Join(tmpDir, "gitconfig-global")
	if err = os.WriteFile(global, nil, 0o644); err != nil {
		return
	}
	xdg := filepath.Join(tmpDir, "xdg-config")
	if err = os.MkdirAll(xdg, 0o755); err != nil {
		return
	}
	for _, entry := range [][2]string{
		{"GIT_CONFIG_GLOBAL", global},
		{"GIT_CONFIG_NOSYSTEM", "1"},
		{"XDG_CONFIG_HOME", xdg},
		{"GIT_TERMINAL_PROMPT", "0"},
	} {
		if err = set(entry[0], entry[1]); err != nil {
			return
		}
	}
	err = installGitLauncher(tmpDir, global, set)
	return
}
