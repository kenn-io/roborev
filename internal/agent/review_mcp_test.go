package agent

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestReviewMCPSelection(t *testing.T) {
	checkout := t.TempDir()
	operator := filepath.Join(t.TempDir(), "operator.json")
	const selected = `{"command":"synthetic-server","args":["--stdio"],"env":{"TOKEN":"secret-env-sentinel"},"extra":{"preserve":1}}`
	require.NoError(t, os.WriteFile(operator, []byte(`{"mcpServers":{"research":`+selected+`,"unused":{"command":"never-start"}},"other":"ignored"}`), 0o600))
	policy := config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"research": {"read", "read"}}}
	cfg := &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: []string{"research/read", "research/read_more"}}
	path, cleanup, err := prepareReviewMCPConfig(policy, cfg, checkout)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{"research":`+selected+`}}`, string(data))
	assert.Equal(t, []string{"mcp__research__read"}, nativeReviewToolNames(policy))
	assert.Equal(t, []string{"read", "read"}, policy.MCP["research"], "selection must not mutate frozen policy")
	if runtime.GOOS != "windows" {
		file, err := os.Stat(path)
		require.NoError(t, err)
		dir, err := os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), file.Mode().Perm())
		assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())
	}
	assert.True(t, filepath.IsAbs(path))
	resolvedCheckout, err := filepath.EvalSymlinks(checkout)
	require.NoError(t, err)
	assert.NotEqual(t, resolvedCheckout, filepath.Dir(path))
	assert.NotContains(t, path, resolvedCheckout+string(filepath.Separator))
	cleanup()
	assert.NoFileExists(t, path)
	assert.NoDirExists(t, filepath.Dir(path))
	cleanup() // Deferred cleanup may run after explicit failure cleanup.
	assert.FileExists(t, operator)
}

func TestReviewMCPEmptyGrants(t *testing.T) {
	for _, grants := range []map[string][]string{nil, {}, {"research": {}}} {
		path, cleanup, err := prepareReviewMCPConfig(config.ReviewToolPolicy{Mode: "read-only", MCP: grants}, &config.Config{ReviewMCPConfig: filepath.Join(t.TempDir(), "missing")}, t.TempDir())
		require.NoError(t, err)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.JSONEq(t, `{"mcpServers":{}}`, string(data))
		cleanup()
		assert.NoFileExists(t, path)
	}
}

func TestReviewMCPExactCeiling(t *testing.T) {
	operator := filepath.Join(t.TempDir(), "operator.json")
	require.NoError(t, os.WriteFile(operator, []byte(`{"mcpServers":{"research":{"command":"synthetic-server"}}}`), 0o600))
	for _, tc := range []struct {
		requested string
		allowed   []string
	}{
		{"read", nil},
		{"read", []string{"research/read_more"}},
		{"read_more", []string{"research/read"}},
		{"read", []string{"research/*"}},
		{"read", []string{"other/read"}},
		{"write", []string{"research/read", "research/read_more"}},
	} {
		path, cleanup, err := prepareReviewMCPConfig(config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"research": {tc.requested}}}, &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: tc.allowed}, t.TempDir())
		require.ErrorIs(t, err, ErrReviewToolPolicy)
		assert.Empty(t, path)
		require.NotNil(t, cleanup)
		cleanup()
	}
}

func TestReviewMCPRevocation(t *testing.T) {
	operator := filepath.Join(t.TempDir(), "operator.json")
	require.NoError(t, os.WriteFile(operator, []byte(`{"mcpServers":{"research":{"command":"synthetic-server"}}}`), 0o600))
	policy := config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"research": {"read"}}}
	globalPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(globalPath, fmt.Appendf(nil, "review_mcp_config = %q\nreview_mcp_allowed_tools = ['research/read']\n", operator), 0o600))
	cfg, err := config.LoadGlobalFrom(globalPath)
	require.NoError(t, err)
	path, cleanup, err := prepareReviewMCPConfig(policy, cfg, t.TempDir())
	require.NoError(t, err)
	cleanup()
	assert.NoFileExists(t, path)
	// The next attempt receives the current global config, not enqueue authority.
	require.NoError(t, os.WriteFile(globalPath, fmt.Appendf(nil, "review_mcp_config = %q\nreview_mcp_allowed_tools = []\n", operator), 0o600))
	cfg, err = config.LoadGlobalFrom(globalPath)
	require.NoError(t, err)
	path, cleanup, err = prepareReviewMCPConfig(policy, cfg, t.TempDir())
	require.ErrorIs(t, err, ErrReviewToolPolicy)
	assert.Empty(t, path)
	cleanup()
}

func TestReviewMCPNativeNames(t *testing.T) {
	policy := config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"z": {"read_more", "read", "read"}, "a": {"fetch"}, "empty": {}}}
	assert.Equal(t, []string{"mcp__a__fetch", "mcp__z__read", "mcp__z__read_more"}, nativeReviewToolNames(policy))
}

func TestReviewMCPAttemptCleanup(t *testing.T) {
	policy := config.ReviewToolPolicy{Mode: "read-only"}
	first, cleanupFirst, err := prepareReviewMCPConfig(policy, nil, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(cleanupFirst)
	second, cleanupSecond, err := prepareReviewMCPConfig(policy, nil, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(cleanupSecond)
	assert.NotEqual(t, filepath.Dir(first), filepath.Dir(second))
	cleanupFirst()
	assert.NoDirExists(t, filepath.Dir(first))
	assert.FileExists(t, second, "cleanup must not remove another attempt's config")
	cleanupSecond()
	assert.NoDirExists(t, filepath.Dir(second))
}

func TestReviewMCPInvalidConfig(t *testing.T) {
	for name, contents := range map[string]string{
		"syntax":         `{"secret-payload-sentinel":`,
		"duplicate":      `{"mcpServers":{},"mcpServers":{"research":{"command":"secret-payload-sentinel"}}}`,
		"null":           `null`,
		"missing":        `{}`,
		"unknown server": `{"mcpServers":{"other":{"command":"secret-payload-sentinel"}}}`,
		"null server":    `{"mcpServers":{"research":null}}`,
		"scalar server":  `{"mcpServers":{"research":"secret-payload-sentinel"}}`,
		"empty server":   `{"mcpServers":{"research":{}}}`,
		"bad command":    `{"mcpServers":{"research":{"command":["secret-payload-sentinel"]}}}`,
		"bad args":       `{"mcpServers":{"research":{"command":"server","args":[1]}}}`,
		"bad env":        `{"mcpServers":{"research":{"command":"server","env":{"TOKEN":1}}}}`,
		"bad headers":    `{"mcpServers":{"research":{"type":"http","url":"https://example.invalid/mcp","headers":["secret-payload-sentinel"]}}}`,
		"no transport":   `{"mcpServers":{"research":{"url":"https://example.invalid/mcp"}}}`,
		"no remote URL":  `{"mcpServers":{"research":{"type":"http","command":"synthetic-server"}}}`,
		"bad transport":  `{"mcpServers":{"research":{"type":"secret-payload-sentinel","command":"synthetic-server"}}}`,
		"SDK transport":  `{"mcpServers":{"research":{"type":"sdk","command":"synthetic-server"}}}`,
		"null transport": `{"mcpServers":{"research":{"type":null,"command":"synthetic-server"}}}`,
		"null argument":  `{"mcpServers":{"research":{"command":"synthetic-server","args":[null]}}}`,
		"null env value": `{"mcpServers":{"research":{"command":"synthetic-server","env":{"TOKEN":null}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			operator := filepath.Join(t.TempDir(), "operator.json")
			require.NoError(t, os.WriteFile(operator, []byte(contents), 0o600))
			path, cleanup, err := prepareReviewMCPConfig(config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"research": {"read"}}}, &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: []string{"research/read"}}, t.TempDir())
			require.ErrorIs(t, err, ErrReviewToolPolicy)
			assert.NotContains(t, err.Error(), "secret-payload-sentinel")
			assert.Contains(t, err.Error(), fmt.Sprintf("%q", operator))
			assert.Empty(t, path)
			require.NotNil(t, cleanup)
			cleanup()
		})
	}
}

