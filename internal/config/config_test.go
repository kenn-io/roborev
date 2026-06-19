package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunIsolatedMain(m))
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.Condition(t, func() bool {
		return cfg.ServerAddr == "127.0.0.1:7373"
	}, "Expected ServerAddr '127.0.0.1:7373', got '%s'", cfg.ServerAddr)
	assert.Condition(t, func() bool {
		return cfg.MaxWorkers == 4
	}, "Expected MaxWorkers 4, got %d", cfg.MaxWorkers)
	assert.Condition(t, func() bool {
		return cfg.DefaultAgent == "codex"
	}, "Expected DefaultAgent 'codex', got '%s'", cfg.DefaultAgent)
	assert.Condition(t, func() bool {
		return cfg.MouseEnabled
	}, "Expected MouseEnabled to default to true")
	assert.True(t, cfg.Agent.Codex.DisableReviewSkills, "expected Codex review skills to be disabled by default")
	assert.True(t, cfg.Agent.Codex.IgnoreReviewUserConfig, "expected Codex review user config to be ignored by default")
	assert.Equal(t, "npm:@nqbao/pi-json-schema@0.1.1", cfg.Agent.Pi.JSONSchemaExtension)
	assert.Empty(t, cfg.Cost.Endpoint)
	assert.Equal(t, "10s", cfg.Cost.Timeout)
	assert.Equal(t, 10*time.Second, cfg.Cost.ResolvedTimeout())
}

func TestDataDir(t *testing.T) {
	t.Run("default uses home directory", func(t *testing.T) {
		t.Setenv("ROBOREV_DATA_DIR", "") // DataDir() treats empty the same as unset

		dir := DataDir()
		home, _ := os.UserHomeDir()
		expected := filepath.Join(home, ".roborev")
		if dir != expected {
			assert.Condition(t, func() bool {
				return false
			}, "Expected %s, got %s", expected, dir)
		}
	})

	t.Run("env var overrides default", func(t *testing.T) {
		t.Setenv("ROBOREV_DATA_DIR", "/custom/data/dir")

		dir := DataDir()
		if dir != "/custom/data/dir" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected /custom/data/dir, got %s", dir)
		}
	})

	t.Run("GlobalConfigPath uses DataDir", func(t *testing.T) {
		testDir := filepath.Join(os.TempDir(), "roborev-test")
		t.Setenv("ROBOREV_DATA_DIR", testDir)

		path := GlobalConfigPath()
		expected := filepath.Join(testDir, "config.toml")
		if path != expected {
			assert.Condition(t, func() bool {
				return false
			}, "Expected %s, got %s", expected, path)
		}
	})
}

func TestResolveAgent(t *testing.T) {
	cfg := DefaultConfig()
	tmpDir := t.TempDir()

	// Test explicit agent takes precedence
	agent := ResolveAgent("claude-code", tmpDir, cfg)
	assert.Condition(t, func() bool {
		return agent == "claude-code"
	}, "Expected 'claude-code', got '%s'", agent)

	// Test empty explicit falls back to global config
	agent = ResolveAgent("", tmpDir, cfg)
	assert.Condition(t, func() bool {
		return agent == "codex"
	}, "Expected 'codex' (from global), got '%s'", agent)

	// Test per-repo config
	writeRepoConfigStr(t, tmpDir, `agent = "claude-code"`)

	agent = ResolveAgent("", tmpDir, cfg)
	assert.Condition(t, func() bool {
		return agent == "claude-code"
	}, "Expected 'claude-code' (from repo config), got '%s'", agent)

	// Explicit still takes precedence over repo config
	agent = ResolveAgent("codex", tmpDir, cfg)
	assert.Condition(t, func() bool {
		return agent == "codex"
	}, "Expected 'codex' (explicit), got '%s'", agent)
}

func TestSaveAndLoadGlobal(t *testing.T) {
	testenv.SetDataDir(t)

	cfg := DefaultConfig()
	cfg.DefaultAgent = "claude-code"
	cfg.MaxWorkers = 8

	err := SaveGlobal(cfg)
	require.Condition(t, func() bool {
		return err == nil
	}, "SaveGlobal failed: %v", err)

	loaded, err := LoadGlobal()
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobal failed: %v", err)
	assert.Condition(t, func() bool {
		return loaded.DefaultAgent == "claude-code"
	}, "Expected DefaultAgent 'claude-code', got '%s'", loaded.DefaultAgent)
	assert.Condition(t, func() bool {
		return loaded.MaxWorkers == 8
	}, "Expected MaxWorkers 8, got %d", loaded.MaxWorkers)
}

