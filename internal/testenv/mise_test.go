package testenv

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitIsolationPreservesMiseToolSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mise resolves the Windows profile independently of HOME")
	}
	mise, err := exec.LookPath("mise")
	if err != nil {
		t.Skip("requires mise")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	work := filepath.Join(root, "work")
	data := filepath.Join(root, "data")
	for _, dir := range []string{home, work} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	for _, version := range []string{"1.27.1", "1.27.3"} {
		dir := filepath.Join(data, "installs", "go", version, "bin")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, goName), nil, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "mise.toml"), []byte("[tools]\ngo = \"1.27.1\"\n"), 0o600))
	systemConfig := filepath.Join(root, "system.toml")
	require.NoError(t, os.WriteFile(systemConfig, []byte("[tools]\ngo = \"1.27.3\"\n"), 0o600))
	for key, value := range map[string]string{
		"HOME": home, "USERPROFILE": home, "MISE_GLOBAL_CONFIG_FILE": "~/mise.toml",
		"MISE_SYSTEM_CONFIG_FILE": systemConfig, "MISE_DATA_DIR": data,
		"MISE_CONFIG_DIR": filepath.Join(root, "config"), "MISE_CACHE_DIR": filepath.Join(root, "cache"),
		"MISE_STATE_DIR": filepath.Join(root, "state"), "MISE_TRUSTED_CONFIG_PATHS": root, "MISE_OFFLINE": "1", "MISE_AUTO_INSTALL": "0",
	} {
		t.Setenv(key, value)
	}
	originalUserEnv := userEnv
	t.Cleanup(func() { userEnv = originalUserEnv })
	selected := func(env []string) string {
		cmd := exec.CommandContext(t.Context(), mise, "which", "go")
		cmd.Dir = work
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return filepath.Clean(strings.TrimSpace(string(output)))
	}
	want := filepath.Join(data, "installs", "go", "1.27.1", "bin", goName)
	assert.Equal(t, want, selected(nil))
	configureGitForTests(t.TempDir())
	assert.Equal(t, filepath.Join(data, "installs", "go", "1.27.3", "bin", goName), selected(nil))
	assert.Equal(t, want, selected(UserEnv()))
}

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
	cmd := exec.CommandContext(t.Context(), "go", "env", "GOROOT")
	cmd.Env = append(os.Environ(), "GO_TELEMETRY_CHILD=2")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	goBin := filepath.Join(strings.TrimSpace(string(output)), "bin", "go")
	if runtime.GOOS == "windows" {
		goBin += ".exe"
	}
	for _, name := range []string{"default", "toolchain configuration", "conflicting GOROOT values", "toolchain on PATH"} {
		t.Run(name, func(t *testing.T) {
			if name == "toolchain on PATH" && runtime.GOROOT() != "" { //nolint:staticcheck
				t.Skip("requires a test binary built with -trimpath")
			}
			toolBin := goBin
			home := t.TempDir()
			for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "GOENV", "GOROOT", "GOTOOLCHAIN", "GOPATH", "GOMODCACHE", "GOCACHE", "MISE_CONFIG_DIR", "MISE_DATA_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_TERMINAL_PROMPT"} {
				t.Setenv(key, "")
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("GOENV", "off")
			t.Setenv("GOTOOLCHAIN", "local")
			if name == "toolchain on PATH" {
				root := t.TempDir()
				bin := filepath.Join(root, "bin")
				require.NoError(t, os.MkdirAll(bin, 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "tool"), 0o755))
				goBinary, err := os.ReadFile(goBin)
				require.NoError(t, err)
				toolBin = filepath.Join(bin, filepath.Base(goBin))
				require.NoError(t, os.WriteFile(toolBin, goBinary, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "go.env"), []byte("GOCACHE="+t.TempDir()+"\n"), 0o600))
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			} else if name != "default" {
				root := t.TempDir()
				goenv := filepath.Join(t.TempDir(), "goenv")
				require.NoError(t, os.WriteFile(goenv, []byte("GOROOT="+root+"\n"), 0o600))
				data := "GOPATH=" + t.TempDir() + "\nGOCACHE=" + t.TempDir() + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(root, "go.env"), []byte(data), 0o600))
				t.Setenv("GOENV", goenv)
				if name == "conflicting GOROOT values" {
					envRoot := t.TempDir()
					require.NoError(t, os.WriteFile(filepath.Join(envRoot, "go.env"), []byte(data), 0o600))
					require.NoError(t, os.WriteFile(filepath.Join(root, "go.env"), nil, 0o600))
					t.Setenv("GOROOT", envRoot)
				}
			}
			originalUserEnv := userEnv
			t.Cleanup(func() { userEnv = originalUserEnv })
			readCaches := func() string {
				cmd := exec.CommandContext(t.Context(), toolBin, "env", "GOMODCACHE", "GOCACHE")
				// Prevent Go's telemetry child from outliving this temporary home.
				cmd.Env = append(os.Environ(), "GO_TELEMETRY_CHILD=2")
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