func TestReviewMCPUnavailableConfig(t *testing.T) {
	for _, operator := range []string{"", filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		path, cleanup, err := prepareReviewMCPConfig(config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"research": {"read"}}}, &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: []string{"research/read"}}, t.TempDir())
		require.ErrorIs(t, err, ErrReviewToolPolicy)
		assert.Empty(t, path)
		cleanup()
	}
}

func TestReviewMCPRejectsCheckoutRelativeOperatorConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile("operator.json", []byte(`{"mcpServers":{"research":{"command":"synthetic-server"}}}`), 0o600))
	path, cleanup, err := prepareReviewMCPConfig(config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"research": {"read"}}}, &config.Config{ReviewMCPConfig: "operator.json", ReviewMCPAllowedTools: []string{"research/read"}}, t.TempDir())
	t.Cleanup(cleanup)
	require.ErrorIs(t, err, ErrReviewToolPolicy)
	assert.Empty(t, path)
	assert.Contains(t, err.Error(), "absolute")
}

func TestReviewMCPFailureLeavesNoTemporaryConfig(t *testing.T) {
	tempRoot := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, tempRoot)
	}
	operator := filepath.Join(t.TempDir(), "operator.json")
	require.NoError(t, os.WriteFile(operator, []byte(`{"mcpServers":{"first":{"command":"synthetic-server"},"second":null}}`), 0o600))
	path, cleanup, err := prepareReviewMCPConfig(config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"first": {"read"}, "second": {"find"}}}, &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: []string{"first/read", "second/find"}}, t.TempDir())
	require.ErrorIs(t, err, ErrReviewToolPolicy)
	assert.Empty(t, path)
	cleanup()
	entries, err := os.ReadDir(tempRoot)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestReviewMCPTempLocation(t *testing.T) {
	for _, location := range []string{"inside checkout", "relative", "symlink into checkout"} {
		t.Run(location, func(t *testing.T) {
			root := t.TempDir()
			checkout := filepath.Join(root, "agent-checkout")
			processDir := filepath.Join(root, "daemon-cwd")
			require.NoError(t, os.Mkdir(checkout, 0o700))
			require.NoError(t, os.Mkdir(processDir, 0o700))
			t.Chdir(processDir) // The agent checkout is not the daemon cwd.
			tempRoot := filepath.Join(checkout, "tmp")
			require.NoError(t, os.Mkdir(tempRoot, 0o700))
			switch location {
			case "relative":
				// Windows resolves relative temp paths before os.TempDir returns.
				// Keep the effective root inside the checkout on either platform.
				tempRoot = filepath.Join("..", "agent-checkout", "tmp")
			case "symlink into checkout":
				link := filepath.Join(root, "temp-link")
				err := os.Symlink(tempRoot, link)
				if err != nil && runtime.GOOS == "windows" {
					t.Skip("symlink creation requires Windows developer mode or privilege")
				}
				require.NoError(t, err)
				tempRoot = link
			}
			for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(key, tempRoot)
			}
			operator := filepath.Join(root, "operator.json")
			require.NoError(t, os.WriteFile(operator, []byte(`{"mcpServers":{"research":{"command":"synthetic-server","env":{"TOKEN":"secret-temp-sentinel"}}}}`), 0o600))
			path, cleanup, err := prepareReviewMCPConfig(config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"research": {"read"}}}, &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: []string{"research/read"}}, checkout)
			t.Cleanup(cleanup)
			require.ErrorIs(t, err, ErrReviewToolPolicy)
			assert.NotContains(t, fmt.Sprint(err), "secret-temp-sentinel")
			assert.Empty(t, path)
			entries, err := os.ReadDir(tempRoot)
			require.NoError(t, err)
			assert.Empty(t, entries, "rejected temp roots must never receive a config")
		})
	}
}

