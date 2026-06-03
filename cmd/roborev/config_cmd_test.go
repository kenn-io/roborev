package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupConfigFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.toml")
}

const (
	errGitStub = "git unavailable stub"
	errCwdStub = "cwd failed stub"
)

func captureOutput(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w

	defer func() { os.Stdout = old }()

	fn()

	w.Close()
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}

func readTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	raw := make(map[string]any)
	_, err := toml.DecodeFile(path, &raw)
	require.NoError(t, err, "read TOML %s", path)
	return raw
}

// getNestedValue traverses a dot-separated key path in a nested map.
func getNestedValue(t *testing.T, raw map[string]any, dotKey string) any {
	t.Helper()
	parts := strings.Split(dotKey, ".")
	var current any = raw
	for _, part := range parts {
		m, ok := current.(map[string]any)
		assert.True(t, ok)
		current = m[part]
	}
	return current
}

func assertConfigValue(t *testing.T, path, dotKey string, expected any) {
	t.Helper()
	raw := readTOML(t, path)
	val := getNestedValue(t, raw, dotKey)
	assert.Equal(t, expected, val, dotKey)
}

func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	require.Error(t, err, "expected error, got nil")
	require.ErrorContains(t, err, want)
}

// stubRepoResolver implements RepoResolver for testing.
type stubRepoResolver struct {
	gitRoot    string
	gitErr     error
	workingDir string
	workingErr error
}

func (s *stubRepoResolver) RepoRoot() (string, error) {
	return s.gitRoot, s.gitErr
}

func (s *stubRepoResolver) WorkingDir() (string, error) {
	return s.workingDir, s.workingErr
}

func (s *stubRepoResolver) SetGitRoot(path string) {
	s.gitRoot = path
	s.gitErr = nil
}

func (s *stubRepoResolver) SetGitError(err error) {
	s.gitErr = err
}

func (s *stubRepoResolver) SetWorkingDir(path string) {
	s.workingDir = path
	s.workingErr = nil
}

func (s *stubRepoResolver) SetWorkingDirError(err error) {
	s.workingErr = err
}

// createFakeGitRepo creates a temporary directory with a .git subdirectory,
// simulating a repository root without running git init.
func createFakeGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	err := os.Mkdir(gitDir, 0o755)
	require.NoError(t, err, "create .git dir: %v", err)
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	return dir
}

type configEnv struct {
	DataDir  string
	RepoDir  string
	Resolver *stubRepoResolver
}

func setupConfigEnv(t *testing.T, globalTOML, localTOML string) configEnv {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)

	if globalTOML != "" {
		globalPath := filepath.Join(dataDir, "config.toml")
		require.NoError(t, os.WriteFile(globalPath, []byte(globalTOML), 0o644), "write global config")
	}

	repoDir := createFakeGitRepo(t)
	if localTOML != "" {
		localPath := filepath.Join(repoDir, ".roborev.toml")
		require.NoError(t, os.WriteFile(localPath, []byte(localTOML), 0o644), "write local config")
	}

	resolver := &stubRepoResolver{}
	resolver.SetGitRoot(repoDir)

	return configEnv{DataDir: dataDir, RepoDir: repoDir, Resolver: resolver}
}

