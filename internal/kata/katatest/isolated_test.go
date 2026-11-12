package katatest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsolatedKataEnv(t *testing.T) {
	home, kataHome := t.TempDir(), t.TempDir()
	parent := []string{
		"PATH=/usr/bin:/bin", "path=/ignored", "HOME=/real/home", "KATA_HOME=/real/kata",
		"KATA_SERVER=http://127.0.0.1:1", "KATA_AUTH_TOKEN=inherited-token", "KATA_AUTH_TOKEN_FILE=/real/token", "KATA_AUTHOR=someone", "KATA_TEAMMATE=helper", "KATA_DAEMON=unix:///real.sock", "KATA_INBOX_USER=someone", "SSH_AUTH_SOCK=/real/agent", "OP_SERVICE_ACCOUNT_TOKEN=secret", "XDG_CONFIG_HOME=/real/config", "KATA_SERVER", "kata_server=http://x", "Kata_Auth_Token=t", "=x", "",
	}
	want := []string{
		"PATH=/usr/bin:/bin", "HOME=" + home, "USERPROFILE=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"), "XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"), "XDG_CACHE_HOME=" + filepath.Join(home, ".cache"), "KATA_HOME=" + kataHome,
	}
	assert.Equal(t, want, IsolatedEnv(parent, home, kataHome))
}

// The complete output must preserve the first spelling and value of each
// allowlisted inherited key, including TMPDIR, and replace routing/identity.
func FuzzIsolatedKataEnv(f *testing.F) {
	for _, seed := range [][2]string{{"PATH=/bin", "path=/other"}, {"TMPDIR=/scratch", "KATA_SERVER=http://host"}, {"HOME=/real", "XDG_CONFIG_HOME=/real"}, {"Kata_Auth_Token=secret", "LANG=C"}, {"NOEQUALS", "="}, {"TEMP=a=b", "temp=ignored"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, first, second string) {
		home, kataHome := "/disposable/home", "/disposable/kata"
		want := []string{}
		seen := map[string]bool{}
		for _, entry := range []string{first, second} {
			key, _, ok := strings.Cut(entry, "=")
			upper := strings.ToUpper(key)
			switch upper {
			case "PATH", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
				"SYSTEMROOT", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "WINDIR":
				if ok && !seen[upper] {
					want = append(want, entry)
					seen[upper] = true
				}
			}
		}
		want = append(want, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "XDG_DATA_HOME="+filepath.Join(home, ".local", "share"), "XDG_STATE_HOME="+filepath.Join(home, ".local", "state"), "XDG_CACHE_HOME="+filepath.Join(home, ".cache"), "KATA_HOME="+kataHome)
		require.Equal(t, want, IsolatedEnv([]string{first, second}, home, kataHome))
	})
}
