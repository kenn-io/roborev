// Package testenv provides environment isolation helpers for tests.
// This package intentionally has no dependencies on other internal packages
// to avoid import cycles.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	// Preserve Go's defaults before replacing the home and XDG config paths.
	home, _ := os.UserHomeDir()
	cache, _ := os.UserCacheDir()
	config, _ := os.UserConfigDir()
	goenv := os.Getenv("GOENV")
	if goenv == "" && config != "" {
		goenv = filepath.Join(config, "go", "env")
		if err = set("GOENV", goenv); err != nil {
			return
		}
	}
	goConfig := make(map[string]string)
	if goenv != "" && goenv != "off" {
		if data, readErr := os.ReadFile(goenv); readErr == nil {
			for line := range strings.SplitSeq(string(data), "\n") {
				if key, value, ok := strings.Cut(line, "="); ok && key != "" && key[0] >= 'A' && key[0] <= 'Z' {
					goConfig[key] = value
				}
			}
		}
	}
	for _, entry := range [][3]string{
		{"GOPATH", home, "go"},
		{"GOCACHE", cache, "go-build"},
	} {
		if entry[1] != "" && os.Getenv(entry[0]) == "" && goConfig[entry[0]] == "" {
			if err = set(entry[0], filepath.Join(entry[1], entry[2])); err != nil {
				return
			}
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
			if err = set(entry[0], filepath.Join(base, "mise")); err != nil {
				return
			}
		}
	}
	// Git-for-Windows cannot read NUL as a config file. A regular file also
	// works for runners that strip GIT_CONFIG_GLOBAL.
	global := filepath.Join(tmpDir, ".gitconfig")
	if err = os.WriteFile(global, nil, 0o644); err != nil {
		return
	}
	xdg := filepath.Join(tmpDir, "xdg-config")
	if err = os.MkdirAll(xdg, 0o755); err != nil {
		return
	}
	for _, entry := range [][2]string{
		{"HOME", tmpDir},
		{"USERPROFILE", tmpDir},
		{"GIT_CONFIG_GLOBAL", global},
		{"GIT_CONFIG_NOSYSTEM", "1"},
		{"XDG_CONFIG_HOME", xdg},
		{"GIT_TERMINAL_PROMPT", "0"},
	} {
		if err = set(entry[0], entry[1]); err != nil {
			return
		}
	}
	return
}
