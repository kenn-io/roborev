package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

type strictReviewTestAgent struct{ *TestAgent }

func (a *strictReviewTestAgent) WithStrictReview(config.ReviewToolPolicy, string) Agent { return a }

func TestReviewToolPolicySelection(t *testing.T) {
	strict := &config.ReviewToolPolicy{Mode: "read-only"}
	require.NoError(t, ValidateReviewToolSelection(strict, NewClaudeAgent("unused"), false))
	capable := &strictReviewTestAgent{NewTestAgent()}
	require.NoError(t, ValidateReviewToolSelection(nil, NewTestAgent(), false))
	require.NoError(t, ValidateReviewToolSelection(nil, NewTestAgent(), true))
	require.NoError(t, ValidateReviewToolSelection(strict, capable, false))
	require.ErrorContains(t, ValidateReviewToolSelection(strict, capable, true), "agentic")
	require.ErrorContains(t, ValidateReviewToolSelection(&config.ReviewToolPolicy{}, capable, false), "mode")
	require.ErrorContains(t, ValidateReviewToolSelection(strict, nil, false), "agent")
	// Only Claude supports the strict native tool boundary.
	for _, name := range []string{"codex", "pi", "test"} {
		t.Run(name, func(t *testing.T) {
			a, err := Get(name)
			require.NoError(t, err)
			require.ErrorContains(t, ValidateReviewToolSelection(strict, a, false), "does not support strict review tools")
		})
	}
}

func TestReviewToolPolicyBackup(t *testing.T) {
	strict := &config.ReviewToolPolicy{Mode: "read-only"}
	resolution := WorkflowConfig{RepoConfig: &config.RepoConfig{}, GlobalConfig: config.DefaultConfig(), BackupAgent: "test"}
	require.NoError(t, ValidateReviewToolBackup(nil, resolution, "claude-code"))
	require.ErrorContains(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "invalid backup agent")
	resolution.BackupAgent = "claude"
	require.NoError(t, ValidateReviewToolBackup(strict, resolution, "claude-code"))
	resolution.BackupAgent = ""
	require.NoError(t, ValidateReviewToolBackup(strict, resolution, "claude-code"))
	resolution.BackupAgent = "no-such-backup"
	require.ErrorContains(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "resolve backup agent")
	resolution.BackupAgent = "acp.configured"
	resolution.RepoConfig.ACP = config.ACPAgentConfigs{"configured": {Command: "not-installed"}}
	require.ErrorContains(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "does not support strict review tools")
}

func TestReviewToolPolicyPreparation(t *testing.T) {
	a := NewClaudeAgent("unused").WithSessionID("old-session")
	unchanged, cleanup, err := PrepareReviewToolPolicy(a, nil, nil, t.TempDir(), true)
	require.NoError(t, err)
	require.Same(t, a, unchanged)
	cleanup()

	prepared, cleanup, err := PrepareReviewToolPolicy(a, &config.ReviewToolPolicy{Mode: "read-only"}, nil, t.TempDir(), false)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	strict := prepared.(*ClaudeAgent)
	assert.Empty(t, strict.SessionID)
	assert.Contains(t, strict.CommandLine(), "--restricted")
	path := strict.strictMCPConfigPath
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{}}`, string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	info, err = os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
	assert.Equal(t, "old-session", a.(*ClaudeAgent).SessionID)
	cleanup()
	_, err = os.Stat(filepath.Dir(path))
	require.ErrorIs(t, err, os.ErrNotExist)
	cleanup()
}

func TestEmptyServerPreparationBoundary(t *testing.T) {
	cfg := config.DefaultConfig()
	// Empty grants must prepare without an operator server configuration.
	cfg.ReviewMCPConfig = filepath.Join(t.TempDir(), "missing-operator-mcp.json")
	policy := &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"sources": {}}}
	a := NewClaudeAgent("unused")
	prepared, cleanup, err := PrepareReviewToolPolicy(a, policy, cfg, t.TempDir(), false)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	strict := prepared.(*ClaudeAgent)
	require.NotNil(t, strict.strictPolicy)
	assert.Empty(t, strict.strictPolicy.MCP)
	assert.Contains(t, strict.CommandLine(), "--restricted")
	data, err := os.ReadFile(strict.strictMCPConfigPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{}}`, string(data))
	cleanup()
	_, err = os.Stat(filepath.Dir(strict.strictMCPConfigPath))
	require.ErrorIs(t, err, os.ErrNotExist)

	// Validate server names before discarding their empty grants.
	invalid := &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"": {}}}
	rejected, cleanupInvalid, err := PrepareReviewToolPolicy(a, invalid, cfg, t.TempDir(), false)
	t.Cleanup(cleanupInvalid)
	require.ErrorIs(t, err, ErrReviewToolPolicy)
	require.ErrorContains(t, err, "invalid review MCP server name")
	assert.Nil(t, rejected)
}

func TestReviewToolPolicyPreparationRefusesInvalid(t *testing.T) {
	for _, tc := range []struct {
		name    string
		p       *config.ReviewToolPolicy
		a       Agent
		agentic bool
	}{
		{name: "mode", p: &config.ReviewToolPolicy{}, a: NewClaudeAgent("unused")},
		{name: "unsupported", p: &config.ReviewToolPolicy{Mode: "read-only"}, a: NewTestAgent()},
		{name: "agentic", p: &config.ReviewToolPolicy{Mode: "read-only"}, a: NewClaudeAgent("unused"), agentic: true},
		{name: "mcp", p: &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"archive": {"lookup"}}}, a: NewClaudeAgent("unused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cleanup, err := PrepareReviewToolPolicy(tc.a, tc.p, nil, t.TempDir(), tc.agentic)
			require.ErrorIs(t, err, ErrReviewToolPolicy)
			cleanup()
		})
	}
}

