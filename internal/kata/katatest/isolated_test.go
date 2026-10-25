package katatest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envMap indexes KEY=VALUE entries by upper-cased key.
func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		out[strings.ToUpper(key)] = value
	}
	return out
}

func TestIsolatedKataEnvDropsInheritedRoutingAndIdentity(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/real/home",
		"KATA_HOME=/real/kata",
		"KATA_SERVER=http://127.0.0.1:1",
		"KATA_AUTH_TOKEN=inherited-token",
		"KATA_AUTH_TOKEN_FILE=/real/token",
		"KATA_AUTHOR=someone",
		"KATA_TEAMMATE=helper",
		"KATA_DAEMON=unix:///real.sock",
		"KATA_INBOX_USER=someone",
		"SSH_AUTH_SOCK=/real/agent",
		"OP_SERVICE_ACCOUNT_TOKEN=secret",
	}
	home, kataHome := t.TempDir(), t.TempDir()

	got := envMap(IsolatedEnv(parent, home, kataHome))

	assert.Equal(t, "/usr/bin:/bin", got["PATH"], "PATH is allowlisted")
	assert.Equal(t, home, got["HOME"])
	assert.Equal(t, kataHome, got["KATA_HOME"])
	for _, name := range []string{
		"KATA_SERVER", "KATA_AUTH_TOKEN", "KATA_AUTH_TOKEN_FILE", "KATA_AUTHOR",
		"KATA_TEAMMATE", "KATA_DAEMON", "KATA_INBOX_USER", "SSH_AUTH_SOCK",
		"OP_SERVICE_ACCOUNT_TOKEN",
	} {
		assert.NotContains(t, got, name)
	}
}

func TestIsolatedKataEnvPointsConfigAndCachesInsideHome(t *testing.T) {
	home := t.TempDir()
	got := envMap(IsolatedEnv([]string{"XDG_CONFIG_HOME=/real/config"}, home, t.TempDir()))
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		require.Contains(t, got, name)
		assert.True(t, strings.HasPrefix(got[name], home+string(filepath.Separator)), "%s=%s", name, got[name])
	}
}

// Regression: the first fuzz seeds kept a bare "KATA_SERVER" entry (no "=") and
// a lower-case spelling; both must be dropped like the canonical form.
func TestIsolatedKataEnvDropsMalformedAndCaseVariantEntries(t *testing.T) {
	got := IsolatedEnv([]string{"KATA_SERVER", "kata_server=http://x", "Kata_Auth_Token=t", "=x", ""}, "/h", "/k")
	for _, entry := range got {
		key, _, _ := strings.Cut(entry, "=")
		assert.False(t, strings.HasPrefix(strings.ToUpper(key), "KATA_") && !strings.EqualFold(key, "KATA_HOME"), entry)
	}
}

func FuzzIsolatedKataEnv(f *testing.F) {
	f.Add("KATA_SERVER=http://127.0.0.1:1")
	f.Add("kata_author=x")
	f.Add("PATH=/bin")
	f.Add("KATA_HOME=/real")
	f.Add("HOME=/real")
	f.Add("NOEQUALS")
	f.Add("=")
	f.Fuzz(func(t *testing.T, entry string) {
		home, kataHome := "/disposable/home", "/disposable/kata"
		got := IsolatedEnv([]string{entry}, home, kataHome)
		seen := map[string]bool{}
		for _, item := range got {
			key, value, ok := strings.Cut(item, "=")
			require.True(t, ok, "entry without '=': %q", item)
			upper := strings.ToUpper(key)
			require.False(t, seen[upper], "duplicate %s", key)
			seen[upper] = true
			if strings.HasPrefix(upper, "KATA_") {
				require.Equal(t, "KATA_HOME", upper, "inherited %s from %q", key, entry)
				require.Equal(t, kataHome, value)
			}
			if upper == "HOME" {
				require.Equal(t, home, value)
			}
		}
	})
}
