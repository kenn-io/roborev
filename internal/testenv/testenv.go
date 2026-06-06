// Package testenv provides environment isolation helpers for tests.
// This package intentionally has no dependencies on other internal packages
// to avoid import cycles.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
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

	configureGitForTests(tmpDir)

	restoreDataDir := setDataDirEnv(tmpDir)
	defer restoreDataDir()

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

func configureGitForTests(tmpDir string) {
	// Prevent global/system git config from leaking into tests.
	// Without this, commit.gpgsign=true in global config triggers
	// gpg-agent/pinentry during test commits.
	//
	// Point GIT_CONFIG_GLOBAL at an empty regular file rather than os.DevNull.
	// On Windows os.DevNull is "NUL" (the null device), and Git-for-Windows
	// fails with "fatal: unable to access 'NUL': Invalid argument" when told to
	// read its global config from that device — which broke essentially every
	// git invocation in the suite on Windows (notably Windows-on-ARM). An empty
	// file is a valid, no-op global config on every platform. On Unix
	// /dev/null happens to work, but the empty file is correct everywhere.
	emptyGlobalConfig := filepath.Join(tmpDir, "gitconfig-global")
	if f, err := os.OpenFile(emptyGlobalConfig, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_ = f.Close()
	}
	_ = os.Setenv("GIT_CONFIG_GLOBAL", emptyGlobalConfig)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Git also reads the default global ignore file from XDG_CONFIG_HOME
	// independently of GIT_CONFIG_GLOBAL. Keep that out of the user's real
	// ~/.config so personal excludes cannot change test behavior.
	xdgConfigHome := filepath.Join(tmpDir, "xdg-config")
	_ = os.MkdirAll(xdgConfigHome, 0o755)
	_ = os.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	// Never allow git to prompt for input (passwords, passphrases, etc).
	// If something unexpected tries to prompt, fail fast instead of blocking.
	_ = os.Setenv("GIT_TERMINAL_PROMPT", "0")
}