func TestLoadGlobalPiJSONSchemaExtension(t *testing.T) {
	testenv.SetDataDir(t)

	path := filepath.Join(DataDir(), "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`[agent.pi]
jsonschemaextension = "/opt/roborev/pi-json-schema/index.ts"
`), 0o600))

	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	assert.Equal(t, "/opt/roborev/pi-json-schema/index.ts", cfg.Agent.Pi.JSONSchemaExtension)
}

func TestLoadGlobalCostConfigFromTOML(t *testing.T) {
	testenv.SetDataDir(t)

	path := filepath.Join(DataDir(), "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`[cost]
endpoint = "https://usage.example.test/api/v1/sessions/{session_id}/usage"
timeout = "250ms"
`), 0o600))

	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	assert.Equal(t, "https://usage.example.test/api/v1/sessions/{session_id}/usage", cfg.Cost.Endpoint)
	assert.Equal(t, "250ms", cfg.Cost.Timeout)
	assert.Equal(t, 250*time.Millisecond, cfg.Cost.ResolvedTimeout())
}

func TestCostConfigResolvedTimeoutFallsBackToDefault(t *testing.T) {
	tests := []struct {
		name    string
		timeout string
	}{
		{name: "empty", timeout: ""},
		{name: "invalid", timeout: "tomorrow"},
		{name: "zero", timeout: "0"},
		{name: "negative", timeout: "-1s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := CostConfig{Timeout: tt.timeout}
			assert.Equal(t, 10*time.Second, cfg.ResolvedTimeout())
		})
	}
}

func TestSaveAndLoadGlobalAutoFilterBranch(t *testing.T) {
	testenv.SetDataDir(t)

	cfg := DefaultConfig()
	cfg.AutoFilterBranch = true
	{

		err := SaveGlobal(cfg)
		require.Condition(t, func() bool {
			return err == nil
		}, "SaveGlobal failed: %v", err)
	}

	loaded, err := LoadGlobal()
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobal failed: %v", err)
	assert.Condition(t, func() bool {
		return loaded.AutoFilterBranch
	}, "AutoFilterBranch should be true after round-trip")
}

func TestLoadGlobalAutoFilterBranchFromTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	{
		err := os.WriteFile(path, []byte("auto_filter_branch = true\n"), 0o644)
		require.Condition(t, func() bool {
			return err == nil
		}, "write config: %v", err)
	}

	cfg, err := LoadGlobalFrom(path)
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobalFrom failed: %v", err)
	assert.Condition(t, func() bool {
		return cfg.AutoFilterBranch
	}, "AutoFilterBranch should be true when loaded from TOML")
}

func TestSaveAndLoadGlobalMouseEnabled(t *testing.T) {
	testenv.SetDataDir(t)

	cfg := DefaultConfig()
	cfg.MouseEnabled = false
	{

		err := SaveGlobal(cfg)
		require.Condition(t, func() bool {
			return err == nil
		}, "SaveGlobal failed: %v", err)
	}

	loaded, err := LoadGlobal()
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobal failed: %v", err)
	assert.Condition(t, func() bool {
		return !loaded.MouseEnabled
	}, "MouseEnabled should be false after round-trip")
}

func TestLoadGlobalMouseEnabledFromTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	{
		err := os.WriteFile(path, []byte("mouse_enabled = false\n"), 0o644)
		require.Condition(t, func() bool {
			return err == nil
		}, "write config: %v", err)
	}

	cfg, err := LoadGlobalFrom(path)
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobalFrom failed: %v", err)
	assert.Condition(t, func() bool {
		return !cfg.MouseEnabled
	}, "MouseEnabled should be false when loaded from TOML")
}

func TestLoadGlobalConfigWithReviewGuidelines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	err := os.WriteFile(path, []byte(`review_guidelines = "Prefer small, focused changes."`+"\n"), 0o644)
	require.NoError(t, err)

	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	assert.Equal(t, "Prefer small, focused changes.", cfg.ReviewGuidelines)
}

func TestLoadRepoConfigWithGuidelines(t *testing.T) {
	tmpDir := newTempRepo(t, `
agent = "claude-code"
review_guidelines = """
We are not doing database migrations because there are no production databases yet.
Prefer composition over inheritance.
All public APIs must have documentation comments.
"""
`)

	cfg, err := LoadRepoConfig(tmpDir)
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadRepoConfig failed: %v", err)
	require.Condition(t, func() bool {
		return cfg != nil
	}, "Expected non-nil config")
	assert.Condition(t, func() bool {
		return cfg.Agent == "claude-code"
	}, "Expected agent 'claude-code', got '%s'", cfg.Agent)
	assert.Condition(t, func() bool {
		return strings.Contains(cfg.ReviewGuidelines, "database migrations")
	}, "Expected guidelines to contain 'database migrations', got '%s'", cfg.ReviewGuidelines)
	assert.Condition(t, func() bool {
		return strings.Contains(cfg.ReviewGuidelines, "composition over inheritance")
	}, "Expected guidelines to contain 'composition over inheritance'")
}

func TestLoadRepoConfigWithGuidelinesSupersedeGlobal(t *testing.T) {
	tmpDir := newTempRepo(t, `
review_guidelines = "Repo-only rule."
review_guidelines_supersede_global = true
`)

	cfg, err := LoadRepoConfig(tmpDir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "Repo-only rule.", cfg.ReviewGuidelines)
	assert.True(t, cfg.ReviewGuidelinesSupersedeGlobal)
}

func TestLoadRepoConfigNoGuidelines(t *testing.T) {
	tmpDir := newTempRepo(t, `agent = "codex"`)

	cfg, err := LoadRepoConfig(tmpDir)
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadRepoConfig failed: %v", err)
	require.Condition(t, func() bool {
		return cfg != nil
	}, "Expected non-nil config")
	assert.Condition(t, func() bool {
		return cfg.ReviewGuidelines == ""
	}, "Expected empty guidelines, got '%s'", cfg.ReviewGuidelines)
}

func TestLoadRepoConfigMissing(t *testing.T) {
	tmpDir := t.TempDir()

	// Test loading from directory with no config file
	cfg, err := LoadRepoConfig(tmpDir)
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadRepoConfig failed: %v", err)
	assert.Condition(t, func() bool {
		return cfg == nil
	}, "Expected nil config when file doesn't exist")
}

// TestLoadRepoConfigWorktreeFallback verifies that a .roborev.toml living only
// in the main checkout (e.g. gitignored, so absent from a linked worktree's
// working tree) is still loaded when LoadRepoConfig is called against the
// worktree directory.
func TestLoadRepoConfigWorktreeFallback(t *testing.T) {
	main := t.TempDir()
	execGit(t, main, "init")
	execGit(t, main, "config", "user.email", "t@example.com")
	execGit(t, main, "config", "user.name", "t")
	writeTestFile(t, main, "base.txt", "base\n")
	execGit(t, main, "add", ".")
	execGit(t, main, "commit", "-m", "init")

	// Config exists only in the main checkout and is gitignored, so it is
	// untracked and will not appear in a worktree's working tree.
	writeTestFile(t, main, ".gitignore", ".roborev.toml\n")
	writeRepoConfigStr(t, main, `agent = "claude-code"`)

	wt := filepath.Join(t.TempDir(), "wt")
	execGit(t, main, "worktree", "add", wt, "HEAD")

	// Sanity: the worktree really lacks its own config.
	_, statErr := os.Stat(filepath.Join(wt, ".roborev.toml"))
	require.True(t, os.IsNotExist(statErr), "worktree should not contain .roborev.toml")

	cfg, err := LoadRepoConfig(wt)
	require.NoError(t, err)
	require.NotNil(t, cfg, "expected fallback to main checkout config")
	assert.Equal(t, "claude-code", cfg.Agent)
}

// TestLoadRepoConfigWorktreeNoFallbackForTracked verifies that a tracked
// .roborev.toml in the main checkout is NOT inherited by a worktree on a
// branch that lacks it. A tracked file is branch-specific, so the worktree's
// own absence must win.
func TestLoadRepoConfigWorktreeNoFallbackForTracked(t *testing.T) {
	main := t.TempDir()
	execGit(t, main, "init")
	execGit(t, main, "config", "user.email", "t@example.com")
	execGit(t, main, "config", "user.name", "t")
	writeTestFile(t, main, "base.txt", "base\n")
	execGit(t, main, "add", ".")
	execGit(t, main, "commit", "-m", "init")

	// Create the worktree from a commit that has no .roborev.toml, then add a
	// tracked .roborev.toml only on the main checkout's branch.
	wt := filepath.Join(t.TempDir(), "wt")
	execGit(t, main, "worktree", "add", "-b", "feature", wt, "HEAD")

	writeRepoConfigStr(t, main, `agent = "claude-code"`)
	execGit(t, main, "add", ".roborev.toml")
	execGit(t, main, "commit", "-m", "add config on main")

	// The worktree branch never received the tracked file, so it must not
	// inherit the main checkout's branch copy.
	_, statErr := os.Stat(filepath.Join(wt, ".roborev.toml"))
	require.True(t, os.IsNotExist(statErr), "worktree should not contain .roborev.toml")

	cfg, err := LoadRepoConfig(wt)
	require.NoError(t, err)
	assert.Nil(t, cfg, "tracked main config must not leak into a worktree on a branch that lacks it")
}

// TestLoadRepoConfigWorktreeLocalWins verifies that a worktree's own
// .roborev.toml takes precedence over the main checkout's config.
func TestLoadRepoConfigWorktreeLocalWins(t *testing.T) {
	main := t.TempDir()
	execGit(t, main, "init")
	execGit(t, main, "config", "user.email", "t@example.com")
	execGit(t, main, "config", "user.name", "t")
	writeTestFile(t, main, "base.txt", "base\n")
	execGit(t, main, "add", ".")
	execGit(t, main, "commit", "-m", "init")

	writeTestFile(t, main, ".gitignore", ".roborev.toml\n")
	writeRepoConfigStr(t, main, `agent = "codex"`)

	wt := filepath.Join(t.TempDir(), "wt")
	execGit(t, main, "worktree", "add", wt, "HEAD")
	writeRepoConfigStr(t, wt, `agent = "gemini"`)

	cfg, err := LoadRepoConfig(wt)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "gemini", cfg.Agent, "worktree-local config should win over main")
}

// TestLoadRepoConfigSymlink verifies that a .roborev.toml that is a symlink to
// a config file elsewhere is followed and loaded.
func TestLoadRepoConfigSymlink(t *testing.T) {
	target := t.TempDir()
	writeTestFile(t, target, "shared.toml", `agent = "copilot"`)

	repo := t.TempDir()
	require.NoError(t, os.Symlink(
		filepath.Join(target, "shared.toml"),
		filepath.Join(repo, ".roborev.toml"),
	))

	cfg, err := LoadRepoConfig(repo)
	require.NoError(t, err)
	require.NotNil(t, cfg, "expected symlinked config to be loaded")
	assert.Equal(t, "copilot", cfg.Agent)
}

// TestLoadRawRepoWorktreeFallback verifies that raw-config loading (used for
// explicit-key detection) applies the same main-checkout fallback as
// LoadRepoConfig when .roborev.toml exists only in the main checkout.
func TestLoadRawRepoWorktreeFallback(t *testing.T) {
	main := t.TempDir()
	execGit(t, main, "init")
	execGit(t, main, "config", "user.email", "t@example.com")
	execGit(t, main, "config", "user.name", "t")
	writeTestFile(t, main, "base.txt", "base\n")
	execGit(t, main, "add", ".")
	execGit(t, main, "commit", "-m", "init")

	writeTestFile(t, main, ".gitignore", ".roborev.toml\n")
	writeRepoConfigStr(t, main, "reuse_review_session_lookback = 5\n")

	wt := filepath.Join(t.TempDir(), "wt")
	execGit(t, main, "worktree", "add", wt, "HEAD")

	raw, err := LoadRawRepo(wt)
	require.NoError(t, err)
	require.NotNil(t, raw, "expected raw config to fall back to main checkout")
	assert.True(t, IsKeyInTOMLFile(raw, "reuse_review_session_lookback"),
		"explicit key should be detected via the main-checkout fallback")
}

func TestResolveJobTimeout(t *testing.T) {
	tests := []struct {
		name         string
		repoConfig   string
		globalConfig *Config
		want         int
	}{
		{
			name: "default when no config",
			want: 30,
		},
		{
			name:         "default when global config has zero",
			globalConfig: &Config{JobTimeoutMinutes: 0},
			want:         30,
		},
		{
			name:         "negative global config falls through to default",
			globalConfig: &Config{JobTimeoutMinutes: -10},
			want:         30,
		},
		{
			name:         "global config takes precedence over default",
			globalConfig: &Config{JobTimeoutMinutes: 45},
			want:         45,
		},
		{
			name:         "repo config takes precedence over global",
			repoConfig:   `job_timeout_minutes = 15`,
			globalConfig: &Config{JobTimeoutMinutes: 45},
			want:         15,
		},
		{
			name:         "repo config zero falls through to global",
			repoConfig:   `job_timeout_minutes = 0`,
			globalConfig: &Config{JobTimeoutMinutes: 45},
			want:         45,
		},
		{
			name:         "repo config negative falls through to global",
			repoConfig:   `job_timeout_minutes = -5`,
			globalConfig: &Config{JobTimeoutMinutes: 45},
			want:         45,
		},
		{
			name:         "repo config without timeout falls through to global",
			repoConfig:   `agent = "codex"`,
			globalConfig: &Config{JobTimeoutMinutes: 60},
			want:         60,
		},
		{
			name:         "malformed repo config falls through to global",
			repoConfig:   `this is not valid toml {{{`,
			globalConfig: &Config{JobTimeoutMinutes: 45},
			want:         45,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := newTempRepo(t, tt.repoConfig)
			got := ResolveJobTimeout(tmpDir, tt.globalConfig)
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "ResolveJobTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveAutoClosePassingReviews(t *testing.T) {
	tests := []struct {
		name         string
		repoConfig   string
		globalConfig *Config
		want         bool
	}{
		{
			name: "default false",
			want: false,
		},
		{
			name:         "global enabled",
			globalConfig: &Config{AutoClosePassingReviews: true},
			want:         true,
		},
		{
			name:         "repo overrides global to true",
			repoConfig:   `auto_close_passing_reviews = true`,
			globalConfig: &Config{AutoClosePassingReviews: false},
			want:         true,
		},
		{
			name:         "repo overrides global to false",
			repoConfig:   `auto_close_passing_reviews = false`,
			globalConfig: &Config{AutoClosePassingReviews: true},
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := newTempRepo(t, tt.repoConfig)
			got := ResolveAutoClosePassingReviews(tmpDir, tt.globalConfig)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveShowClassifyJobs(t *testing.T) {
	tests := []struct {
		name         string
		repoConfig   string
		globalConfig *Config
		want         bool
	}{
		{
			name: "default false",
			want: false,
		},
		{
			name:         "global enabled",
			globalConfig: &Config{ShowClassifyJobs: true},
			want:         true,
		},
		{
			name:         "repo overrides global to true",
			repoConfig:   `show_classify_jobs = true`,
			globalConfig: &Config{ShowClassifyJobs: false},
			want:         true,
		},
		{
			name:         "repo overrides global to false",
			repoConfig:   `show_classify_jobs = false`,
			globalConfig: &Config{ShowClassifyJobs: true},
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := newTempRepo(t, tt.repoConfig)
			got := ResolveShowClassifyJobs(tmpDir, tt.globalConfig)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveReasoning(t *testing.T) {
	type resolverFunc func(explicit string, dir string, globalCfg *Config) (string, error)

	runTests := func(t *testing.T, name string, fn resolverFunc, configKey, defaultVal, repoVal string) {
		t.Run(name, func(t *testing.T) {
			tests := []struct {
				testName     string
				explicit     string
				repoConfig   string
				globalConfig string
				want         string
				wantErr      bool
			}{
				{"default when no config", "", "", "", defaultVal, false},
				{"global config when explicit and repo empty", "", "", fmt.Sprintf(`%s = "%s"`, configKey, repoVal), repoVal, false},
				{"repo config when explicit empty", "", fmt.Sprintf(`%s = "%s"`, configKey, repoVal), "", repoVal, false},
				{"repo config overrides global config", "", fmt.Sprintf(`%s = "%s"`, configKey, repoVal), fmt.Sprintf(`%s = "%s"`, configKey, defaultVal), repoVal, false},
				{"explicit overrides repo config", "fast", fmt.Sprintf(`%s = "%s"`, configKey, repoVal), fmt.Sprintf(`%s = "%s"`, configKey, defaultVal), "fast", false},
				{"explicit normalization", "FAST", "", "", "fast", false},
				{"explicit with valid repo config but no reasoning override", "fast", "# valid config", "", "fast", false},
				{"explicit bypasses malformed repo config", "fast", fmt.Sprintf(`%s = [`, configKey), "", "fast", false},
				{"explicit does not bypass invalid repo config", "fast", fmt.Sprintf(`%s = "invalid"`, configKey), "", "", true},
				{"invalid explicit", "unknown", "", "", "", true},
				{"invalid repo config", "", fmt.Sprintf(`%s = "invalid"`, configKey), "", "", true},
				{"malformed repo config does not fall back to global", "", fmt.Sprintf(`%s = [`, configKey), fmt.Sprintf(`%s = "%s"`, configKey, repoVal), "", true},
				{"invalid global config", "", "", fmt.Sprintf(`%s = "invalid"`, configKey), "", true},
			}

			for _, tt := range tests {
				t.Run(tt.testName, func(t *testing.T) {
					dataDir := t.TempDir()
					t.Setenv("ROBOREV_DATA_DIR", dataDir)
					if tt.globalConfig != "" {
						err := os.WriteFile(
							filepath.Join(dataDir, "config.toml"),
							[]byte(tt.globalConfig),
							0o644,
						)
						require.NoError(t, err)
					}
					var globalCfg *Config
					if tt.globalConfig != "" {
						cfg, loadErr := LoadGlobalFrom(filepath.Join(dataDir, "config.toml"))
						require.NoError(t, loadErr)
						globalCfg = cfg
					}
					tmpDir := newTempRepo(t, tt.repoConfig)
					got, err := fn(tt.explicit, tmpDir, globalCfg)
					if (err != nil) != tt.wantErr {
						assert.Condition(t, func() bool {
							return false
						}, "error = %v, wantErr %v", err, tt.wantErr)
					}
					if !tt.wantErr && got != tt.want {
						assert.Condition(t, func() bool {
							return false
						}, "got %q, want %q", got, tt.want)
					}
				})
			}
		})
	}

	runTests(t, "Review", ResolveReviewReasoning, "review_reasoning", "thorough", "standard")
	runTests(t, "Refine", ResolveRefineReasoning, "refine_reasoning", "standard", "thorough")
	runTests(t, "Fix", ResolveFixReasoning, "fix_reasoning", "standard", "thorough")
}

func TestFixEmptyReasoningSelectsStandardAgent(t *testing.T) {
	// End-to-end: empty --reasoning resolves to "standard" via ResolveFixReasoning,
	// then ResolveAgentForWorkflow selects fix_agent_standard over fix_agent.
	tmpDir := t.TempDir()
	writeRepoConfig(t, tmpDir, M{
		"fix_agent":          "codex",
		"fix_agent_standard": "claude",
		"fix_agent_fast":     "gemini",
	})

	reasoning, err := ResolveFixReasoning("", tmpDir, nil)
	require.Condition(t, func() bool {
		return err == nil
	}, "ResolveFixReasoning: %v", err)
	require.Condition(t, func() bool {
		return reasoning == "standard"
	}, "expected default reasoning 'standard', got %q", reasoning)

	agent := ResolveAgentForWorkflow("", tmpDir, nil, "fix", reasoning)
	assert.Condition(t, func() bool {
		return agent == "claude"
	}, "expected fix_agent_standard 'claude', got %q", agent)

	model := ResolveModelForWorkflow("", tmpDir, nil, "fix", reasoning)
	assert.Condition(t, func() bool {
		return model == ""
	}, "expected empty model (none configured), got %q", model)
}

func TestIsBranchExcluded(t *testing.T) {
	tests := []struct {
		name       string
		repoConfig string
		branch     string
		want       bool
	}{
		{
			name:   "no config file",
			branch: "main",
			want:   false,
		},
		{
			name:       "empty excluded_branches",
			repoConfig: `agent = "codex"`,
			branch:     "main",
			want:       false,
		},
		{
			name:       "branch is excluded (wip)",
			repoConfig: `excluded_branches = ["wip", "scratch", "test-branch"]`,
			branch:     "wip",
			want:       true,
		},
		{
			name:       "branch is excluded (scratch)",
			repoConfig: `excluded_branches = ["wip", "scratch", "test-branch"]`,
			branch:     "scratch",
			want:       true,
		},
		{
			name:       "branch is excluded (test-branch)",
			repoConfig: `excluded_branches = ["wip", "scratch", "test-branch"]`,
			branch:     "test-branch",
			want:       true,
		},
		{
			name:       "branch is not excluded",
			repoConfig: `excluded_branches = ["wip", "scratch"]`,
			branch:     "main",
			want:       false,
		},
		{
			name:       "branch is not excluded (feature/foo)",
			repoConfig: `excluded_branches = ["wip", "scratch"]`,
			branch:     "feature/foo",
			want:       false,
		},
		{
			name:       "exact match required (prefix mismatch)",
			repoConfig: `excluded_branches = ["wip"]`,
			branch:     "wip-feature",
			want:       false,
		},
		{
			name:       "exact match required (suffix mismatch)",
			repoConfig: `excluded_branches = ["wip"]`,
			branch:     "my-wip",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := newTempRepo(t, tt.repoConfig)
			// For "no config file", we just don't write anything.

			got := IsBranchExcluded(tmpDir, tt.branch)
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "IsBranchExcluded(%q) = %v, want %v", tt.branch, got, tt.want)
			}
		})
	}
}

func TestResolveExcludePatterns(t *testing.T) {
	t.Run("no config", func(t *testing.T) {
		tmpDir := t.TempDir()
		got := ResolveExcludePatterns(t.Context(), tmpDir, nil, "")
		assert.Nil(t, got)
	})

	t.Run("repo only", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := os.WriteFile(filepath.Join(tmpDir, ".roborev.toml"),
			[]byte(`exclude_patterns = ["custom.lock", "*.min.js"]`), 0o644)
		require.NoError(t, err)
		got := ResolveExcludePatterns(t.Context(), tmpDir, nil, "")
		assert.Equal(t, []string{"custom.lock", "*.min.js"}, got)
	})

	t.Run("global only", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfg := &Config{ExcludePatterns: []string{"global.lock"}}
		got := ResolveExcludePatterns(t.Context(), tmpDir, cfg, "")
		assert.Equal(t, []string{"global.lock"}, got)
	})

	t.Run("repo and global merged", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := os.WriteFile(filepath.Join(tmpDir, ".roborev.toml"),
			[]byte(`exclude_patterns = ["repo.lock"]`), 0o644)
		require.NoError(t, err)
		cfg := &Config{
			ExcludePatterns: []string{"global.lock", "repo.lock"},
		}
		got := ResolveExcludePatterns(t.Context(), tmpDir, cfg, "")
		// repo.lock appears only once; repo patterns listed first
		assert.Equal(t, []string{"repo.lock", "global.lock"}, got)
	})

	t.Run("whitespace-only patterns skipped", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfg := &Config{ExcludePatterns: []string{" ", "keep"}}
		got := ResolveExcludePatterns(t.Context(), tmpDir, cfg, "")
		assert.Equal(t, []string{"keep"}, got)
	})

	t.Run("security reviews skip repo patterns", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := os.WriteFile(filepath.Join(tmpDir, ".roborev.toml"),
			[]byte(`exclude_patterns = ["repo.lock"]`), 0o644)
		require.NoError(t, err)
		cfg := &Config{
			ExcludePatterns: []string{"global.lock"},
		}
		got := ResolveExcludePatterns(t.Context(), tmpDir, cfg, "security")
		// Only global patterns; repo patterns skipped
		assert.Equal(t, []string{"global.lock"}, got)
	})
}

func TestResolveExcludePatternsLocal(t *testing.T) {
	t.Run("reads working-tree config", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := os.WriteFile(filepath.Join(tmpDir, ".roborev.toml"),
			[]byte(`exclude_patterns = ["local.dat"]`), 0o644)
		require.NoError(t, err)
		got := ResolveExcludePatternsLocal(tmpDir, nil, "")
		assert.Equal(t, []string{"local.dat"}, got)
	})

	t.Run("merges with global", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := os.WriteFile(filepath.Join(tmpDir, ".roborev.toml"),
			[]byte(`exclude_patterns = ["local.dat"]`), 0o644)
		require.NoError(t, err)
		cfg := &Config{ExcludePatterns: []string{"global.lock"}}
		got := ResolveExcludePatternsLocal(tmpDir, cfg, "")
		assert.Equal(t, []string{"local.dat", "global.lock"}, got)
	})

	t.Run("security skips repo patterns", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := os.WriteFile(filepath.Join(tmpDir, ".roborev.toml"),
			[]byte(`exclude_patterns = ["local.dat"]`), 0o644)
		require.NoError(t, err)
		cfg := &Config{ExcludePatterns: []string{"global.lock"}}
		got := ResolveExcludePatternsLocal(tmpDir, cfg, "security")
		assert.Equal(t, []string{"global.lock"}, got)
	})
}

func TestIsCommitMessageExcluded(t *testing.T) {
	tests := []struct {
		name       string
		repoConfig string
		message    string
		want       bool
	}{
		{
			name:    "no config file",
			message: "fix: update handler",
			want:    false,
		},
		{
			name:       "empty excluded_commit_patterns",
			repoConfig: `agent = "codex"`,
			message:    "fix: update handler",
			want:       false,
		},
		{
			name:       "message matches pattern",
			repoConfig: `excluded_commit_patterns = ["[skip review]"]`,
			message:    "wip: quick fix [skip review]",
			want:       true,
		},
		{
			name:       "message matches one of several patterns",
			repoConfig: `excluded_commit_patterns = ["[skip review]", "[wip]", "[no review]"]`,
			message:    "checkpoint [wip]",
			want:       true,
		},
		{
			name:       "message does not match",
			repoConfig: `excluded_commit_patterns = ["[skip review]", "[wip]"]`,
			message:    "feat: add new endpoint",
			want:       false,
		},
		{
			name:       "case insensitive match",
			repoConfig: `excluded_commit_patterns = ["[Skip Review]"]`,
			message:    "wip: quick fix [SKIP REVIEW]",
			want:       true,
		},
		{
			name:       "pattern in body not just subject",
			repoConfig: `excluded_commit_patterns = ["[skip review]"]`,
			message:    "feat: add feature\n\nsome details [skip review]",
			want:       true,
		},
		{
			name:       "empty pattern is ignored",
			repoConfig: `excluded_commit_patterns = [""]`,
			message:    "any commit message",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := newTempRepo(t, tt.repoConfig)
			got := IsCommitMessageExcluded(tmpDir, tt.message)
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "IsCommitMessageExcluded(%q) = %v, want %v", tt.message, got, tt.want)
			}
		})
	}
}

func TestAllCommitMessagesExcluded(t *testing.T) {
	tests := []struct {
		name       string
		repoConfig string
		messages   []string
		want       bool
	}{
		{
			name:     "empty messages returns false",
			messages: nil,
			want:     false,
		},
		{
			name:       "all match",
			repoConfig: `excluded_commit_patterns = ["[wip]"]`,
			messages: []string{
				"[wip] checkpoint 1",
				"[wip] checkpoint 2",
			},
			want: true,
		},
		{
			name:       "one does not match",
			repoConfig: `excluded_commit_patterns = ["[wip]"]`,
			messages: []string{
				"[wip] checkpoint",
				"feat: real work",
			},
			want: false,
		},
		{
			name:     "no config file",
			messages: []string{"[wip] anything"},
			want:     false,
		},
		{
			name:       "no patterns configured",
			repoConfig: `agent = "codex"`,
			messages:   []string{"[wip] anything"},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := newTempRepo(t, tt.repoConfig)
			got := AllCommitMessagesExcluded(tmpDir, tt.messages)
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "AllCommitMessagesExcluded() = %v, want %v",
					got, tt.want)
			}
		})
	}
}

func TestSyncConfigPostgresURLExpanded(t *testing.T) {
	t.Run("empty URL returns empty", func(t *testing.T) {
		cfg := SyncConfig{}
		if got := cfg.PostgresURLExpanded(); got != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected empty string, got %q", got)
		}
	})

	t.Run("URL without env vars unchanged", func(t *testing.T) {
		cfg := SyncConfig{PostgresURL: "postgres://user:pass@localhost:5432/db"}
		if got := cfg.PostgresURLExpanded(); got != cfg.PostgresURL {
			assert.Condition(t, func() bool {
				return false
			}, "Expected %q, got %q", cfg.PostgresURL, got)
		}
	})

	t.Run("URL with env var is expanded", func(t *testing.T) {
		t.Setenv("TEST_PG_PASS", "secret123")

		cfg := SyncConfig{PostgresURL: "postgres://user:${TEST_PG_PASS}@localhost:5432/db"}
		expected := "postgres://user:secret123@localhost:5432/db"
		if got := cfg.PostgresURLExpanded(); got != expected {
			assert.Condition(t, func() bool {
				return false
			}, "Expected %q, got %q", expected, got)
		}
	})

	t.Run("missing env var becomes empty", func(t *testing.T) {
		t.Setenv("NONEXISTENT_VAR", "")
		cfg := SyncConfig{PostgresURL: "postgres://user:${NONEXISTENT_VAR}@localhost:5432/db"}
		expected := "postgres://user:@localhost:5432/db"
		if got := cfg.PostgresURLExpanded(); got != expected {
			assert.Condition(t, func() bool {
				return false
			}, "Expected %q, got %q", expected, got)
		}
	})
}

func TestSyncConfigGetRepoDisplayName(t *testing.T) {
	t.Run("nil receiver returns empty", func(t *testing.T) {
		var cfg *SyncConfig
		if got := cfg.GetRepoDisplayName("any"); got != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected empty string for nil receiver, got %q", got)
		}
	})

	t.Run("nil map returns empty", func(t *testing.T) {
		cfg := &SyncConfig{}
		if got := cfg.GetRepoDisplayName("any"); got != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected empty string for nil map, got %q", got)
		}
	})

	t.Run("missing key returns empty", func(t *testing.T) {
		cfg := &SyncConfig{
			RepoNames: map[string]string{
				"git@github.com:org/repo.git": "my-repo",
			},
		}
		if got := cfg.GetRepoDisplayName("unknown"); got != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected empty string for missing key, got %q", got)
		}
	})

	t.Run("returns configured name", func(t *testing.T) {
		cfg := &SyncConfig{
			RepoNames: map[string]string{
				"git@github.com:org/repo.git": "my-custom-name",
			},
		}
		expected := "my-custom-name"
		if got := cfg.GetRepoDisplayName("git@github.com:org/repo.git"); got != expected {
			assert.Condition(t, func() bool {
				return false
			}, "Expected %q, got %q", expected, got)
		}
	})
}

func TestSyncConfigValidate(t *testing.T) {
	t.Run("disabled returns no warnings", func(t *testing.T) {
		cfg := SyncConfig{Enabled: false}
		warnings := cfg.Validate()
		if len(warnings) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected no warnings when disabled, got %v", warnings)
		}
	})

	t.Run("enabled without URL warns", func(t *testing.T) {
		cfg := SyncConfig{Enabled: true, PostgresURL: ""}
		warnings := cfg.Validate()
		if len(warnings) != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 1 warning, got %d", len(warnings))
		}
		if !strings.Contains(warnings[0], "postgres_url is not set") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected warning about missing URL, got %q", warnings[0])
		}
	})

	t.Run("valid config no warnings", func(t *testing.T) {
		cfg := SyncConfig{
			Enabled:     true,
			PostgresURL: "postgres://user:pass@localhost:5432/db",
		}
		warnings := cfg.Validate()
		if len(warnings) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected no warnings for valid config, got %v", warnings)
		}
	})

	t.Run("unexpanded env var warns", func(t *testing.T) {
		t.Setenv("MISSING_VAR", "")
		cfg := SyncConfig{
			Enabled:     true,
			PostgresURL: "postgres://user:${MISSING_VAR}@localhost:5432/db",
		}
		warnings := cfg.Validate()
		if len(warnings) != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 1 warning for unexpanded var, got %d: %v", len(warnings), warnings)
		}
		if !strings.Contains(warnings[0], "unexpanded") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected warning about unexpanded vars, got %q", warnings[0])
		}
	})

	t.Run("expanded env var no warning", func(t *testing.T) {
		t.Setenv("TEST_PG_PASS2", "secret")

		cfg := SyncConfig{
			Enabled:     true,
			PostgresURL: "postgres://user:${TEST_PG_PASS2}@localhost:5432/db",
		}
		warnings := cfg.Validate()
		if len(warnings) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected no warnings when env var is set, got %v", warnings)
		}
	})
}

func TestLoadGlobalWithSyncConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	{
		err := os.WriteFile(configPath, []byte(`
default_agent = "codex"

[sync]
enabled = true
postgres_url = "postgres://roborev:pass@localhost:5432/roborev"
interval = "10m"
machine_name = "test-machine"
connect_timeout = "10s"
`), 0o644)
		require.Condition(t, func() bool {
			return err == nil
		}, "Failed to write config: %v", err)
	}

	cfg, err := LoadGlobalFrom(configPath)
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobalFrom failed: %v", err)
	assert.Condition(t, func() bool {
		return cfg.Sync.Enabled
	}, "Expected Sync.Enabled to be true")
	assert.Condition(t, func() bool {
		return cfg.Sync.PostgresURL == "postgres://roborev:pass@localhost:5432/roborev"
	}, "Unexpected PostgresURL: %s", cfg.Sync.PostgresURL)
	assert.Condition(t, func() bool {
		return cfg.Sync.Interval == "10m"
	}, "Expected Interval '10m', got '%s'", cfg.Sync.Interval)
	assert.Condition(t, func() bool {
		return cfg.Sync.MachineName == "test-machine"
	}, "Expected MachineName 'test-machine', got '%s'", cfg.Sync.MachineName)
	assert.Condition(t, func() bool {
		return cfg.Sync.ConnectTimeout == "10s"
	}, "Expected ConnectTimeout '10s', got '%s'", cfg.Sync.ConnectTimeout)
}

func TestGetDisplayName(t *testing.T) {
	t.Run("no config file", func(t *testing.T) {
		tmpDir := t.TempDir()
		name := GetDisplayName(tmpDir)
		if name != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected empty display name when no config file, got '%s'", name)
		}
	})

	t.Run("display_name not set", func(t *testing.T) {
		tmpDir := newTempRepo(t, `agent = "codex"`)
		name := GetDisplayName(tmpDir)
		if name != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected empty display name when not set, got '%s'", name)
		}
	})

	t.Run("display_name is set", func(t *testing.T) {
		tmpDir := newTempRepo(t, `display_name = "My Cool Project"`)
		name := GetDisplayName(tmpDir)
		if name != "My Cool Project" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected display name 'My Cool Project', got '%s'", name)
		}
	})

	t.Run("display_name with other config", func(t *testing.T) {
		tmpDir := newTempRepo(t, `
agent = "claude-code"
display_name = "Backend Service"
excluded_branches = ["wip"]
`)
		name := GetDisplayName(tmpDir)
		if name != "Backend Service" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected display name 'Backend Service', got '%s'", name)
		}
	})
}

func TestValidateRoborevID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"valid simple", "my-project", false},
		{"valid with dots", "my.project.name", false},
		{"valid with underscores", "my_project_name", false},
		{"valid with colons", "org:my-project", false},
		{"valid with slashes", "org/repo/name", false},
		{"valid with at", "user@host", false},
		{"valid URL-like", "github.com/user/repo", false},
		{"valid numeric start", "123project", false},
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"starts with dot", ".hidden", true},
		{"starts with dash", "-invalid", true},
		{"starts with underscore", "_invalid", true},
		{"contains spaces", "my project", true},
		{"contains newline", "my\nproject", true},
		{"too long", strings.Repeat("a", 257), true},
		{"max length", strings.Repeat("a", 256), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errMsg := ValidateRoborevID(tt.id)
			gotErr := errMsg != ""
			if gotErr != tt.wantErr {
				assert.Condition(t, func() bool {
					return false
				}, "ValidateRoborevID(%q) error = %q, wantErr = %v", tt.id, errMsg, tt.wantErr)
			}
		})
	}
}

func TestReadRoborevID(t *testing.T) {
	t.Run("file does not exist", func(t *testing.T) {
		tmpDir := t.TempDir()
		id, err := ReadRoborevID(tmpDir)
		if err != nil {
			assert.Condition(t, func() bool {
				return false
			}, "Expected no error for missing file, got: %v", err)
		}
		if id != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected empty ID for missing file, got: %q", id)
		}
	})

	t.Run("valid file", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmpDir, ".roborev-id"), []byte("my-project\n"), 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}
		id, err := ReadRoborevID(tmpDir)
		if err != nil {
			assert.Condition(t, func() bool {
				return false
			}, "Unexpected error: %v", err)
		}
		if id != "my-project" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 'my-project', got: %q", id)
		}
	})

	t.Run("valid file with whitespace", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmpDir, ".roborev-id"), []byte("  my-project  \n\n"), 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}
		id, err := ReadRoborevID(tmpDir)
		if err != nil {
			assert.Condition(t, func() bool {
				return false
			}, "Unexpected error: %v", err)
		}
		if id != "my-project" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 'my-project', got: %q", id)
		}
	})

	t.Run("invalid file content", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmpDir, ".roborev-id"), []byte(".invalid-start"), 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}
		id, err := ReadRoborevID(tmpDir)
		if err == nil {
			assert.Condition(t, func() bool {
				return false
			}, "Expected error for invalid content")
		}
		if id != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected empty ID on error, got: %q", id)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmpDir, ".roborev-id"), []byte(""), 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}
		id, err := ReadRoborevID(tmpDir)
		if err == nil {
			assert.Condition(t, func() bool {
				return false
			}, "Expected error for empty file")
		}
		if id != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected empty ID on error, got: %q", id)
		}
	})
}

func TestResolveRepoIdentity(t *testing.T) {
	t.Run("uses roborev-id when present", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmpDir, ".roborev-id"), []byte("my-custom-id"), 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}

		mockRemote := func(repoPath, remoteName string) string {
			return "https://github.com/user/repo.git"
		}

		id := ResolveRepoIdentity(tmpDir, mockRemote)
		if id != "my-custom-id" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 'my-custom-id', got: %q", id)
		}
	})

	t.Run("falls back to git remote when no roborev-id", func(t *testing.T) {
		tmpDir := t.TempDir()

		mockRemote := func(repoPath, remoteName string) string {
			return "https://github.com/user/repo.git"
		}

		id := ResolveRepoIdentity(tmpDir, mockRemote)
		if id != "https://github.com/user/repo.git" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected git remote URL, got: %q", id)
		}
	})

	t.Run("falls back to local path when no remote", func(t *testing.T) {
		tmpDir := t.TempDir()

		mockRemote := func(repoPath, remoteName string) string {
			return ""
		}

		id := ResolveRepoIdentity(tmpDir, mockRemote)
		expected := "local://" + tmpDir
		if id != expected {
			assert.Condition(t, func() bool {
				return false
			}, "Expected %q, got: %q", expected, id)
		}
	})

	t.Run("uses default git.GetRemoteURL when getRemoteURL is nil", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Initialize a git repo with a remote
		execGit(t, tmpDir, "init")
		execGit(t, tmpDir, "remote", "add", "origin", "https://github.com/test/repo.git")

		// With nil getRemoteURL, should use git.GetRemoteURL and find the remote
		id := ResolveRepoIdentity(tmpDir, nil)
		if id != "https://github.com/test/repo.git" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 'https://github.com/test/repo.git', got: %q", id)
		}
	})

	t.Run("falls back to local path when nil and no git remote", func(t *testing.T) {
		tmpDir := t.TempDir()

		// With nil getRemoteURL and no git repo, should fall back to local path
		id := ResolveRepoIdentity(tmpDir, nil)
		expected := "local://" + tmpDir
		if id != expected {
			assert.Condition(t, func() bool {
				return false
			}, "Expected %q, got: %q", expected, id)
		}
	})

	t.Run("skips invalid roborev-id and uses remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Write invalid content (starts with dot)
		if err := os.WriteFile(filepath.Join(tmpDir, ".roborev-id"), []byte(".invalid"), 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}

		mockRemote := func(repoPath, remoteName string) string {
			return "https://github.com/user/repo.git"
		}

		id := ResolveRepoIdentity(tmpDir, mockRemote)
		if id != "https://github.com/user/repo.git" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected git remote URL when roborev-id is invalid, got: %q", id)
		}
	})

	t.Run("strips credentials from remote URL", func(t *testing.T) {
		tmpDir := t.TempDir()

		mockRemote := func(repoPath, remoteName string) string {
			return "https://user:token@github.com/org/repo.git"
		}

		id := ResolveRepoIdentity(tmpDir, mockRemote)
		if id != "https://github.com/org/repo.git" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected credentials stripped from URL, got: %q", id)
		}
	})
}

func TestResolveModel(t *testing.T) {
	tests := []struct {
		name         string
		explicit     string
		repoConfig   string
		globalConfig *Config
		want         string
	}{
		{
			name:         "explicit model takes precedence",
			explicit:     "explicit-model",
			globalConfig: &Config{DefaultModel: "global-model"},
			want:         "explicit-model",
		},
		{
			name:         "explicit with whitespace is trimmed",
			explicit:     "  explicit-model  ",
			globalConfig: &Config{DefaultModel: "global-model"},
			want:         "explicit-model",
		},
		{
			name:         "empty explicit falls back to repo config",
			explicit:     "",
			repoConfig:   `model = "repo-model"`,
			globalConfig: &Config{DefaultModel: "global-model"},
			want:         "repo-model",
		},
		{
			name:       "repo config with whitespace is trimmed",
			repoConfig: `model = "  repo-model  "`,
			want:       "repo-model",
		},
		{
			name:         "no repo config falls back to global config",
			globalConfig: &Config{DefaultModel: "global-model"},
			want:         "global-model",
		},
		{
			name:         "global config with whitespace is trimmed",
			globalConfig: &Config{DefaultModel: "  global-model  "},
			want:         "global-model",
		},
		{
			name: "no config returns empty",
			want: "",
		},
		{
			name:         "empty global config returns empty",
			globalConfig: &Config{DefaultModel: ""},
			want:         "",
		},
		{
			name:         "whitespace-only explicit falls through to repo config",
			explicit:     "   ",
			repoConfig:   `model = "repo-model"`,
			globalConfig: &Config{DefaultModel: "global-model"},
			want:         "repo-model",
		},
		{
			name:         "whitespace-only repo config falls through to global",
			repoConfig:   `model = "   "`,
			globalConfig: &Config{DefaultModel: "global-model"},
			want:         "global-model",
		},
		{
			name:         "explicit overrides repo config",
			explicit:     "explicit-model",
			repoConfig:   `model = "repo-model"`,
			globalConfig: &Config{DefaultModel: "global-model"},
			want:         "explicit-model",
		},
		{
			name:         "malformed repo config falls through to global",
			repoConfig:   `this is not valid toml {{{`,
			globalConfig: &Config{DefaultModel: "global-model"},
			want:         "global-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := newTempRepo(t, tt.repoConfig)
			got := ResolveModel(tt.explicit, tmpDir, tt.globalConfig)
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "ResolveModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveMaxPromptSize(t *testing.T) {
	t.Run("default when no config", func(t *testing.T) {
		tmpDir := t.TempDir()
		size := ResolveMaxPromptSize(tmpDir, nil)
		if size != DefaultMaxPromptSize {
			assert.Condition(t, func() bool {
				return false
			}, "Expected default %d, got %d", DefaultMaxPromptSize, size)
		}
	})

	t.Run("default when global config has zero", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfg := &Config{DefaultMaxPromptSize: 0}
		size := ResolveMaxPromptSize(tmpDir, cfg)
		if size != DefaultMaxPromptSize {
			assert.Condition(t, func() bool {
				return false
			}, "Expected default %d when global is 0, got %d", DefaultMaxPromptSize, size)
		}
	})

	t.Run("global config takes precedence over default", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfg := &Config{DefaultMaxPromptSize: 500 * 1024}
		size := ResolveMaxPromptSize(tmpDir, cfg)
		if size != 500*1024 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 500KB from global config, got %d", size)
		}
	})

	t.Run("repo config takes precedence over global", func(t *testing.T) {
		tmpDir := newTempRepo(t, `max_prompt_size = 300000`)
		cfg := &Config{DefaultMaxPromptSize: 500 * 1024}
		size := ResolveMaxPromptSize(tmpDir, cfg)
		if size != 300000 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 300000 from repo config, got %d", size)
		}
	})

	t.Run("repo config zero falls through to global", func(t *testing.T) {
		tmpDir := newTempRepo(t, `max_prompt_size = 0`)
		cfg := &Config{DefaultMaxPromptSize: 500 * 1024}
		size := ResolveMaxPromptSize(tmpDir, cfg)
		if size != 500*1024 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 500KB from global (repo is 0), got %d", size)
		}
	})

	t.Run("repo config without max_prompt_size falls through to global", func(t *testing.T) {
		tmpDir := newTempRepo(t, `agent = "codex"`)
		cfg := &Config{DefaultMaxPromptSize: 600 * 1024}
		size := ResolveMaxPromptSize(tmpDir, cfg)
		if size != 600*1024 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 600KB from global (repo has no max_prompt_size), got %d", size)
		}
	})

	t.Run("malformed repo config falls through to global", func(t *testing.T) {
		tmpDir := newTempRepo(t, `this is not valid toml {{{`)
		cfg := &Config{DefaultMaxPromptSize: 500 * 1024}
		size := ResolveMaxPromptSize(tmpDir, cfg)
		if size != 500*1024 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 500KB from global (repo config malformed), got %d", size)
		}
	})
}

func TestResolveSnapshotDir(t *testing.T) {
	tests := []struct {
		name       string
		repoConfig string
		want       string
		wantErr    string
	}{
		{
			name: "default under repo .roborev",
			want: DefaultSnapshotDir,
		},
		{
			name:       "repo config override",
			repoConfig: `snapshot_dir = "var/roborev"`,
			want:       filepath.Join("var", "roborev"),
		},
		{
			name:       "accepts local path",
			repoConfig: `snapshot_dir = "cache/roborev"`,
			want:       filepath.Join("cache", "roborev"),
		},
		{
			name:       "rejects absolute path",
			repoConfig: `snapshot_dir = "/tmp/roborev"`,
			wantErr:    "relative path",
		},
		{
			name:       "rejects parent traversal",
			repoConfig: `snapshot_dir = "../tmp"`,
			wantErr:    "repo root",
		},
		{
			name:       "rejects traversal after clean",
			repoConfig: `snapshot_dir = "tmp/../../tmp"`,
			wantErr:    "repo root",
		},
		{
			name:       "rejects git dir",
			repoConfig: `snapshot_dir = ".git/roborev"`,
			wantErr:    ".git",
		},
		{
			name:       "rejects control characters",
			repoConfig: "snapshot_dir = \"tmp\\nbad\"",
			wantErr:    "control characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoPath := t.TempDir()
			if tt.repoConfig != "" {
				repoPath = newTempRepo(t, tt.repoConfig)
			}

			dir, err := ResolveSnapshotDir(repoPath)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Empty(t, dir)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(repoPath, tt.want), dir)
		})
	}
}

func TestResolveAgentForWorkflow(t *testing.T) {
	tests := []struct {
		name     string
		cli      string
		repo     map[string]string
		global   *Config
		workflow string
		level    string
		expect   string
	}{
		// Defaults
		{"empty config", "", nil, nil, "review", "fast", "codex"},
		{"global default only", "", nil, &Config{DefaultAgent: "claude"}, "review", "fast", "claude"},

		// Global specificity ladder
		{"global workflow > global default", "", nil, &Config{DefaultAgent: "codex", ReviewAgent: "claude"}, "review", "fast", "claude"},
		{"global level > global workflow", "", nil, &Config{ReviewAgent: "codex", ReviewAgentFast: "claude"}, "review", "fast", "claude"},
		{"global level ignored for wrong level", "", nil, &Config{ReviewAgent: "codex", ReviewAgentFast: "claude"}, "review", "thorough", "codex"},

		// Repo specificity ladder
		{"repo generic only", "", M{"agent": "claude"}, nil, "review", "fast", "claude"},
		{"repo workflow > repo generic", "", M{"agent": "codex", "review_agent": "claude"}, nil, "review", "fast", "claude"},
		{"repo level > repo workflow", "", M{"review_agent": "codex", "review_agent_fast": "claude"}, nil, "review", "fast", "claude"},

		// Layer beats specificity (Option A)
		{"repo generic > global level-specific", "", M{"agent": "claude"}, &Config{ReviewAgentFast: "gemini"}, "review", "fast", "claude"},
		{"repo generic > global workflow-specific", "", M{"agent": "claude"}, &Config{ReviewAgent: "gemini"}, "review", "fast", "claude"},
		{"repo workflow > global level-specific", "", M{"review_agent": "claude"}, &Config{ReviewAgentFast: "gemini"}, "review", "fast", "claude"},

		// CLI wins all
		{"cli > repo level-specific", "droid", M{"review_agent_fast": "claude"}, nil, "review", "fast", "droid"},
		{"cli > everything", "droid", M{"review_agent_fast": "claude"}, &Config{ReviewAgentFast: "gemini"}, "review", "fast", "droid"},

		// Refine workflow isolation
		{"refine uses refine_agent not review_agent", "", M{"review_agent": "claude", "refine_agent": "gemini"}, nil, "refine", "fast", "gemini"},
		{"refine level-specific", "", M{"refine_agent": "codex", "refine_agent_fast": "claude"}, nil, "refine", "fast", "claude"},
		{"review config ignored for refine", "", M{"review_agent_fast": "claude"}, &Config{DefaultAgent: "codex"}, "refine", "fast", "codex"},

		// Level isolation
		{"fast config ignored for standard", "", M{"review_agent_fast": "claude", "review_agent": "codex"}, nil, "review", "standard", "codex"},
		{"standard config used for standard", "", M{"review_agent_standard": "claude"}, nil, "review", "standard", "claude"},
		{"thorough config used for thorough", "", M{"review_agent_thorough": "claude"}, nil, "review", "thorough", "claude"},

		// Mixed layers
		{"repo workflow + global level (repo wins)", "", M{"review_agent": "claude"}, &Config{ReviewAgentFast: "gemini", ReviewAgentThorough: "droid"}, "review", "fast", "claude"},
		{"global fills gaps repo doesn't set", "", M{"agent": "codex"}, &Config{ReviewAgentFast: "claude"}, "review", "standard", "codex"},

		// Fix workflow
		{"fix uses fix_agent", "", M{"fix_agent": "claude"}, nil, "fix", "fast", "claude"},
		{"fix level-specific", "", M{"fix_agent": "codex", "fix_agent_fast": "claude"}, nil, "fix", "fast", "claude"},
		{"fix falls back to generic agent", "", M{"agent": "claude"}, nil, "fix", "fast", "claude"},
		{"fix falls back to global fix_agent", "", nil, &Config{FixAgent: "claude"}, "fix", "fast", "claude"},
		{"fix global level-specific", "", nil, &Config{FixAgent: "codex", FixAgentFast: "claude"}, "fix", "fast", "claude"},
		{"fix standard level selects fix_agent_standard", "", M{"fix_agent_standard": "claude", "fix_agent": "codex"}, nil, "fix", "standard", "claude"},
		{"fix default reasoning (standard) selects level-specific", "", nil, &Config{FixAgentStandard: "claude", FixAgent: "codex"}, "fix", "standard", "claude"},
		{"fix isolated from review", "", M{"review_agent": "claude"}, &Config{DefaultAgent: "codex"}, "fix", "fast", "codex"},
		{"fix isolated from refine", "", M{"refine_agent": "claude"}, &Config{DefaultAgent: "codex"}, "fix", "fast", "codex"},

		// Design workflow
		{"design uses design_agent", "", M{"design_agent": "claude"}, nil, "design", "fast", "claude"},
		{"design level-specific", "", M{"design_agent": "codex", "design_agent_fast": "claude"}, nil, "design", "fast", "claude"},
		{"design falls back to generic agent", "", M{"agent": "claude"}, nil, "design", "fast", "claude"},
		{"design falls back to global design_agent", "", nil, &Config{DesignAgent: "claude"}, "design", "fast", "claude"},
		{"design global level-specific", "", nil, &Config{DesignAgent: "codex", DesignAgentThorough: "claude"}, "design", "thorough", "claude"},
		{"design isolated from review", "", M{"review_agent": "claude"}, &Config{DefaultAgent: "codex"}, "design", "fast", "codex"},
		{"design isolated from security", "", M{"security_agent": "claude"}, &Config{DefaultAgent: "codex"}, "design", "fast", "codex"},

		// Maximum level
		{"maximum repo level-specific", "", M{"review_agent_maximum": "claude"}, nil, "review", "maximum", "claude"},
		{"maximum global level-specific", "", nil, &Config{ReviewAgentMaximum: "claude"}, "review", "maximum", "claude"},
		{"maximum falls back to workflow", "", M{"review_agent": "claude"}, nil, "review", "maximum", "claude"},
		{"maximum falls back to generic", "", M{"agent": "claude"}, nil, "review", "maximum", "claude"},
		{"fix maximum level-specific", "", M{"fix_agent_maximum": "claude"}, nil, "fix", "maximum", "claude"},

		// Medium level
		{"medium repo level-specific", "", M{"review_agent_medium": "claude"}, nil, "review", "medium", "claude"},
		{"medium global level-specific", "", nil, &Config{ReviewAgentMedium: "claude"}, "review", "medium", "claude"},
		{"medium falls back to workflow", "", M{"review_agent": "claude"}, nil, "review", "medium", "claude"},
		{"medium falls back to generic", "", M{"agent": "claude"}, nil, "review", "medium", "claude"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			writeRepoConfig(t, tmpDir, tt.repo)
			got := ResolveAgentForWorkflow(tt.cli, tmpDir, tt.global, tt.workflow, tt.level)
			if got != tt.expect {
				assert.Condition(t, func() bool {
					return false
				}, "got %q, want %q", got, tt.expect)
			}
		})
	}
}

func TestResolveModelForWorkflow(t *testing.T) {
	tests := []struct {
		name     string
		cli      string
		repo     map[string]string
		global   *Config
		workflow string
		level    string
		expect   string
	}{
		// Defaults (model defaults to empty, not "codex")
		{"empty config", "", nil, nil, "review", "fast", ""},
		{"global default only", "", nil, &Config{DefaultModel: "gpt-4"}, "review", "fast", "gpt-4"},

		// Global specificity ladder
		{"global workflow > global default", "", nil, &Config{DefaultModel: "gpt-4", ReviewModel: "claude-3"}, "review", "fast", "claude-3"},
		{"global level > global workflow", "", nil, &Config{ReviewModel: "gpt-4", ReviewModelFast: "claude-3"}, "review", "fast", "claude-3"},

		// Repo specificity ladder
		{"repo generic only", "", M{"model": "gpt-4"}, nil, "review", "fast", "gpt-4"},
		{"repo workflow > repo generic", "", M{"model": "gpt-4", "review_model": "claude-3"}, nil, "review", "fast", "claude-3"},
		{"repo level > repo workflow", "", M{"review_model": "gpt-4", "review_model_fast": "claude-3"}, nil, "review", "fast", "claude-3"},

		// Layer beats specificity (Option A)
		{"repo generic > global level-specific", "", M{"model": "gpt-4"}, &Config{ReviewModelFast: "claude-3"}, "review", "fast", "gpt-4"},

		// CLI wins all
		{"cli > everything", "o1", M{"review_model_fast": "gpt-4"}, &Config{ReviewModelFast: "claude-3"}, "review", "fast", "o1"},

		// Refine workflow isolation
		{"refine uses refine_model", "", M{"review_model": "gpt-4", "refine_model": "claude-3"}, nil, "refine", "fast", "claude-3"},

		// Fix workflow
		{"fix uses fix_model", "", M{"fix_model": "gpt-4"}, nil, "fix", "fast", "gpt-4"},
		{"fix level-specific model", "", M{"fix_model": "gpt-4", "fix_model_fast": "claude-3"}, nil, "fix", "fast", "claude-3"},
		{"fix falls back to generic model", "", M{"model": "gpt-4"}, nil, "fix", "fast", "gpt-4"},
		{"fix isolated from review model", "", M{"review_model": "gpt-4"}, nil, "fix", "fast", ""},

		// Design workflow
		{"design uses design_model", "", M{"design_model": "gpt-4"}, nil, "design", "fast", "gpt-4"},
		{"design level-specific model", "", M{"design_model": "gpt-4", "design_model_fast": "claude-3"}, nil, "design", "fast", "claude-3"},
		{"design falls back to generic model", "", M{"model": "gpt-4"}, nil, "design", "fast", "gpt-4"},
		{"design isolated from review model", "", M{"review_model": "gpt-4"}, nil, "design", "fast", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			writeRepoConfig(t, tmpDir, tt.repo)
			got := ResolveModelForWorkflow(tt.cli, tmpDir, tt.global, tt.workflow, tt.level)
			if got != tt.expect {
				assert.Condition(t, func() bool {
					return false
				}, "got %q, want %q", got, tt.expect)
			}
		})
	}
}

func TestResolveWorkflowModel(t *testing.T) {
	tests := []struct {
		name     string
		repo     map[string]string
		global   *Config
		workflow string
		level    string
		expect   string
	}{
		// Empty config returns empty
		{
			"empty config",
			nil, nil,
			"fix", "fast", "",
		},
		// Skips generic global default_model
		{
			"skips global default_model",
			nil, &Config{DefaultModel: "gpt-5.4"},
			"fix", "fast", "",
		},
		// Skips generic repo model
		{
			"skips repo generic model",
			M{"model": "gpt-5.4"},
			nil,
			"fix", "fast", "",
		},
		// Uses workflow-specific model from global config
		{
			"global fix_model",
			nil, &Config{DefaultModel: "gpt-5.4", FixModel: "gemini-2.5-pro"},
			"fix", "fast", "gemini-2.5-pro",
		},
		// Uses level-specific model from global config
		{
			"global fix_model_fast",
			nil, &Config{DefaultModel: "gpt-5.4", FixModelFast: "gemini-2.5-flash"},
			"fix", "fast", "gemini-2.5-flash",
		},
		// Level-specific beats workflow-level in global
		{
			"global level > global workflow",
			nil, &Config{FixModel: "gpt-4", FixModelFast: "claude-3"},
			"fix", "fast", "claude-3",
		},
		// Uses workflow-specific model from repo config
		{
			"repo fix_model",
			M{"model": "gpt-5.4", "fix_model": "gemini-2.5-pro"},
			nil,
			"fix", "fast", "gemini-2.5-pro",
		},
		// Uses level-specific model from repo config
		{
			"repo fix_model_fast",
			M{"fix_model_fast": "claude-3"},
			nil,
			"fix", "fast", "claude-3",
		},
		// Repo beats global for workflow-specific
		{
			"repo workflow > global workflow",
			M{"fix_model": "repo-model"},
			&Config{FixModel: "global-model"},
			"fix", "fast", "repo-model",
		},
		// Review workflow isolation
		{
			"review workflow uses review_model",
			M{"fix_model": "fix-only", "review_model": "review-only"},
			nil,
			"review", "standard", "review-only",
		},
		// Skips both global default_model and repo generic model
		{
			"skips both generic defaults",
			M{"model": "repo-generic"},
			&Config{DefaultModel: "global-generic"},
			"fix", "fast", "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			writeRepoConfig(t, tmpDir, tt.repo)
			got := ResolveWorkflowModel(tmpDir, tt.global, tt.workflow, tt.level)
			if got != tt.expect {
				assert.Condition(t, func() bool {
					return false
				}, "got %q, want %q", got, tt.expect)
			}
		})
	}
}

func TestResolveBackupAgentForWorkflow(t *testing.T) {
	tests := []struct {
		name     string
		repo     map[string]string
		global   *Config
		workflow string
		expect   string
	}{
		// No backup configured
		{"empty config", nil, nil, "review", ""},
		{"only primary agent configured", M{"review_agent": "claude"}, nil, "review", ""},

		// Global backup agent
		{"global backup only", nil, &Config{ReviewBackupAgent: "test"}, "review", "test"},
		{"global backup for refine", nil, &Config{RefineBackupAgent: "claude"}, "refine", "claude"},
		{"global backup for fix", nil, &Config{FixBackupAgent: "codex"}, "fix", "codex"},
		{"global backup for security", nil, &Config{SecurityBackupAgent: "gemini"}, "security", "gemini"},
		{"global backup for design", nil, &Config{DesignBackupAgent: "droid"}, "design", "droid"},

		// Repo backup agent overrides global
		{"repo overrides global", M{"review_backup_agent": "repo-test"}, &Config{ReviewBackupAgent: "global-test"}, "review", "repo-test"},
		{"repo backup only", M{"review_backup_agent": "test"}, nil, "review", "test"},

		// Different workflows resolve independently
		{"review backup doesn't affect refine", M{"review_backup_agent": "claude"}, nil, "refine", ""},
		{"each workflow has own backup", M{"review_backup_agent": "claude", "refine_backup_agent": "codex"}, nil, "review", "claude"},
		{"each workflow has own backup - refine", M{"review_backup_agent": "claude", "refine_backup_agent": "codex"}, nil, "refine", "codex"},

		// Unknown workflow returns empty
		{"unknown workflow", M{"review_backup_agent": "test"}, nil, "unknown", ""},

		// No reasoning level support for backup agents
		{"no level variants recognized", M{"review_backup_agent_fast": "claude"}, nil, "review", ""},
		{"backup agent doesn't use levels", M{"review_backup_agent": "claude"}, nil, "review", "claude"},

		// Default/generic backup agent fallback
		{"global default_backup_agent", nil, &Config{DefaultBackupAgent: "test"}, "review", "test"},
		{"global default_backup_agent for any workflow", nil, &Config{DefaultBackupAgent: "test"}, "fix", "test"},
		{"global workflow-specific overrides default", nil, &Config{DefaultBackupAgent: "test", ReviewBackupAgent: "claude"}, "review", "claude"},
		{"global default used when workflow not set", nil, &Config{DefaultBackupAgent: "test", ReviewBackupAgent: "claude"}, "fix", "test"},
		{"repo backup_agent generic", M{"backup_agent": "repo-fallback"}, nil, "review", "repo-fallback"},
		{"repo backup_agent generic for any workflow", M{"backup_agent": "repo-fallback"}, nil, "refine", "repo-fallback"},
		{"repo workflow-specific overrides repo generic", M{"backup_agent": "generic", "review_backup_agent": "specific"}, nil, "review", "specific"},
		{"repo generic overrides global workflow-specific", M{"backup_agent": "repo"}, &Config{ReviewBackupAgent: "global"}, "review", "repo"},
		{"repo generic overrides global default", M{"backup_agent": "repo"}, &Config{DefaultBackupAgent: "global"}, "review", "repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp dir for repo config
			repoDir := t.TempDir()

			// Write repo config if provided
			if tt.repo != nil {
				writeRepoConfig(t, repoDir, tt.repo)
			}

			// Test the function
			result := ResolveBackupAgentForWorkflow(repoDir, tt.global, tt.workflow)

			if result != tt.expect {
				assert.Condition(t, func() bool {
					return false
				}, "ResolveBackupAgentForWorkflow(%q, global, %q) = %q, want %q",
					repoDir, tt.workflow, result, tt.expect)
			}
		})
	}
}

func TestResolveBackupModelForWorkflow(t *testing.T) {
	tests := []struct {
		name     string
		repo     map[string]string
		global   *Config
		workflow string
		expect   string
	}{
		// No backup model configured
		{"empty config", nil, nil, "review", ""},
		{"only backup agent configured", M{"review_backup_agent": "claude"}, nil, "review", ""},

		// Global backup model
		{"global backup model only", nil, &Config{ReviewBackupModel: "gpt-4"}, "review", "gpt-4"},
		{"global backup model for refine", nil, &Config{RefineBackupModel: "claude-3"}, "refine", "claude-3"},
		{"global backup model for fix", nil, &Config{FixBackupModel: "o3-mini"}, "fix", "o3-mini"},
		{"global backup model for security", nil, &Config{SecurityBackupModel: "gpt-4"}, "security", "gpt-4"},
		{"global backup model for design", nil, &Config{DesignBackupModel: "claude-3"}, "design", "claude-3"},

		// Repo backup model overrides global
		{"repo overrides global", M{"review_backup_model": "repo-model"}, &Config{ReviewBackupModel: "global-model"}, "review", "repo-model"},
		{"repo backup model only", M{"review_backup_model": "gpt-4"}, nil, "review", "gpt-4"},

		// Different workflows resolve independently
		{"review backup model doesn't affect refine", M{"review_backup_model": "gpt-4"}, nil, "refine", ""},
		{"each workflow has own backup model", M{"review_backup_model": "gpt-4", "refine_backup_model": "claude-3"}, nil, "review", "gpt-4"},
		{"each workflow has own backup model - refine", M{"review_backup_model": "gpt-4", "refine_backup_model": "claude-3"}, nil, "refine", "claude-3"},

		// Unknown workflow returns empty
		{"unknown workflow", M{"review_backup_model": "gpt-4"}, nil, "unknown", ""},

		// Default/generic backup model fallback
		{"global default_backup_model", nil, &Config{DefaultBackupModel: "gpt-4"}, "review", "gpt-4"},
		{"global default_backup_model for any workflow", nil, &Config{DefaultBackupModel: "gpt-4"}, "fix", "gpt-4"},
		{"global workflow-specific overrides default", nil, &Config{DefaultBackupModel: "gpt-4", ReviewBackupModel: "claude-3"}, "review", "claude-3"},
		{"global default used when workflow not set", nil, &Config{DefaultBackupModel: "gpt-4", ReviewBackupModel: "claude-3"}, "fix", "gpt-4"},
		{"repo backup_model generic", M{"backup_model": "repo-model"}, nil, "review", "repo-model"},
		{"repo backup_model generic for any workflow", M{"backup_model": "repo-model"}, nil, "refine", "repo-model"},
		{"repo workflow-specific overrides repo generic", M{"backup_model": "generic", "review_backup_model": "specific"}, nil, "review", "specific"},
		{"repo generic overrides global workflow-specific", M{"backup_model": "repo"}, &Config{ReviewBackupModel: "global"}, "review", "repo"},
		{"repo generic overrides global default", M{"backup_model": "repo"}, &Config{DefaultBackupModel: "global"}, "review", "repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			if tt.repo != nil {
				writeRepoConfig(t, repoDir, tt.repo)
			}
			result := ResolveBackupModelForWorkflow(repoDir, tt.global, tt.workflow)
			if result != tt.expect {
				assert.Condition(t, func() bool {
					return false
				}, "ResolveBackupModelForWorkflow(%q, global, %q) = %q, want %q",
					repoDir, tt.workflow, result, tt.expect)
			}
		})
	}
}

func TestResolvedReviewTypes(t *testing.T) {
	t.Run("uses configured types", func(t *testing.T) {
		ci := CIConfig{ReviewTypes: []string{"security", "review"}}
		got := ci.ResolvedReviewTypes()
		if len(got) != 2 || got[0] != "security" || got[1] != "review" {
			assert.Condition(t, func() bool {
				return false
			}, "got %v, want [security review]", got)
		}
	})

	t.Run("defaults to security", func(t *testing.T) {
		ci := CIConfig{}
		got := ci.ResolvedReviewTypes()
		if len(got) != 1 || got[0] != "security" {
			assert.Condition(t, func() bool {
				return false
			}, "got %v, want [security]", got)
		}
	})
}

func TestResolvedAgents(t *testing.T) {
	t.Run("uses configured agents", func(t *testing.T) {
		ci := CIConfig{Agents: []string{"codex", "gemini"}}
		got := ci.ResolvedAgents()
		if len(got) != 2 || got[0] != "codex" || got[1] != "gemini" {
			assert.Condition(t, func() bool {
				return false
			}, "got %v, want [codex gemini]", got)
		}
	})

	t.Run("defaults to auto-detect", func(t *testing.T) {
		ci := CIConfig{}
		got := ci.ResolvedAgents()
		if len(got) != 1 || got[0] != "" {
			assert.Condition(t, func() bool {
				return false
			}, "got %v, want [\"\"]", got)
		}
	})
}

func TestResolvedMaxRepos(t *testing.T) {
	tests := []struct {
		name     string
		maxRepos int
		want     int
	}{
		{"default when zero", 0, 100},
		{"default when negative", -5, 100},
		{"custom value", 50, 50},
		{"custom large value", 500, 500},
		{"value of 1", 1, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ci := CIConfig{MaxRepos: tt.maxRepos}
			got := ci.ResolvedMaxRepos()
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "ResolvedMaxRepos() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCIConfigNewFields(t *testing.T) {
	t.Run("parses exclude_repos and max_repos", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.toml")
		if err := os.WriteFile(configPath, []byte(`
[ci]
enabled = true
repos = ["myorg/*", "other/repo"]
exclude_repos = ["myorg/archived-*", "myorg/internal-*"]
max_repos = 50
`), 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}

		cfg, err := LoadGlobalFrom(configPath)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "LoadGlobalFrom: %v", err)
		}

		if len(cfg.CI.Repos) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "got %d repos, want 2", len(cfg.CI.Repos))
		}
		if len(cfg.CI.ExcludeRepos) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "got %d exclude_repos, want 2", len(cfg.CI.ExcludeRepos))
		}
		if cfg.CI.MaxRepos != 50 {
			assert.Condition(t, func() bool {
				return false
			}, "got max_repos %d, want 50", cfg.CI.MaxRepos)
		}
		if cfg.CI.ResolvedMaxRepos() != 50 {
			assert.Condition(t, func() bool {
				return false
			}, "ResolvedMaxRepos() = %d, want 50", cfg.CI.ResolvedMaxRepos())
		}
	})
}

func TestNormalizeMinSeverity(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"critical", "critical", false},
		{"high", "high", false},
		{"medium", "medium", false},
		{"low", "low", false},
		{"CRITICAL", "critical", false},
		{"  High  ", "high", false},
		{"Medium", "medium", false},
		{"invalid", "", true},
		{"thorough", "", true},
	}

	for _, tt := range tests {
		t.Run("input_"+tt.input, func(t *testing.T) {
			got, err := NormalizeMinSeverity(tt.input)
			if (err != nil) != tt.wantErr {
				require.Condition(t, func() bool {
					return false
				}, "NormalizeMinSeverity(%q) error = %v, wantErr = %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "NormalizeMinSeverity(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRepoCIConfig(t *testing.T) {
	t.Run("parses agents and review_types", func(t *testing.T) {
		tmpDir := newTempRepo(t, `
agent = "codex"

[ci]
agents = ["gemini", "claude"]
review_types = ["security", "review"]
reasoning = "standard"
`)
		cfg, err := LoadRepoConfig(tmpDir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "LoadRepoConfig: %v", err)
		}
		if len(cfg.CI.Agents) != 2 || cfg.CI.Agents[0] != "gemini" || cfg.CI.Agents[1] != "claude" {
			assert.Condition(t, func() bool {
				return false
			}, "got agents %v, want [gemini claude]", cfg.CI.Agents)
		}
		if len(cfg.CI.ReviewTypes) != 2 || cfg.CI.ReviewTypes[0] != "security" || cfg.CI.ReviewTypes[1] != "review" {
			assert.Condition(t, func() bool {
				return false
			}, "got review_types %v, want [security review]", cfg.CI.ReviewTypes)
		}
		if cfg.CI.Reasoning != "standard" {
			assert.Condition(t, func() bool {
				return false
			}, "got reasoning %q, want %q", cfg.CI.Reasoning, "standard")
		}
	})

	t.Run("empty CI section", func(t *testing.T) {
		tmpDir := newTempRepo(t, `agent = "codex"`)
		cfg, err := LoadRepoConfig(tmpDir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "LoadRepoConfig: %v", err)
		}
		if len(cfg.CI.Agents) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "got agents %v, want empty", cfg.CI.Agents)
		}
		if len(cfg.CI.ReviewTypes) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "got review_types %v, want empty", cfg.CI.ReviewTypes)
		}
		if cfg.CI.Reasoning != "" {
			assert.Condition(t, func() bool {
				return false
			}, "got reasoning %q, want empty", cfg.CI.Reasoning)
		}
	})
}

func TestInstallationIDForOwner(t *testing.T) {
	t.Run("map lookup", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{
			GitHubAppInstallations: map[string]int64{
				"wesm":        111111,
				"roborev-dev": 222222,
			},
			GitHubAppInstallationID: 999999,
		}}
		if got := ci.InstallationIDForOwner("wesm"); got != 111111 {
			assert.Condition(t, func() bool {
				return false
			}, "got %d, want 111111", got)
		}
		if got := ci.InstallationIDForOwner("roborev-dev"); got != 222222 {
			assert.Condition(t, func() bool {
				return false
			}, "got %d, want 222222", got)
		}
	})

	t.Run("falls back to singular", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{
			GitHubAppInstallations:  map[string]int64{"wesm": 111111},
			GitHubAppInstallationID: 999999,
		}}
		if got := ci.InstallationIDForOwner("unknown-org"); got != 999999 {
			assert.Condition(t, func() bool {
				return false
			}, "got %d, want 999999", got)
		}
	})

	t.Run("zero when unset", func(t *testing.T) {
		ci := CIConfig{}
		if got := ci.InstallationIDForOwner("wesm"); got != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "got %d, want 0", got)
		}
	})

	t.Run("zero mapped value falls back to singular", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{
			GitHubAppInstallations:  map[string]int64{"wesm": 0},
			GitHubAppInstallationID: 999999,
		}}
		if got := ci.InstallationIDForOwner("wesm"); got != 999999 {
			assert.Condition(t, func() bool {
				return false
			}, "got %d, want 999999 (fallback to singular)", got)
		}
	})

	t.Run("case-insensitive lookup after normalization", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{
			GitHubAppInstallations: map[string]int64{"Wesm": 111111, "RoboRev-Dev": 222222},
		}}
		if err := ci.NormalizeInstallations(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "NormalizeInstallations: %v", err)
		}
		if got := ci.InstallationIDForOwner("wesm"); got != 111111 {
			assert.Condition(t, func() bool {
				return false
			}, "got %d, want 111111", got)
		}
		if got := ci.InstallationIDForOwner("WESM"); got != 111111 {
			assert.Condition(t, func() bool {
				return false
			}, "got %d, want 111111", got)
		}
		if got := ci.InstallationIDForOwner("roborev-dev"); got != 222222 {
			assert.Condition(t, func() bool {
				return false
			}, "got %d, want 222222", got)
		}
	})
}

func TestNormalizeInstallations(t *testing.T) {
	t.Run("lowercases keys", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{
			GitHubAppInstallations: map[string]int64{"Wesm": 111111, "RoboRev-Dev": 222222},
		}}
		if err := ci.NormalizeInstallations(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "NormalizeInstallations: %v", err)
		}
		if _, ok := ci.GitHubAppInstallations["wesm"]; !ok {
			assert.Condition(t, func() bool {
				return false
			}, "expected lowercase key 'wesm' after normalization")
		}
		if _, ok := ci.GitHubAppInstallations["roborev-dev"]; !ok {
			assert.Condition(t, func() bool {
				return false
			}, "expected lowercase key 'roborev-dev' after normalization")
		}
		if _, ok := ci.GitHubAppInstallations["Wesm"]; ok {
			assert.Condition(t, func() bool {
				return false
			}, "original mixed-case key 'Wesm' should not exist after normalization")
		}
	})

	t.Run("noop on nil map", func(t *testing.T) {
		ci := CIConfig{}
		if err := ci.NormalizeInstallations(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "NormalizeInstallations on nil map: %v", err)
		}
	})

	t.Run("case-colliding keys returns error", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{
			GitHubAppInstallations: map[string]int64{"wesm": 111111, "Wesm": 222222},
		}}
		err := ci.NormalizeInstallations()
		if err == nil {
			require.Condition(t, func() bool {
				return false
			}, "expected error for case-colliding keys")
		}
		if !strings.Contains(err.Error(), "case-colliding") {
			assert.Condition(t, func() bool {
				return false
			}, "expected case-colliding error, got: %v", err)
		}
	})
}

func TestLoadGlobalFrom_NormalizesInstallations(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	{
		err := os.WriteFile(configPath, []byte(`
[ci]
github_app_id = 12345
github_app_private_key = "~/.roborev/app.pem"

[ci.github_app_installations]
Wesm = 111111
RoboRev-Dev = 222222
`), 0o644)
		require.Condition(t, func() bool {
			return err == nil
		}, err)
	}

	cfg, err := LoadGlobalFrom(configPath)
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobalFrom: %v", err)
	{

		got := cfg.CI.InstallationIDForOwner("wesm")
		assert.Condition(t, func() bool {
			return got == 111111
		}, "got %d, want 111111 for normalized 'wesm'", got)
	}
	{

		got := cfg.CI.InstallationIDForOwner("roborev-dev")
		assert.Condition(t, func() bool {
			return got == 222222
		}, "got %d, want 222222 for normalized 'roborev-dev'", got)
	}
}

func TestGitHubAppConfigured_MultiInstall(t *testing.T) {
	t.Run("configured with map only", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{
			GitHubAppID:            12345,
			GitHubAppPrivateKey:    "~/.roborev/app.pem",
			GitHubAppInstallations: map[string]int64{"wesm": 111111},
		}}
		if !ci.GitHubAppConfigured() {
			assert.Condition(t, func() bool {
				return false
			}, "expected GitHubAppConfigured() == true with map only")
		}
	})

	t.Run("configured with singular only", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{
			GitHubAppID:             12345,
			GitHubAppPrivateKey:     "~/.roborev/app.pem",
			GitHubAppInstallationID: 111111,
		}}
		if !ci.GitHubAppConfigured() {
			assert.Condition(t, func() bool {
				return false
			}, "expected GitHubAppConfigured() == true with singular only")
		}
	})

	t.Run("not configured without any installation", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{
			GitHubAppID:         12345,
			GitHubAppPrivateKey: "~/.roborev/app.pem",
		}}
		if ci.GitHubAppConfigured() {
			assert.Condition(t, func() bool {
				return false
			}, "expected GitHubAppConfigured() == false without any installation ID")
		}
	})

	t.Run("not configured without private key", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{
			GitHubAppID:             12345,
			GitHubAppInstallationID: 111111,
		}}
		if ci.GitHubAppConfigured() {
			assert.Condition(t, func() bool {
				return false
			}, "expected GitHubAppConfigured() == false without private key")
		}
	})
}

func TestGitHubAppPrivateKeyResolved_TildeExpansion(t *testing.T) {
	// Create a temp PEM file
	dir := t.TempDir()
	pemFile := filepath.Join(dir, "test.pem")
	pemContent := "-----BEGIN RSA PRIVATE KEY-----\nfakekey\n-----END RSA PRIVATE KEY-----"
	{
		err := os.WriteFile(pemFile, []byte(pemContent), 0o600)
		require.Condition(t, func() bool {
			return err == nil
		}, err)
	}

	t.Run("inline PEM returned directly", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{GitHubAppPrivateKey: pemContent}}
		got, err := ci.GitHubAppPrivateKeyResolved()
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}
		if got != pemContent {
			assert.Condition(t, func() bool {
				return false
			}, "got %q, want inline PEM", got)
		}
	})

	t.Run("absolute path reads file", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{GitHubAppPrivateKey: pemFile}}
		got, err := ci.GitHubAppPrivateKeyResolved()
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}
		if got != pemContent {
			assert.Condition(t, func() bool {
				return false
			}, "got %q, want %q", got, pemContent)
		}
	})

	t.Run("tilde path expands to home", func(t *testing.T) {
		// Use a fake HOME so we don't touch the real home directory
		fakeHome := t.TempDir()
		t.Setenv("HOME", fakeHome)
		t.Setenv("USERPROFILE", fakeHome) // Windows compatibility

		fakePem := filepath.Join(fakeHome, ".roborev", "test.pem")
		if err := os.MkdirAll(filepath.Dir(fakePem), 0o700); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}
		if err := os.WriteFile(fakePem, []byte(pemContent), 0o600); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}

		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{GitHubAppPrivateKey: "~/.roborev/test.pem"}}
		got, err := ci.GitHubAppPrivateKeyResolved()
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "tilde expansion failed: %v", err)
		}
		if got != pemContent {
			assert.Condition(t, func() bool {
				return false
			}, "got %q, want %q", got, pemContent)
		}
	})

	t.Run("empty after expansion returns error", func(t *testing.T) {
		ci := CIConfig{GitHubAppConfig: GitHubAppConfig{GitHubAppPrivateKey: ""}}
		_, err := ci.GitHubAppPrivateKeyResolved()
		if err == nil {
			assert.Condition(t, func() bool {
				return false
			}, "expected error for empty key")
		}
	})
}

func TestStripURLCredentials(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "HTTPS URL with user:password",
			input:    "https://user:password@github.com/org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "HTTPS URL with token only",
			input:    "https://token@github.com/org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "HTTPS URL without credentials",
			input:    "https://github.com/org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "SSH URL unchanged",
			input:    "git@github.com:org/repo.git",
			expected: "git@github.com:org/repo.git",
		},
		{
			name:     "HTTP URL with credentials",
			input:    "http://user:pass@gitlab.example.com/project.git",
			expected: "http://gitlab.example.com/project.git",
		},
		{
			name:     "URL with only username",
			input:    "https://user@bitbucket.org/team/repo.git",
			expected: "https://bitbucket.org/team/repo.git",
		},
		{
			name:     "Local path unchanged",
			input:    "/path/to/repo",
			expected: "/path/to/repo",
		},
		{
			name:     "File URL unchanged",
			input:    "file:///path/to/repo",
			expected: "file:///path/to/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stripURLCredentials(tt.input)
			if result != tt.expected {
				assert.Condition(t, func() bool {
					return false
				}, "stripURLCredentials(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestHideClosedDefaultPersistence(t *testing.T) {
	testenv.SetDataDir(t)

	// Test saving hide_closed_by_default as true
	cfg := &Config{HideClosedByDefault: true}
	err := SaveGlobal(cfg)
	require.Condition(t, func() bool {
		return err == nil
	}, "SaveGlobal failed: %v", err)

	// Load and verify it persisted
	loaded, err := LoadGlobal()
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobal failed: %v", err)
	assert.Condition(t, func() bool {
		return loaded.HideClosedByDefault
	}, "Expected HideClosedByDefault to be true")

	// Toggle to false and verify
	loaded.HideClosedByDefault = false
	err = SaveGlobal(loaded)
	require.Condition(t, func() bool {
		return err == nil
	}, "SaveGlobal failed: %v", err)

	reloaded, err := LoadGlobal()
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobal failed: %v", err)
	assert.Condition(t, func() bool {
		return !reloaded.HideClosedByDefault
	}, "Expected HideClosedByDefault to be false")
}

func TestAdvancedTasksEnabledPersistence(t *testing.T) {
	testenv.SetDataDir(t)

	cfg := &Config{}
	cfg.Advanced.TasksEnabled = true
	{
		err := SaveGlobal(cfg)
		require.Condition(t, func() bool {
			return err == nil
		}, "SaveGlobal failed: %v", err)
	}

	loaded, err := LoadGlobal()
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobal failed: %v", err)
	assert.Condition(t, func() bool {
		return loaded.Advanced.TasksEnabled
	}, "Expected Advanced.TasksEnabled to be true")

	loaded.Advanced.TasksEnabled = false
	{
		err := SaveGlobal(loaded)
		require.Condition(t, func() bool {
			return err == nil
		}, "SaveGlobal failed: %v", err)
	}

	reloaded, err := LoadGlobal()
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobal failed: %v", err)
	assert.Condition(t, func() bool {
		return !reloaded.Advanced.TasksEnabled
	}, "Expected Advanced.TasksEnabled to be false")
}

func TestSaveGlobalWritesDocumentedSettings(t *testing.T) {
	testenv.SetDataDir(t)

	cfg := DefaultConfig()
	cfg.DefaultAgent = "claude-code"
	cfg.HideClosedByDefault = true
	cfg.MouseEnabled = false
	cfg.Advanced.TasksEnabled = true
	{

		err := SaveGlobal(cfg)
		require.Condition(t, func() bool {
			return err == nil
		}, "SaveGlobal failed: %v", err)
	}

	data, err := os.ReadFile(GlobalConfigPath())
	require.Condition(t, func() bool {
		return err == nil
	}, "ReadFile failed: %v", err)

	got := string(data)

	for _, want := range []string{
		"default_agent = 'claude-code'",
		"# Hide closed reviews by default in the TUI queue.\nhide_closed_by_default = true",
		"# Enable mouse support in the TUI.\nmouse_enabled = false",
		"[advanced]\n# Enable the advanced Tasks workflow in the TUI.\ntasks_enabled = true",
		"max_workers = 4",
	} {
		require.Condition(t, func() bool {
			return strings.Contains(got, want)
		}, "saved config missing documented setting %q:\n%s", want, got)
	}
}

func TestHideAddressedDeprecatedMigration(t *testing.T) {
	testenv.SetDataDir(t)

	// Write a config using the deprecated hide_addressed_by_default key
	cfgPath := GlobalConfigPath()
	{
		err := os.MkdirAll(filepath.Dir(cfgPath), 0o755)
		require.Condition(t, func() bool {
			return err == nil
		}, "mkdir failed: %v", err)
	}
	{

		err := os.WriteFile(cfgPath, []byte("hide_addressed_by_default = true\n"), 0o644)
		require.Condition(t, func() bool {
			return err == nil
		}, "WriteFile failed: %v", err)
	}

	// Load should migrate to HideClosedByDefault
	loaded, err := LoadGlobal()
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobal failed: %v", err)
	assert.Condition(t, func() bool {
		return loaded.HideClosedByDefault
	}, "Expected deprecated hide_addressed_by_default to migrate to HideClosedByDefault")
	assert.Condition(t, func() bool {
		return !loaded.HideAddressedByDefault
	}, "Expected HideAddressedByDefault to be cleared after migration")
}

func TestHideAddressedDoesNotOverrideExplicitNewKey(t *testing.T) {
	testenv.SetDataDir(t)

	// Both deprecated and new key set — explicit new key should win
	cfgPath := GlobalConfigPath()
	{
		err := os.MkdirAll(filepath.Dir(cfgPath), 0o755)
		require.Condition(t, func() bool {
			return err == nil
		}, "mkdir failed: %v", err)
	}

	content := "hide_addressed_by_default = true\nhide_closed_by_default = false\n"
	{
		err := os.WriteFile(cfgPath, []byte(content), 0o644)
		require.Condition(t, func() bool {
			return err == nil
		}, "WriteFile failed: %v", err)
	}

	loaded, err := LoadGlobal()
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobal failed: %v", err)
	assert.Condition(t, func() bool {
		return !loaded.HideClosedByDefault
	}, "Deprecated key should not override explicit hide_closed_by_default = false")
	assert.Condition(t, func() bool {
		return !loaded.HideAddressedByDefault
	}, "Expected HideAddressedByDefault to be cleared after migration")
}

func TestHiddenColumnsExplicitEmptyBecomesSentinel(t *testing.T) {
	testenv.SetDataDir(t)

	cfgPath := GlobalConfigPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(cfgPath), 0o755))
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("hidden_columns = []\n"), 0o644))

	loaded, err := LoadGlobal()
	require.NoError(t, err)
	assert.Equal(t,
		[]string{HiddenColumnsNoneSentinel}, loaded.HiddenColumns,
		"explicit hidden_columns = [] should become sentinel")
}

func TestHiddenColumnsRenamedOnlyDoesNotBecomeSentinel(t *testing.T) {
	testenv.SetDataDir(t)

	cfgPath := GlobalConfigPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(cfgPath), 0o755))
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("hidden_columns = [\"handled\"]\n"), 0o644))

	loaded, err := LoadGlobal()
	require.NoError(t, err)
	assert.Equal(t, []string{"closed"}, loaded.HiddenColumns,
		"hidden_columns with renamed entries should migrate, not become sentinel")
}

func TestIsDefaultReviewType(t *testing.T) {
	defaults := []string{"", "default", "general", "review"}
	for _, rt := range defaults {
		assert.Condition(t, func() bool {
			return IsDefaultReviewType(rt)
		}, "expected %q to be default review type", rt)
	}
	nonDefaults := []string{"security", "design", "bogus"}
	for _, rt := range nonDefaults {
		assert.Condition(t, func() bool {
			return !IsDefaultReviewType(rt)
		}, "expected %q to NOT be default review type", rt)
	}
}

func TestLoadRepoConfigFromRef(t *testing.T) {
	// Create a real git repo with .roborev.toml at a commit
	dir := t.TempDir()
	execGit(t, dir, "init")
	execGit(t, dir, "config", "user.email", "test@test.com")
	execGit(t, dir, "config", "user.name", "Test")

	// Write .roborev.toml and commit
	configContent := `review_guidelines = "Use descriptive variable names."` + "\n"
	writeTestFile(t, dir, ".roborev.toml", configContent)
	execGit(t, dir, "add", ".roborev.toml")
	execGit(t, dir, "commit", "-m", "add config")

	// Get the commit SHA
	sha := execGit(t, dir, "rev-parse", "HEAD")

	t.Run("loads config from ref", func(t *testing.T) {
		cfg, err := LoadRepoConfigFromRef(dir, sha)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "LoadRepoConfigFromRef: %v", err)
		}
		require.NotNil(t, cfg, "expected non-nil config")
		assert.Equal(t, "Use descriptive variable names.", cfg.ReviewGuidelines, "got review guidelines")
	})

	t.Run("returns nil for nonexistent ref", func(t *testing.T) {
		cfg, err := LoadRepoConfigFromRef(dir, "0000000000000000000000000000000000000000")
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "unexpected error: %v", err)
		}
		if cfg != nil {
			assert.Condition(t, func() bool {
				return false
			}, "expected nil config for nonexistent ref")
		}
	})

	t.Run("returns nil when file missing from ref", func(t *testing.T) {
		// Remove .roborev.toml and commit
		execGit(t, dir, "rm", ".roborev.toml")
		execGit(t, dir, "commit", "-m", "remove config")
		headSHA := execGit(t, dir, "rev-parse", "HEAD")

		cfg, err := LoadRepoConfigFromRef(dir, headSHA)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "unexpected error: %v", err)
		}
		if cfg != nil {
			assert.Condition(t, func() bool {
				return false
			}, "expected nil config when file removed from ref")
		}
	})
}

func TestValidateReviewTypes(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		want    []string
		wantErr bool
	}{
		{
			name:  "valid types pass through",
			input: []string{"default", "security", "design"},
			want:  []string{"default", "security", "design"},
		},
		{
			name:  "alias review canonicalizes",
			input: []string{"review"},
			want:  []string{"default"},
		},
		{
			name:  "alias general canonicalizes",
			input: []string{"general"},
			want:  []string{"default"},
		},
		{
			name:  "duplicates removed",
			input: []string{"default", "review", "general"},
			want:  []string{"default"},
		},
		{
			name:  "mixed valid with dedup",
			input: []string{"security", "review", "security"},
			want:  []string{"security", "default"},
		},
		{
			name:    "invalid type returns error",
			input:   []string{"typo"},
			wantErr: true,
		},
		{
			name:    "empty string returns error",
			input:   []string{""},
			wantErr: true,
		},
		{
			name:    "invalid among valid",
			input:   []string{"security", "bogus"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateReviewTypes(tt.input)
			if tt.wantErr {
				if err == nil {
					require.Condition(t, func() bool {
						return false
					}, "expected error")
				}
				return
			}
			if err != nil {
				require.Condition(t, func() bool {
					return false
				}, "unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				require.Condition(t, func() bool {
					return false
				}, "got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					assert.Condition(t, func() bool {
						return false
					}, "got[%d] = %q, want %q",
						i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestResolvedReviewMatrix(t *testing.T) {
	t.Run("falls back to cross-product", func(t *testing.T) {
		ci := CIConfig{
			Agents:      []string{"codex", "gemini"},
			ReviewTypes: []string{"security", "default"},
		}
		matrix := ci.ResolvedReviewMatrix()
		if len(matrix) != 4 {
			require.Condition(t, func() bool {
				return false
			}, "got %d entries, want 4", len(matrix))
		}
		// Cross-product: reviewTypes outer, agents inner
		want := []AgentReviewType{
			{"codex", "security"},
			{"gemini", "security"},
			{"codex", "default"},
			{"gemini", "default"},
		}
		for i, got := range matrix {
			if got != want[i] {
				assert.Condition(t, func() bool {
					return false
				}, "matrix[%d] = %v, want %v",
					i, got, want[i])
			}
		}
	})

	t.Run("uses reviews map when set", func(t *testing.T) {
		ci := CIConfig{
			Reviews: map[string][]string{
				"codex":  {"security", "default"},
				"gemini": {"default"},
			},
			// These should be ignored when Reviews is set
			Agents:      []string{"ignored"},
			ReviewTypes: []string{"ignored"},
		}
		matrix := ci.ResolvedReviewMatrix()
		if len(matrix) != 3 {
			require.Condition(t, func() bool {
				return false
			}, "got %d entries, want 3", len(matrix))
		}
		// Sort for deterministic comparison (map iteration order)
		sort.Slice(matrix, func(i, j int) bool {
			if matrix[i].Agent != matrix[j].Agent {
				return matrix[i].Agent < matrix[j].Agent
			}
			return matrix[i].ReviewType < matrix[j].ReviewType
		})
		want := []AgentReviewType{
			{"codex", "default"},
			{"codex", "security"},
			{"gemini", "default"},
		}
		for i, got := range matrix {
			if got != want[i] {
				assert.Condition(t, func() bool {
					return false
				}, "matrix[%d] = %v, want %v",
					i, got, want[i])
			}
		}
	})

	t.Run("defaults when empty", func(t *testing.T) {
		ci := CIConfig{}
		matrix := ci.ResolvedReviewMatrix()
		// Default: [""] x ["security"]
		if len(matrix) != 1 {
			require.Condition(t, func() bool {
				return false
			}, "got %d entries, want 1", len(matrix))
		}
		if matrix[0].Agent != "" || matrix[0].ReviewType != "security" {
			assert.Condition(t, func() bool {
				return false
			}, "got %v, want {\"\" security}", matrix[0])
		}
	})
}

func TestRepoCIConfigResolvedReviewMatrix(t *testing.T) {
	t.Run("returns nil when Reviews not set", func(t *testing.T) {
		ci := RepoCIConfig{
			Agents:      []string{"codex"},
			ReviewTypes: []string{"security"},
		}
		if matrix := ci.ResolvedReviewMatrix(); matrix != nil {
			assert.Condition(t, func() bool {
				return false
			}, "expected nil, got %v", matrix)
		}
	})

	t.Run("returns matrix from Reviews", func(t *testing.T) {
		ci := RepoCIConfig{
			Reviews: map[string][]string{
				"codex": {"security"},
			},
		}
		matrix := ci.ResolvedReviewMatrix()
		if len(matrix) != 1 {
			require.Condition(t, func() bool {
				return false
			}, "got %d entries, want 1", len(matrix))
		}
		if matrix[0].Agent != "codex" ||
			matrix[0].ReviewType != "security" {
			assert.Condition(t, func() bool {
				return false
			}, "got %v", matrix[0])
		}
	})

	t.Run("empty Reviews disables reviews", func(t *testing.T) {
		ci := RepoCIConfig{
			Reviews: map[string][]string{},
		}
		matrix := ci.ResolvedReviewMatrix()
		if matrix == nil {
			require.Condition(t, func() bool {
				return false
			}, "expected non-nil empty slice, got nil")
		}
		if len(matrix) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "expected 0 entries, got %d", len(matrix))
		}
	})

	t.Run("Reviews with all empty lists disables reviews", func(t *testing.T) {
		ci := RepoCIConfig{
			Reviews: map[string][]string{"codex": {}},
		}
		matrix := ci.ResolvedReviewMatrix()
		if matrix == nil {
			require.Condition(t, func() bool {
				return false
			}, "expected non-nil empty slice, got nil")
		}
		if len(matrix) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "expected 0 entries, got %d", len(matrix))
		}
	})
}

func TestResolvePostCommitReview(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{"no config file", "", "commit"},
		{"field not set", `agent = "claude-code"`, "commit"},
		{"explicit commit", `post_commit_review = "commit"`, "commit"},
		{"branch", `post_commit_review = "branch"`, "branch"},
		{"unknown value falls back to commit", `post_commit_review = "auto"`, "commit"},
		{"empty string falls back to commit", `post_commit_review = ""`, "commit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dir string
			if tt.config == "" {
				dir = t.TempDir()
			} else {
				dir = newTempRepo(t, tt.config)
			}
			got := ResolvePostCommitReview(dir)
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "ResolvePostCommitReview() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveReuseReviewSession(t *testing.T) {
	boolTrue := true
	boolFalse := false

	tests := []struct {
		name   string
		global *Config
		repo   string
		want   bool
	}{
		{
			name:   "default false",
			global: DefaultConfig(),
			want:   false,
		},
		{
			name:   "global true",
			global: &Config{ReuseReviewSession: &boolTrue},
			want:   true,
		},
		{
			name:   "repo overrides global true to false",
			global: &Config{ReuseReviewSession: &boolTrue},
			repo:   `reuse_review_session = false`,
			want:   false,
		},
		{
			name:   "repo true",
			global: &Config{ReuseReviewSession: &boolFalse},
			repo:   `reuse_review_session = true`,
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.repo != "" {
				writeRepoConfigStr(t, dir, tt.repo)
			}
			if got := ResolveReuseReviewSession(dir, tt.global); got != tt.want {
				require.Condition(t, func() bool {
					return false
				}, "ResolveReuseReviewSession() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveDisableCodexReviewSkills(t *testing.T) {
	tests := []struct {
		name   string
		global *Config
		want   bool
	}{
		{
			name: "default true",
			want: true,
		},
		{
			name:   "global false",
			global: &Config{Agent: AgentConfig{Codex: CodexConfig{DisableReviewSkills: false}}},
			want:   false,
		},
		{
			name:   "global true",
			global: &Config{Agent: AgentConfig{Codex: CodexConfig{DisableReviewSkills: true}}},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()

			got := ResolveDisableCodexReviewSkills(dir, tt.global)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveIgnoreCodexReviewUserConfig(t *testing.T) {
	tests := []struct {
		name   string
		global *Config
		want   bool
	}{
		{
			name: "default true",
			want: true,
		},
		{
			name:   "global false",
			global: &Config{Agent: AgentConfig{Codex: CodexConfig{IgnoreReviewUserConfig: false}}},
			want:   false,
		},
		{
			name:   "global true",
			global: &Config{Agent: AgentConfig{Codex: CodexConfig{IgnoreReviewUserConfig: true}}},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()

			got := ResolveIgnoreCodexReviewUserConfig(dir, tt.global)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveReuseReviewSessionLookback(t *testing.T) {
	tests := []struct {
		name   string
		global *Config
		repo   string
		want   int
	}{
		{
			name:   "default",
			global: DefaultConfig(),
			want:   0,
		},
		{
			name:   "global override",
			global: &Config{ReuseReviewSessionLookback: 25},
			want:   25,
		},
		{
			name:   "repo overrides global",
			global: &Config{ReuseReviewSessionLookback: 25},
			repo:   `reuse_review_session_lookback = 5`,
			want:   5,
		},
		{
			name:   "repo zero disables cap",
			global: &Config{ReuseReviewSessionLookback: 25},
			repo:   `reuse_review_session_lookback = 0`,
			want:   0,
		},
		{
			name:   "repo negative disables cap",
			global: &Config{ReuseReviewSessionLookback: 25},
			repo:   `reuse_review_session_lookback = -1`,
			want:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.repo != "" {
				writeRepoConfigStr(t, dir, tt.repo)
			}
			if got := ResolveReuseReviewSessionLookback(dir, tt.global); got != tt.want {
				require.Condition(t, func() bool {
					return false
				}, "ResolveReuseReviewSessionLookback() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolvedThrottleInterval(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"empty defaults to 1h", "", time.Hour},
		{"zero disables", "0", 0},
		{"valid duration", "30m", 30 * time.Minute},
		{"valid seconds", "3600s", time.Hour},
		{"invalid falls back to 1h", "not-a-duration", time.Hour},
		{"negative falls back to 1h", "-5m", time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ci := CIConfig{ThrottleInterval: tt.value}
			got := ci.ResolvedThrottleInterval()
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "ResolvedThrottleInterval() = %v, want %v",
					got, tt.want)
			}
		})
	}
}

func TestCIConfigReviewsFieldParsing(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	{
		err := os.WriteFile(configPath, []byte(`
[ci]
enabled = true
throttle_interval = "30m"

[ci.reviews]
codex = ["security", "default"]
gemini = ["default"]
`), 0o644)
		require.Condition(t, func() bool {
			return err == nil
		}, err)
	}

	cfg, err := LoadGlobalFrom(configPath)
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadGlobalFrom: %v", err)
	assert.Condition(t, func() bool {
		return cfg.CI.ThrottleInterval == "30m"
	}, "got throttle_interval %q, want %q",
		cfg.CI.ThrottleInterval, "30m")
	require.Condition(t, func() bool {
		return len(cfg.CI.Reviews) == 2
	}, "got %d review entries, want 2",
		len(cfg.CI.Reviews))

	codexTypes := cfg.CI.Reviews["codex"]
	assert.Condition(t, func() bool {
		return len(codexTypes) == 2 &&
			codexTypes[0] == "security" &&
			codexTypes[1] == "default"
	}, "got codex types %v", codexTypes)
}

func TestResolveCIIncludeCosts(t *testing.T) {
	t.Run("default false", func(t *testing.T) {
		assert.False(t, ResolveCIIncludeCosts(nil, &Config{}))
	})

	t.Run("global true", func(t *testing.T) {
		cfg := &Config{CI: CIConfig{IncludeCosts: true}}
		assert.True(t, ResolveCIIncludeCosts(nil, cfg))
	})

	t.Run("repo false overrides global true", func(t *testing.T) {
		repoFalse := false
		repoCfg := &RepoConfig{CI: RepoCIConfig{IncludeCosts: &repoFalse}}
		globalCfg := &Config{CI: CIConfig{IncludeCosts: true}}
		assert.False(t, ResolveCIIncludeCosts(repoCfg, globalCfg))
	})

	t.Run("repo true overrides global false", func(t *testing.T) {
		repoTrue := true
		repoCfg := &RepoConfig{CI: RepoCIConfig{IncludeCosts: &repoTrue}}
		assert.True(t, ResolveCIIncludeCosts(repoCfg, &Config{}))
	})
}

func TestIsThrottleBypassed(t *testing.T) {
	ci := CIConfig{
		ThrottleBypassUsers: []string{"wesm", "mariusvniekerk"},
	}

	tests := []struct {
		login string
		want  bool
	}{
		{"wesm", true},
		{"mariusvniekerk", true},
		{"Wesm", true}, // case-insensitive
		{"WESM", true}, // all caps
		{"MariusVNiekerk", true},
		{"someone-else", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.login, func(t *testing.T) {
			got := ci.IsThrottleBypassed(tt.login)
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "IsThrottleBypassed(%q) = %v, want %v",
					tt.login, got, tt.want)
			}
		})
	}

	t.Run("empty list", func(t *testing.T) {
		empty := CIConfig{}
		if empty.IsThrottleBypassed("wesm") {
			assert.Condition(t, func() bool {
				return false
			}, "expected false for empty bypass list")
		}
	})
}

func TestRepoCIConfigReviewsFieldParsing(t *testing.T) {
	tmpDir := newTempRepo(t, `
agent = "codex"

[ci]
agents = ["codex"]

[ci.reviews]
codex = ["security"]
gemini = ["default"]
`)
	cfg, err := LoadRepoConfig(tmpDir)
	require.Condition(t, func() bool {
		return err == nil
	}, "LoadRepoConfig: %v", err)
	require.Condition(t, func() bool {
		return len(cfg.CI.Reviews) == 2
	}, "got %d review entries, want 2",
		len(cfg.CI.Reviews))
}

func TestResolveMinSeverity(t *testing.T) {
	type resolverFunc func(explicit string, dir string, globalCfg *Config) (string, error)

	runTests := func(
		t *testing.T, name string, fn resolverFunc,
		configKey string, makeGlobal func(string) *Config,
	) {
		t.Run(name, func(t *testing.T) {
			tests := []struct {
				testName     string
				explicit     string
				repoConfig   string
				globalConfig *Config
				want         string
				wantErr      bool
			}{
				{"default when no config", "", "", nil, "", false},
				{"repo config when explicit empty", "", fmt.Sprintf(`%s = "high"`, configKey), nil, "high", false},
				{"explicit overrides repo config", "critical", fmt.Sprintf(`%s = "medium"`, configKey), nil, "critical", false},
				{"explicit normalization", "HIGH", "", nil, "high", false},
				{"invalid explicit", "bogus", "", nil, "", true},
				{"invalid repo config", "", fmt.Sprintf(`%s = "invalid"`, configKey), nil, "", true},
				{"global wins when repo empty", "", "", makeGlobal("medium"), "medium", false},
				{"repo wins over global", "", fmt.Sprintf(`%s = "high"`, configKey), makeGlobal("medium"), "high", false},
				{"explicit wins over both", "critical", fmt.Sprintf(`%s = "high"`, configKey), makeGlobal("medium"), "critical", false},
				{"empty everywhere returns empty", "", "", &Config{}, "", false},
			}
			for _, tt := range tests {
				t.Run(tt.testName, func(t *testing.T) {
					tmpDir := newTempRepo(t, tt.repoConfig)
					got, err := fn(tt.explicit, tmpDir, tt.globalConfig)
					if (err != nil) != tt.wantErr {
						assert.Condition(t, func() bool { return false }, "error = %v, wantErr %v", err, tt.wantErr)
					}
					if !tt.wantErr && got != tt.want {
						assert.Condition(t, func() bool { return false }, "got %q, want %q", got, tt.want)
					}
				})
			}
		})
	}

	runTests(t, "Fix", ResolveFixMinSeverity, "fix_min_severity",
		func(v string) *Config { return &Config{FixMinSeverity: v} })
	runTests(t, "Refine", ResolveRefineMinSeverity, "refine_min_severity",
		func(v string) *Config { return &Config{RefineMinSeverity: v} })
	runTests(t, "Review", ResolveReviewMinSeverity, "review_min_severity",
		func(v string) *Config { return &Config{ReviewMinSeverity: v} })
}

func TestSeverityInstruction(t *testing.T) {
	tests := []struct {
		name        string
		minSeverity string
		wantEmpty   bool
		wantSubstr  string
	}{
		{
			name:        "empty returns empty",
			minSeverity: "",
			wantEmpty:   true,
		},
		{
			name:        "low returns empty",
			minSeverity: "low",
			wantEmpty:   true,
		},
		{
			name:        "unknown returns empty",
			minSeverity: "bogus",
			wantEmpty:   true,
		},
		{
			name:        "medium",
			minSeverity: "medium",
			wantSubstr:  "Medium, High, and Critical",
		},
		{
			name:        "high",
			minSeverity: "high",
			wantSubstr:  "High and Critical",
		},
		{
			name:        "critical",
			minSeverity: "critical",
			wantSubstr:  "Only include Critical",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SeverityInstruction(tt.minSeverity)
			if tt.wantEmpty {
				if got != "" {
					assert.Condition(t, func() bool {
						return false
					}, "SeverityInstruction(%q) = %q, want empty",
						tt.minSeverity, got)
				}
				return
			}
			if got == "" {
				require.Condition(t, func() bool {
					return false
				}, "SeverityInstruction(%q) = empty, want non-empty",
					tt.minSeverity)
			}
			if !strings.Contains(got, tt.wantSubstr) {
				assert.Condition(t, func() bool {
					return false
				}, "SeverityInstruction(%q) = %q, missing %q",
					tt.minSeverity, got, tt.wantSubstr)
			}
			if !strings.Contains(got, SeverityThresholdMarker) {
				assert.Condition(t, func() bool {
					return false
				}, "SeverityInstruction(%q) missing threshold marker %q",
					tt.minSeverity, SeverityThresholdMarker)
			}
		})
	}
}

func TestIsMarkerOnlyOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"empty", "", false},
		{"whitespace only", "   \n\t  ", false},
		{"marker alone", "SEVERITY_THRESHOLD_MET", true},
		{"marker with surrounding whitespace", "  SEVERITY_THRESHOLD_MET\n", true},
		{"marker with newlines", "\n\nSEVERITY_THRESHOLD_MET\n\n", true},
		{"marker with trailing period", "SEVERITY_THRESHOLD_MET.", true},
		{"marker bold star", "**SEVERITY_THRESHOLD_MET**", true},
		{"marker bold underscore", "__SEVERITY_THRESHOLD_MET__", true},
		{"marker italic star", "*SEVERITY_THRESHOLD_MET*", true},
		{"marker italic underscore", "_SEVERITY_THRESHOLD_MET_", true},
		{"marker bold italic", "***SEVERITY_THRESHOLD_MET***", true},
		{"marker bold with period", "**SEVERITY_THRESHOLD_MET**.", false},
		{"marker as bullet", "- SEVERITY_THRESHOLD_MET", true},
		{"marker as star bullet", "* SEVERITY_THRESHOLD_MET", true},
		{"marker in fenced block", "```\nSEVERITY_THRESHOLD_MET\n```", true},
		{"marker in fenced block with language", "```text\nSEVERITY_THRESHOLD_MET\n```", true},
		{"marker plus prose finding", "SEVERITY_THRESHOLD_MET\n\nThe auth module leaks tokens.", false},
		{"marker plus narration before", "All findings below threshold.\nSEVERITY_THRESHOLD_MET", false},
		{"marker plus narration after", "SEVERITY_THRESHOLD_MET\nNo code changes needed.", false},
		{"marker buried in prose", "I checked the code and SEVERITY_THRESHOLD_MET applies here.", false},
		{"marker plus severity label", "SEVERITY_THRESHOLD_MET\n- High: critical bug", false},
		{"different text only", "All good, no issues.", false},
		{"marker substring inside another word", "PRESEVERITY_THRESHOLD_MET", false},
		{"marker split by newline (gemini quirk)", "SEVERITY_THRESHOLD\n_MET", true},
		{"marker split by double newline (gemini quirk)", "SEVERITY_THRESHOLD\n\n_MET", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsMarkerOnlyOutput(tt.output),
				"IsMarkerOnlyOutput(%q)", tt.output)
		})
	}
}

func TestCIConfig_ResolvedBatchTimeout(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		c := CIConfig{}
		assert.Equal(t, 15*time.Minute, c.ResolvedBatchTimeout())
	})
	t.Run("custom", func(t *testing.T) {
		c := CIConfig{BatchTimeout: "5m"}
		assert.Equal(t, 5*time.Minute, c.ResolvedBatchTimeout())
	})
	t.Run("disabled", func(t *testing.T) {
		c := CIConfig{BatchTimeout: "0"}
		assert.Equal(t, time.Duration(0), c.ResolvedBatchTimeout())
	})
	t.Run("invalid_falls_back", func(t *testing.T) {
		c := CIConfig{BatchTimeout: "garbage"}
		assert.Equal(t, 15*time.Minute, c.ResolvedBatchTimeout())
	})
	t.Run("negative_falls_back", func(t *testing.T) {
		c := CIConfig{BatchTimeout: "-5m"}
		assert.Equal(t, 15*time.Minute, c.ResolvedBatchTimeout())
	})
}

func TestAutoDesignReviewConfig_Load(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "config.toml")
	err := os.WriteFile(cfgFile, []byte(`
[auto_design_review]
enabled = true
hook_enabled = true
min_diff_lines = 20
large_diff_lines = 400
large_file_count = 8
trigger_paths = ["foo/**", "bar/**"]
skip_paths = ["**/*.md"]
trigger_message_patterns = ['\brefactor\b']
skip_message_patterns = ['^docs:']
classifier_timeout_seconds = 45
classifier_max_prompt_size = 32768
`), 0o644)
	require.NoError(t, err)

	cfg, err := LoadGlobalFrom(cfgFile)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.True(cfg.AutoDesignReview.Enabled)
	assert.True(cfg.AutoDesignReview.HookEnabled)
	assert.Equal(20, cfg.AutoDesignReview.MinDiffLines)
	assert.Equal(400, cfg.AutoDesignReview.LargeDiffLines)
	assert.Equal(8, cfg.AutoDesignReview.LargeFileCount)
	assert.Equal([]string{"foo/**", "bar/**"}, cfg.AutoDesignReview.TriggerPaths)
	assert.Equal([]string{"**/*.md"}, cfg.AutoDesignReview.SkipPaths)
	assert.Equal([]string{`\brefactor\b`}, cfg.AutoDesignReview.TriggerMessagePatterns)
	assert.Equal([]string{`^docs:`}, cfg.AutoDesignReview.SkipMessagePatterns)
	assert.Equal(45, cfg.AutoDesignReview.ClassifierTimeoutSeconds)
	assert.Equal(32768, cfg.AutoDesignReview.ClassifierMaxPromptSize)
}

func TestClassifyWorkflowConfig_Load(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "config.toml")
	err := os.WriteFile(cfgFile, []byte(`
classify_agent = "claude-code"
classify_model = "haiku"
classify_reasoning = "fast"
classify_backup_agent = "codex"
classify_backup_model = "o-mini"
`), 0o644)
	require.NoError(t, err)

	cfg, err := LoadGlobalFrom(cfgFile)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal("claude-code", cfg.ClassifyAgent)
	assert.Equal("haiku", cfg.ClassifyModel)
	assert.Equal("fast", cfg.ClassifyReasoning)
	assert.Equal("codex", cfg.ClassifyBackupAgent)
	assert.Equal("o-mini", cfg.ClassifyBackupModel)
}

func TestResolveAutoDesignEnabled_Disabled(t *testing.T) {
	tmp := t.TempDir()
	assert.False(t, ResolveAutoDesignEnabled(tmp, &Config{}))
}

func TestResolveAutoDesignEnabled_GlobalEnabled(t *testing.T) {
	tmp := t.TempDir()
	cfg := &Config{AutoDesignReview: AutoDesignReviewConfig{Enabled: true}}
	assert.True(t, ResolveAutoDesignEnabled(tmp, cfg))
}

func TestResolveAutoDesignEnabled_RepoEnablesOverGlobalOff(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".roborev.toml"),
		[]byte("[auto_design_review]\nenabled = true\n"), 0o644))
	cfg := &Config{AutoDesignReview: AutoDesignReviewConfig{Enabled: false}}
	assert.True(t, ResolveAutoDesignEnabled(tmp, cfg))
}

func TestResolveAutoDesignEnabled_RepoDisablesOverGlobalOn(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".roborev.toml"),
		[]byte("[auto_design_review]\nenabled = false\n"), 0o644))
	cfg := &Config{AutoDesignReview: AutoDesignReviewConfig{Enabled: true}}
	assert.False(t, ResolveAutoDesignEnabled(tmp, cfg), "repo explicit false must override global true")
}

func TestResolveAutoDesignEnabled_RepoUnsetInherits(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".roborev.toml"),
		[]byte("[auto_design_review]\nmin_diff_lines = 5\n"), 0o644))
	cfg := &Config{AutoDesignReview: AutoDesignReviewConfig{Enabled: true}}
	assert.True(t, ResolveAutoDesignEnabled(tmp, cfg), "repo without explicit enabled must inherit global")
}

func TestResolveAutoDesignHeuristics_Defaults(t *testing.T) {
	h := ResolveAutoDesignHeuristics(t.TempDir(), &Config{})
	assert := assert.New(t)
	assert.Equal(10, h.MinDiffLines)
	assert.Equal(500, h.LargeDiffLines)
	assert.NotEmpty(h.TriggerPaths)
}

func TestResolveAutoDesignHeuristics_GlobalOverride(t *testing.T) {
	cfg := &Config{AutoDesignReview: AutoDesignReviewConfig{
		MinDiffLines:   20,
		LargeDiffLines: 1000,
		TriggerPaths:   []string{"foo/**"},
	}}
	h := ResolveAutoDesignHeuristics(t.TempDir(), cfg)
	assert := assert.New(t)
	assert.Equal(20, h.MinDiffLines)
	assert.Equal(1000, h.LargeDiffLines)
	assert.Equal([]string{"foo/**"}, h.TriggerPaths)
}

func TestAutoDesignHeuristics_Validate_Defaults(t *testing.T) {
	require.NoError(t, DefaultAutoDesignHeuristics().Validate())
}

func TestAutoDesignHeuristics_Validate_InvalidTriggerGlob(t *testing.T) {
	h := DefaultAutoDesignHeuristics()
	h.TriggerPaths = []string{"["}
	err := h.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trigger_paths")
}

func TestAutoDesignHeuristics_Validate_InvalidSkipGlob(t *testing.T) {
	h := DefaultAutoDesignHeuristics()
	h.SkipPaths = []string{"["}
	err := h.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skip_paths")
}

func TestAutoDesignHeuristics_Validate_InvalidTriggerRegex(t *testing.T) {
	h := DefaultAutoDesignHeuristics()
	h.TriggerMessagePatterns = []string{"(unclosed"}
	err := h.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trigger_message_patterns")
}

func TestAutoDesignHeuristics_Validate_InvalidSkipRegex(t *testing.T) {
	h := DefaultAutoDesignHeuristics()
	h.SkipMessagePatterns = []string{"(unclosed"}
	err := h.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skip_message_patterns")
}

func TestResolveGlobalAutoDesignHeuristics_IgnoresPerRepo(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".roborev.toml"),
		[]byte("[auto_design_review]\ntrigger_paths = [\"[\"]\n"), 0o644))
	cfg := &Config{AutoDesignReview: AutoDesignReviewConfig{
		TriggerPaths: []string{"foo/**"},
	}}
	h := ResolveGlobalAutoDesignHeuristics(cfg)
	assert.Equal(t, []string{"foo/**"}, h.TriggerPaths,
		"ResolveGlobalAutoDesignHeuristics must ignore per-repo overlays from cwd")
	require.NoError(t, h.Validate())
}

func TestResolveClassifyAgent_Default(t *testing.T) {
	RegisterClassifyAgentValidator(nil)
	name, err := ResolveClassifyAgent("", t.TempDir(), &Config{})
	require.NoError(t, err)
	assert.Equal(t, "claude-code", name)
}

func TestResolveClassifyAgent_CLIOverride(t *testing.T) {
	RegisterClassifyAgentValidator(nil)
	name, err := ResolveClassifyAgent("codex", t.TempDir(), &Config{})
	require.NoError(t, err)
	assert.Equal(t, "codex", name)
}

func TestResolveClassifyAgent_RepoOverridesGlobal(t *testing.T) {
	RegisterClassifyAgentValidator(nil)
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".roborev.toml"),
		[]byte("classify_agent = \"codex\"\n"), 0o644))
	name, err := ResolveClassifyAgent("", tmp, &Config{ClassifyAgent: "claude-code"})
	require.NoError(t, err)
	assert.Equal(t, "codex", name)
}

func TestResolveClassifyAgent_ValidatorReject(t *testing.T) {
	RegisterClassifyAgentValidator(func(name string) error {
		if name == "gemini" {
			return fmt.Errorf("agent %q does not support structured output; valid: claude-code, codex", name)
		}
		return nil
	})
	t.Cleanup(func() { RegisterClassifyAgentValidator(nil) })
	_, err := ResolveClassifyAgent("gemini", t.TempDir(), &Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "structured output")
}

func TestResolveBackupClassifyAgent_EmptyOK(t *testing.T) {
	RegisterClassifyAgentValidator(func(name string) error {
		return fmt.Errorf("should not be called for empty backup")
	})
	t.Cleanup(func() { RegisterClassifyAgentValidator(nil) })
	name, err := ResolveBackupClassifyAgent(t.TempDir(), &Config{})
	require.NoError(t, err)
	assert.Empty(t, name)
}

func TestResolveBackupClassifyAgent_ValidatorReject(t *testing.T) {
	RegisterClassifyAgentValidator(func(name string) error {
		return fmt.Errorf("agent %q does not support structured output", name)
	})
	t.Cleanup(func() { RegisterClassifyAgentValidator(nil) })
	_, err := ResolveBackupClassifyAgent(t.TempDir(), &Config{ClassifyBackupAgent: "gemini"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "structured output")
}

func TestResolveBackupClassifyAgent_ValidatorAccepts(t *testing.T) {
	RegisterClassifyAgentValidator(func(name string) error { return nil })
	t.Cleanup(func() { RegisterClassifyAgentValidator(nil) })
	name, err := ResolveBackupClassifyAgent(t.TempDir(), &Config{ClassifyBackupAgent: "claude-code"})
	require.NoError(t, err)
	assert.Equal(t, "claude-code", name)
}

func TestResolveClassifyReasoning_Default(t *testing.T) {
	lvl := ResolveClassifyReasoning("", t.TempDir(), &Config{})
	assert.Equal(t, "fast", lvl)
}

func TestResolveClassifyReasoning_CLIOverride(t *testing.T) {
	lvl := ResolveClassifyReasoning("standard", t.TempDir(), &Config{ClassifyReasoning: "fast"})
	assert.Equal(t, "standard", lvl)
}

func TestResolveClassifierTimeout_Default(t *testing.T) {
	d := ResolveClassifierTimeout(t.TempDir(), &Config{})
	assert.Equal(t, 60*time.Second, d)
}

func TestResolveClassifierTimeout_GlobalOverride(t *testing.T) {
	cfg := &Config{AutoDesignReview: AutoDesignReviewConfig{ClassifierTimeoutSeconds: 45}}
	d := ResolveClassifierTimeout(t.TempDir(), cfg)
	assert.Equal(t, 45*time.Second, d)
}

func TestResolveClassifierMaxPromptSize_Default(t *testing.T) {
	n := ResolveClassifierMaxPromptSize(t.TempDir(), &Config{})
	assert.Equal(t, 20*1024, n)
}

func TestResolveClassifierMaxPromptSize_RepoOverride(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".roborev.toml"),
		[]byte("[auto_design_review]\nclassifier_max_prompt_size = 8192\n"), 0o644))
	n := ResolveClassifierMaxPromptSize(tmp, &Config{})
	assert.Equal(t, 8192, n)
}

func TestResolveAutoDesignHeuristics_EmptyListClearsDefaults(t *testing.T) {
	// Explicit empty TOML list (trigger_paths = []) must clear the
	// built-in defaults. Using len(...) > 0 in the overlay would
	// conflate empty-override with unset-inherit and leave the baked-in
	// rules active, preventing users from disabling a heuristic family.
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".roborev.toml"),
		[]byte(`[auto_design_review]
trigger_paths = []
`), 0o644))
	h := ResolveAutoDesignHeuristics(tmp, &Config{})
	assert.Empty(t, h.TriggerPaths, "explicit empty list must clear defaults")
	// Sibling lists untouched by the repo config still inherit.
	assert.NotEmpty(t, h.SkipPaths)
}

func TestResolveAutoDesignHeuristics_RepoWins(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".roborev.toml"),
		[]byte(`[auto_design_review]
min_diff_lines = 5
trigger_paths = ["bar/**"]
`), 0o644))
	cfg := &Config{AutoDesignReview: AutoDesignReviewConfig{
		MinDiffLines: 20,
		TriggerPaths: []string{"foo/**"},
	}}
	h := ResolveAutoDesignHeuristics(tmp, cfg)
	assert.Equal(t, 5, h.MinDiffLines)
	assert.Equal(t, []string{"bar/**"}, h.TriggerPaths)
}

func TestClassifyWorkflowConfig_FoundByReflectionTag(t *testing.T) {
	cfg := &Config{
		ClassifyAgent:       "claude-code",
		ClassifyModel:       "haiku",
		ClassifyBackupAgent: "codex",
		ClassifyBackupModel: "o-mini",
	}
	assert.Equal(t, "claude-code", lookupFieldByTag(reflect.ValueOf(*cfg), "classify_agent"))
	assert.Equal(t, "haiku", lookupFieldByTag(reflect.ValueOf(*cfg), "classify_model"))
	assert.Equal(t, "codex", lookupFieldByTag(reflect.ValueOf(*cfg), "classify_backup_agent"))
	assert.Equal(t, "o-mini", lookupFieldByTag(reflect.ValueOf(*cfg), "classify_backup_model"))
}

func TestResolveKataContextDefaults(t *testing.T) {
	kc := ResolveKataContext(t.TempDir(), &Config{})
	assert.Equal(t, KataModeOff, kc.Mode)
	assert.Equal(t, 50000, kc.MaxChars)
}

func TestResolveKataContextGlobal(t *testing.T) {
	g := &Config{}
	g.KataContext.Mode = "current"
	g.KataContext.MaxChars = 1234
	kc := ResolveKataContext(t.TempDir(), g)
	assert.Equal(t, KataModeCurrent, kc.Mode)
	assert.Equal(t, 1234, kc.MaxChars)
}

func TestResolveKataContextRepoOverridesGlobal(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"),
		[]byte("[kata_context]\nmode = \"open\"\n"), 0o644))
	g := &Config{}
	g.KataContext.Mode = "current"
	g.KataContext.MaxChars = 999
	kc := ResolveKataContext(dir, g)
	assert.Equal(t, KataModeOpen, kc.Mode) // repo wins
	assert.Equal(t, 999, kc.MaxChars)      // repo unset -> global kept
}

func TestResolveKataContextNormalizesUnknown(t *testing.T) {
	g := &Config{}
	g.KataContext.Mode = "bogus"
	g.KataContext.MaxChars = -5
	kc := ResolveKataContext(t.TempDir(), g)
	assert.Equal(t, KataModeOff, kc.Mode)
	assert.Equal(t, 50000, kc.MaxChars) // <=0 clamps to default
}

func TestHookConfigKataFields(t *testing.T) {
	var cfg RepoConfig
	_, err := toml.Decode(`
[[hooks]]
event = "review.*"
type = "kata"
project = "myproj"
labels = ["from-review"]
priority = 3
`, &cfg)
	require.NoError(t, err)
	require.Len(t, cfg.Hooks, 1)
	assert.Equal(t, "kata", cfg.Hooks[0].Type)
	assert.Equal(t, "myproj", cfg.Hooks[0].Project)
	assert.Equal(t, []string{"from-review"}, cfg.Hooks[0].Labels)
	require.NotNil(t, cfg.Hooks[0].Priority)
	assert.Equal(t, 3, *cfg.Hooks[0].Priority)
}
