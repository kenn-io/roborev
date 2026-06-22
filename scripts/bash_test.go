package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func bashPath(t *testing.T, path string) string {
	t.Helper()

	if runtime.GOOS != "windows" {
		return path
	}

	abs, err := filepath.Abs(path)
	require.NoError(t, err)

	volume := filepath.VolumeName(abs)
	if len(volume) != 2 || volume[1] != ':' {
		return filepath.ToSlash(abs)
	}

	drive := strings.ToLower(volume[:1])
	rest := strings.TrimPrefix(filepath.ToSlash(strings.TrimPrefix(abs, volume)), "/")
	candidates := []string{
		"/mnt/" + drive + "/" + rest,
		"/" + drive + "/" + rest,
		filepath.ToSlash(abs),
	}
	for _, candidate := range candidates {
		cmd := exec.Command("bash", "-lc", "test -e "+shellQuote(candidate))
		if err := cmd.Run(); err == nil {
			return candidate
		}
	}

	t.Fatalf("could not translate Windows path %q for bash", path)
	return ""
}

func readShellScript(t *testing.T, path string) []byte {
	t.Helper()

	script, err := os.ReadFile(path)
	require.NoError(t, err)
	script = bytes.ReplaceAll(script, []byte("\r\n"), []byte("\n"))
	return script
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
