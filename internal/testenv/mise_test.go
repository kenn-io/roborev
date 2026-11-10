package testenv

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitIsolationPreservesMiseDirectories(t *testing.T) {
	mise, err := exec.LookPath("mise")
	if err != nil {
		t.Skip("requires mise")
	}
	for _, useXDG := range []bool{false, true} {
		name := "default"
		if useXDG {
			name = "xdg"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(home, "temp"), 0o755))
			for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "MISE_CONFIG_DIR", "MISE_DATA_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_TERMINAL_PROMPT", "GOENV", "GOPATH", "GOCACHE", "LOCALAPPDATA", "TEMP"} {
				t.Setenv(key, "")
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("LOCALAPPDATA", filepath.Join(home, "local"))
			t.Setenv("TEMP", filepath.Join(home, "temp"))
			t.Setenv("GOENV", "off")
			t.Setenv("MISE_OFFLINE", "1")
			t.Setenv("MISE_AUTO_INSTALL", "0")
			if useXDG {
				for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
					t.Setenv(key, t.TempDir())
				}
			}
			originalUserEnv := userEnv
			t.Cleanup(func() { userEnv = originalUserEnv })
			readDirs := func() map[string]string {
				cmd := exec.CommandContext(t.Context(), mise, "doctor", "--json")
				cmd.Dir = home
				output, err := cmd.Output()
				if err != nil {
					var exit *exec.ExitError
					require.ErrorAs(t, err, &exit)
				}
				var report struct {
					Dirs map[string]string `json:"dirs"`
				}
				require.NoError(t, json.Unmarshal(output, &report))
				return report.Dirs
			}
			before := readDirs()
			configureGitForTests(t.TempDir())
			after := readDirs()
			for _, key := range []string{"config", "data", "cache", "state"} {
				require.NotEmpty(t, before[key], key)
				assert.Equal(t, before[key], after[key], key)
			}
		})
	}
}

func TestGitIsolationPreservesGoCaches(t *testing.T) {
	goBin, err := exec.LookPath("go")
	require.NoError(t, err)
	for _, configured := range []bool{false, true} {
		name := "default"
		if configured {
			name = "toolchain configuration"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "GOENV", "GOROOT", "GOTOOLCHAIN", "GOPATH", "GOMODCACHE", "GOCACHE", "MISE_CONFIG_DIR", "MISE_DATA_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_TERMINAL_PROMPT"} {
				t.Setenv(key, "")
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("GOENV", "off")
			t.Setenv("GOTOOLCHAIN", "local")
			if configured {
				root := t.TempDir()
				goenv := filepath.Join(t.TempDir(), "goenv")
				require.NoError(t, os.WriteFile(goenv, []byte("GOROOT="+root+"\n"), 0o600))
				data := "GOPATH=" + t.TempDir() + "\nGOCACHE=" + t.TempDir() + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(root, "go.env"), []byte(data), 0o600))
				t.Setenv("GOENV", goenv)
			}
			originalUserEnv := userEnv
			t.Cleanup(func() { userEnv = originalUserEnv })
			readCaches := func() string {
				cmd := exec.CommandContext(t.Context(), goBin, "env", "GOMODCACHE", "GOCACHE")
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", output)
				return string(output)
			}
			before := readCaches()
			configureGitForTests(t.TempDir())
			assert.Equal(t, before, readCaches())
		})
	}
}