func TestDetermineScope(t *testing.T) {
	tests := []struct {
		name       string
		globalFlag bool
		localFlag  bool
		want       configScope
		wantErr    bool
	}{
		{
			name:       "merged default",
			globalFlag: false,
			localFlag:  false,
			want:       scopeMerged,
		},
		{
			name:       "global",
			globalFlag: true,
			localFlag:  false,
			want:       scopeGlobal,
		},
		{
			name:       "local",
			globalFlag: false,
			localFlag:  true,
			want:       scopeLocal,
		},
		{
			name:       "conflicting flags",
			globalFlag: true,
			localFlag:  true,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := determineScope(tt.globalFlag, tt.localFlag)
			if tt.wantErr {
				require.Error(t, err, "expected error")
				return
			}
			require.NoError(t, err, "determineScope returned error: %v", err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestRepoRoot(t *testing.T) {
	t.Run("uses git resolver when available", func(t *testing.T) {
		resolver := &stubRepoResolver{}
		resolver.SetGitRoot("/tmp/from-git")

		got, err := repoRoot(resolver)
		require.NoError(t, err)
		require.Equal(t, "/tmp/from-git", got)
	})

	t.Run("falls back to filesystem when git resolver fails", func(t *testing.T) {
		repoDir := createFakeGitRepo(t)
		nestedDir := filepath.Join(repoDir, "nested", "deeper")
		require.NoError(t, os.MkdirAll(nestedDir, 0o755), "create nested dir")

		resolver := &stubRepoResolver{}
		resolver.SetGitError(errors.New(errGitStub))
		resolver.SetWorkingDir(nestedDir)

		got, err := repoRoot(resolver)
		require.NoError(t, err)
		require.Equal(t, repoDir, got)
	})

	t.Run("optional lookup returns empty when not in repo", func(t *testing.T) {
		resolver := &stubRepoResolver{}
		resolver.SetGitError(errors.New(errGitStub))
		resolver.SetWorkingDir(t.TempDir())

		got, err := repoRoot(resolver)
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("ignores invalid git metadata in parent directory", func(t *testing.T) {
		parent := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(parent, ".git"), 0o755))
		nested := filepath.Join(parent, "nested")
		require.NoError(t, os.Mkdir(nested, 0o755))

		resolver := &stubRepoResolver{}
		resolver.SetGitError(errors.New(errGitStub))
		resolver.SetWorkingDir(nested)

		got, err := repoRoot(resolver)
		require.NoError(t, err)
		require.Empty(t, got)
	})
}

func TestRequireRepoRoot(t *testing.T) {
	t.Run("returns not repo error when required and missing", func(t *testing.T) {
		resolver := &stubRepoResolver{}
		resolver.SetGitError(errors.New(errGitStub))
		resolver.SetWorkingDir(t.TempDir())

		_, err := requireRepoRoot(resolver)
		require.Error(t, err, "expected error")
		require.ErrorIs(t, err, errNotGitRepository)
	})

	t.Run("surfaces resolver errors", func(t *testing.T) {
		resolver := &stubRepoResolver{}
		resolver.SetGitError(errors.New(errGitStub))
		resolver.SetWorkingDirError(errors.New(errCwdStub))

		_, err := requireRepoRoot(resolver)
		require.ErrorContains(t, err, "determine repository root: "+errCwdStub)
	})
}

func TestGetValueForScopeMergedPrefersLocal(t *testing.T) {
	env := setupConfigEnv(t, "review_agent = \"global-agent\"\n", "review_agent = \"local-agent\"\n")

	nestedDir := filepath.Join(env.RepoDir, "a", "b")
	require.NoError(t, os.MkdirAll(nestedDir, 0o755), "create nested dir")

	env.Resolver.SetGitError(errors.New(errGitStub))
	env.Resolver.SetWorkingDir(nestedDir)

	got, err := getValueForScope(env.Resolver, "review_agent", scopeMerged)
	require.NoError(t, err)
	require.Equal(t, "local-agent", got)
}

func TestGetValueForScopeMergedRepoResolutionError(t *testing.T) {
	env := setupConfigEnv(t, "", "")

	env.Resolver.SetGitError(errors.New(errGitStub))
	env.Resolver.SetWorkingDirError(errors.New(errCwdStub))

	_, err := getValueForScope(env.Resolver, "review_agent", scopeMerged)
	require.ErrorContains(t, err, "determine repository root: "+errCwdStub)
}

func TestListMergedConfigRepoResolutionError(t *testing.T) {
	env := setupConfigEnv(t, "", "")

	env.Resolver.SetGitError(errors.New(errGitStub))
	env.Resolver.SetWorkingDirError(errors.New(errCwdStub))

	err := listMergedConfig(env.Resolver, false)
	require.ErrorContains(t, err, "determine repository root: "+errCwdStub)
}

func TestSetConfigKey(t *testing.T) {
	path := setupConfigFile(t)

	tests := []struct {
		name     string
		key      string
		val      string
		expected any
	}{
		{"String", "default_agent", "gemini", "gemini"},
		{"Integer", "max_workers", "8", int64(8)},
		{"Boolean", "sync.enabled", "true", true},
		{"NestedBoolean", "advanced.tasks_enabled", "true", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, setConfigKey(path, tt.key, tt.val, true), "setConfigKey failed")
			assertConfigValue(t, path, tt.key, tt.expected)
		})
	}

	t.Run("Persistence", func(t *testing.T) {
		// Previous values should still be present after multiple sets.
		assertConfigValue(t, path, "default_agent", "gemini")
		assertConfigValue(t, path, "max_workers", int64(8))
		assertConfigValue(t, path, "sync.enabled", true)
		assertConfigValue(t, path, "advanced.tasks_enabled", true)
	})
}

func TestSetConfigKeyNestedCreation(t *testing.T) {
	path := setupConfigFile(t)

	require.NoError(t, setConfigKey(path, "ci.poll_interval", "10m", true), "setConfigKey nested failed")
	assertConfigValue(t, path, "ci.poll_interval", "10m")
}

func TestSetConfigKeyInvalidKey(t *testing.T) {
	path := setupConfigFile(t)

	err := setConfigKey(path, "nonexistent_key", "value", true)
	require.Error(t, err, "expected error for invalid key")
}

func TestSetConfigKeySlice(t *testing.T) {
	path := setupConfigFile(t)

	require.NoError(t, setConfigKey(path, "ci.repos", "org/repo1,org/repo2", true), "setConfigKey slice")

	raw := readTOML(t, path)
	repos, ok := getNestedValue(t, raw, "ci.repos").([]any)
	require.True(t, ok, "ci.repos is not a slice: %v (%T)", getNestedValue(t, raw, "ci.repos"), getNestedValue(t, raw, "ci.repos"))
	require.Len(t, repos, 2)
}

func TestSetConfigKeySliceEmpty(t *testing.T) {
	path := setupConfigFile(t)

	// Seed with a non-empty slice first.
	require.NoError(t, setConfigKey(path, "ci.repos", "org/repo1,org/repo2", true), "setConfigKey seed")

	// Clear the slice by setting it to an empty string.
	require.NoError(t, setConfigKey(path, "ci.repos", "", true), "setConfigKey empty")

	raw := readTOML(t, path)
	repos := getNestedValue(t, raw, "ci.repos")
	slice, ok := repos.([]any)
	require.True(t, ok, "ci.repos is not a slice after clearing: %v (%T)", repos, repos)
	require.Empty(t, slice)
}

func TestSetConfigKeyRepoConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".roborev.toml")

	require.NoError(t, setConfigKey(path, "agent", "claude-code", false), "setConfigKey repo failed")
	assertConfigValue(t, path, "agent", "claude-code")
}

func TestSetConfigKeyRepoConfigWritesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".roborev.toml")

	if err := setConfigKey(path, "agent", "claude-code", false); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "setConfigKey repo: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "read config: %v", err)
	}
	got := string(data)

	for _, want := range []string{
		"# Default agent for this repo when no workflow-specific agent is set.\n",
		"agent = 'claude-code'",
	} {
		if !strings.Contains(got, want) {
			require.Condition(t, func() bool {
				return false
			}, "repo config missing %q:\n%s", want, got)
		}
	}
}

// Note: ACP config is now supported at both global and repo level.
// This test was removed as repo-level ACP config is now valid.

func TestSetConfigKeyGlobalWritesComments(t *testing.T) {
	path := setupConfigFile(t)

	if err := setConfigKey(path, "default_agent", "codex", true); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "setConfigKey: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "read config: %v", err)
	}
	got := string(data)

	for _, want := range []string{
		"# Default agent when no workflow-specific agent is set.\n",
		"default_agent = 'codex'",
		"# Hide closed reviews by default in the TUI queue.\n",
	} {
		if !strings.Contains(got, want) {
			require.Condition(t, func() bool {
				return false
			}, "global config missing %q:\n%s", want, got)
		}
	}
}

func TestSetRawMapKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  any
		path string // dot-path to check in the resulting map
		want any
	}{
		{
			name: "SimpleKey",
			key:  "foo",
			val:  "bar",
			path: "foo",
			want: "bar",
		},
		{
			name: "NestedKey",
			key:  "a.b.c",
			val:  42,
			path: "a.b.c",
			want: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := make(map[string]any)
			setRawMapKey(m, tt.key, tt.val)
			got := getNestedValue(t, m, tt.path)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestGetValueForScopeMergedMalformedLocalConfig(t *testing.T) {
	env := setupConfigEnv(t, `review_agent = "global-agent"\n`, "invalid toml [[[")

	_, err := getValueForScope(env.Resolver, "review_agent", scopeMerged)
	require.ErrorContains(t, err, "load repo config")
}

func TestListMergedConfigMalformedLocalConfig(t *testing.T) {
	env := setupConfigEnv(t, "", "invalid toml [[[")

	err := listMergedConfig(env.Resolver, false)
	require.ErrorContains(t, err, "load repo config")
}

func TestListGlobalConfigExplicitKeys(t *testing.T) {
	env := setupConfigEnv(t, strings.Join([]string{
		`max_workers = 4`,
		`review_context_count = 0`,
		``,
		`[sync]`,
		`enabled = false`,
	}, "\n")+"\n", "")

	_ = env

	// Capture stdout
	output := captureOutput(t, listGlobalConfig)
	// Explicit default-valued key should be shown
	require.Contains(t, output, "max_workers=4")

	// Explicit zero key should be shown
	require.Contains(t, output, "review_context_count=0")

	// Explicit false key should be shown
	require.Contains(t, output, "sync.enabled=false")

	// Non-explicit default key (default_agent) should NOT be shown
	require.NotContains(t, output, "default_agent=")
}

func TestListLocalConfigExplicitKeys(t *testing.T) {
	env := setupConfigEnv(t, "", strings.Join([]string{
		`agent = "claude-code"`,
		`review_context_count = 0`,
	}, "\n")+"\n")

	// Capture stdout
	output := captureOutput(t, func() error {
		return listLocalConfig(env.Resolver)
	})
	// Explicit key should be shown
	require.Contains(t, output, "agent=claude-code")

	// Explicit zero key should be shown
	require.Contains(t, output, "review_context_count=0")

	// Non-explicit keys should NOT be shown (review_guidelines was not set)
	require.NotContains(t, output, "review_guidelines=")
}

func TestGetValueForScopeMergedRepoOnlyKeyNotSet(t *testing.T) {
	env := setupConfigEnv(t, `default_agent = "codex"\n`, "")

	// No repo (no .git dir)
	env.Resolver.SetGitError(errors.New(errGitStub))
	env.Resolver.SetWorkingDir(t.TempDir())

	// "agent" is a repo-only key — should not fall through to global config
	_, err := getValueForScope(env.Resolver, "agent", scopeMerged)
	require.ErrorContains(t, err, "not set in local config")
}