func TestReviewToolPolicyConfigLocation(t *testing.T) {
	for _, kind := range []string{"checkout", "descendant", "relative", "symlink-inside", "checkout-alias", "external", "external-symlink", "relative-checkout", "empty-checkout", "case-checkout", "case-temp-root"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			checkout := filepath.Join(base, "checkout")
			inside := filepath.Join(checkout, "tmp-private-marker")
			external := filepath.Join(base, "checkout-other")
			require.NoError(t, os.MkdirAll(inside, 0o700))
			require.NoError(t, os.Mkdir(external, 0o700))
			actualCheckout := checkout
			tempRoot := inside
			allowed := false
			t.Chdir(base)
			switch kind {
			case "checkout":
				tempRoot = checkout
			case "relative":
				tempRoot = filepath.Join("checkout", "tmp-private-marker")
			case "checkout-alias", "symlink-inside", "external-symlink":
				target := inside
				switch kind {
				case "checkout-alias":
					target = checkout
				case "external-symlink":
					target, allowed = external, true
				}
				alias := filepath.Join(base, "alias")
				err := os.Symlink(target, alias)
				if err != nil && runtime.GOOS == "windows" {
					t.Skip("symlink creation requires Windows privileges")
				}
				require.NoError(t, err)
				if kind == "checkout-alias" {
					actualCheckout = alias
				} else {
					tempRoot = alias
				}
			case "external":
				tempRoot, allowed = external, true
			case "relative-checkout":
				actualCheckout = "checkout"
				tempRoot, allowed = external, true
			case "empty-checkout":
				t.Chdir(checkout)
				actualCheckout = ""
			case "case-checkout", "case-temp-root":
				alias := filepath.Join(base, "CHECKOUT")
				if _, err := os.Stat(alias); os.IsNotExist(err) {
					t.Skip("filesystem is case-sensitive")
				}
				if kind == "case-checkout" {
					actualCheckout = alias
				} else {
					tempRoot = filepath.Join(alias, "tmp-private-marker")
				}
			}
			for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(key, tempRoot)
			}
			before, err := os.ReadDir(inside)
			require.NoError(t, err)
			checkoutBefore, err := os.ReadDir(checkout)
			require.NoError(t, err)
			prepared, cleanup, err := PrepareReviewToolPolicy(NewClaudeAgent("unused"), &config.ReviewToolPolicy{Mode: "read-only"}, nil, actualCheckout, false)
			t.Cleanup(cleanup)
			if allowed {
				require.NoError(t, err)
				path := prepared.(*ClaudeAgent).strictMCPConfigPath
				assert.True(t, filepath.IsAbs(path))
				resolved, err := filepath.EvalSymlinks(filepath.Dir(path))
				require.NoError(t, err)
				root, err := filepath.EvalSymlinks(external)
				require.NoError(t, err)
				assert.Equal(t, root, filepath.Dir(resolved))
				cleanup()
				_, err = os.Stat(filepath.Dir(path))
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.ErrorIs(t, err, ErrReviewToolPolicy)
				assert.Nil(t, prepared)
				assert.NotContains(t, err.Error(), tempRoot)
				assert.NotContains(t, err.Error(), checkout)
			}
			after, err := os.ReadDir(inside)
			require.NoError(t, err)
			assert.Equal(t, before, after, "preparation must not leave config artifacts inside the checkout")
			checkoutAfter, err := os.ReadDir(checkout)
			require.NoError(t, err)
			assert.Equal(t, checkoutBefore, checkoutAfter)
		})
	}
}

func TestPrivateReviewConfigDirAcrossWindowsVolumes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows volume semantics")
	}
	checkout, err := os.Getwd()
	require.NoError(t, err)
	checkout, err = filepath.EvalSymlinks(checkout)
	require.NoError(t, err)
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	require.NoError(t, err)
	if strings.EqualFold(filepath.VolumeName(checkout), filepath.VolumeName(tempRoot)) {
		t.Skip("requires checkout and temporary root on different volumes")
	}

	dir, err := newPrivateReviewConfigDir(checkout)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.True(filepath.IsAbs(dir))
	assert.True(info.IsDir())
	assert.Equal(tempRoot, filepath.Dir(dir))
	assert.NotEqual(filepath.VolumeName(checkout), filepath.VolumeName(dir))
}

func TestPrivateReviewConfigDirErrorsOmitPaths(t *testing.T) {
	for _, kind := range []string{"missing-checkout", "file-checkout", "missing-root", "file-root"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			checkout, root := filepath.Join(base, "checkout"), filepath.Join(base, "tmp-private-marker")
			if kind == "file-checkout" {
				require.NoError(t, os.WriteFile(checkout, []byte("synthetic private payload"), 0o600))
			} else if kind != "missing-checkout" {
				require.NoError(t, os.Mkdir(checkout, 0o700))
			}
			if kind == "file-root" {
				require.NoError(t, os.WriteFile(root, []byte("synthetic private payload"), 0o600))
			} else if kind != "missing-root" {
				require.NoError(t, os.Mkdir(root, 0o700))
			}
			for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(key, root)
			}
			dir, err := newPrivateReviewConfigDir(checkout)
			require.ErrorIs(t, err, ErrReviewToolPolicy)
			assert.Empty(t, dir)
			assert.NotContains(t, err.Error(), base)
			assert.NotContains(t, err.Error(), "tmp-private-marker")
			assert.NotContains(t, err.Error(), "synthetic private payload")
		})
	}
}