func TestReviewMCPSelectedDefinitions(t *testing.T) {
	for _, definition := range []string{
		`{"type":"http","url":"https://example.invalid/mcp","headers":{"Authorization":"secret-header-sentinel"}}`,
		`{"type":"sse","url":"https://example.invalid/events","headers":{"X-Key":"secret-header-sentinel"}}`,
		`{"type":"ws","url":"wss://example.invalid/mcp","headers":{"X-Key":"secret-header-sentinel"}}`,
		`{"type":"streamable-http","url":"https://example.invalid/mcp"}`,
		`{"type":"stdio","command":"synthetic-server","args":["arg with spaces"],"env":{"KEY":"secret-env-sentinel"}}`,
	} {
		operator := filepath.Join(t.TempDir(), "operator.json")
		require.NoError(t, os.WriteFile(operator, []byte(`{"mcpServers":{"research":`+definition+`,"unused":null}}`), 0o600))
		path, cleanup, err := prepareReviewMCPConfig(config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"research": {"read"}, "unused": {}}}, &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: []string{"research/read"}}, t.TempDir())
		require.NoError(t, err)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.JSONEq(t, `{"mcpServers":{"research":`+definition+`}}`, string(data))
		cleanup()
	}
}

// Retaining arbitrary string payloads is part of the native-definition contract:
// decoding to a reduced server struct must not drop credentials or extensions.
func FuzzReviewMCPDefinitionPreservation(f *testing.F) {
	f.Add("synthetic-server", "secret-env-sentinel")
	f.Add("command with spaces", "quotes\"\\\n\u0000")
	f.Fuzz(func(t *testing.T, command, value string) {
		if command == "" || !utf8.ValidString(command) || !utf8.ValidString(value) {
			t.Skip("native JSON command must be a nonempty UTF-8 string")
		}
		definition := map[string]any{"command": command, "env": map[string]string{"TOKEN": value}, "extension": []string{value}}
		input, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"research": definition, "unused": nil}})
		require.NoError(t, err)
		operator := filepath.Join(t.TempDir(), "operator.json")
		require.NoError(t, os.WriteFile(operator, input, 0o600))
		path, cleanup, err := prepareReviewMCPConfig(config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"research": {"read"}}}, &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: []string{"research/read"}}, t.TempDir())
		require.NoError(t, err)
		defer cleanup()
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		expected, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"research": definition}})
		require.NoError(t, err)
		assert.JSONEq(t, string(expected), string(data))
	})
}
