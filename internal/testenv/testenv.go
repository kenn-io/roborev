// Package testenv provides environment isolation helpers for tests.
// This package intentionally has no dependencies on other internal packages
// to avoid import cycles.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		userAgentEnv = append(userAgentEnv, key+"="+orig)
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

var userEnv, userAgentEnv []string

func goFileValue(key, goenv, root string) string {
	var files []string
	if goenv != "" && goenv != "off" {
		files = append(files, goenv)
	}
	if root != "" {
		files = append(files, filepath.Join(root, "go.env"))
	}
	for _, file := range files {
		data, _ := os.ReadFile(file)
		value, found := "", false
		for line := range strings.SplitSeq(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, key+"="); ok {
				value, found = v, true
			}
		}
		if found {
			return value
		}
	}
	return ""
}

// UserEnv restores the original home and config paths for tool subprocesses.
func UserEnv() []string {
	return append(os.Environ(), userEnv...)
}

// UserAgentEnv returns the original paths needed by an authenticated adapter.
func UserAgentEnv() []string {
	return append(append([]string(nil), userEnv...), userAgentEnv...)
}

func configureGitForTests(tmpDir string) {
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		userEnv = append(userEnv, key+"="+os.Getenv(key))
	}
	home, _ := os.UserHomeDir()
	cache, _ := os.UserCacheDir()
	config, _ := os.UserConfigDir()
	goenv := os.Getenv("GOENV")
	if goenv == "" && config != "" {
		goenv = filepath.Join(config, "go", "env")
		_ = os.Setenv("GOENV", goenv)
	}
	root := goFileValue("GOROOT", goenv, "")
	if root == "" {
		root = os.Getenv("GOROOT")
	}
	if root == "" {
		// PATH may resolve a version-manager shim rather than the toolchain.
		root = runtime.GOROOT() //nolint:staticcheck
	}
	for _, entry := range [][3]string{{"GOPATH", home, "go"}, {"GOCACHE", cache, "go-build"}} {
		path := filepath.Join(entry[1], entry[2])
		if os.Getenv(entry[0]) == "" && entry[1] != "" && goFileValue(entry[0], goenv, root) == "" && (entry[0] != "GOPATH" || path != root) {
			_ = os.Setenv(entry[0], path)
		}
	}
	// Mise selects tools and trusts config from these directories before it
	// reads its own configuration. Keep their original locations across HOME.
	for _, entry := range [][3]string{
		{"MISE_CONFIG_DIR", "XDG_CONFIG_HOME", ".config"},
		{"MISE_DATA_DIR", "XDG_DATA_HOME", ".local/share"},
		{"MISE_STATE_DIR", "XDG_STATE_HOME", ".local/state"},
		{"MISE_CACHE_DIR", "XDG_CACHE_HOME", ".cache"},
	} {
		base := os.Getenv(entry[1])
		if base == "" {
			base = home
			switch {
			case runtime.GOOS == "windows" && entry[0] == "MISE_DATA_DIR":
				base = os.Getenv("LOCALAPPDATA")
			case runtime.GOOS == "windows" && entry[0] == "MISE_CACHE_DIR":
				base = os.Getenv("TEMP")
			case runtime.GOOS == "darwin" && entry[0] == "MISE_CACHE_DIR" && base != "":
				base = filepath.Join(base, "Library", "Caches")
			case base != "":
				base = filepath.Join(base, entry[2])
			}
		}
		if base != "" && os.Getenv(entry[0]) == "" {
			_ = os.Setenv(entry[0], filepath.Join(base, "mise"))
		}
	}
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
	emptyGlobalConfig := filepath.Join(tmpDir, ".gitconfig")
	if f, err := os.OpenFile(emptyGlobalConfig, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_ = f.Close()
	}
	_ = os.Setenv("GIT_CONFIG_GLOBAL", emptyGlobalConfig)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Git runners strip GIT_* variables, so the empty file must also be the
	// default global config under HOME.
	_ = os.Setenv("HOME", tmpDir)
	_ = os.Setenv("USERPROFILE", tmpDir)
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
