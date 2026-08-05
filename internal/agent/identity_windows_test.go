//go:build windows

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWindowsProbeFixture_VersionFlagsAndPositionals exercises the generated
// .bat fixture directly so cmd.exe if-block syntax is validated on Windows,
// not only through cross-platform string construction on other OSes.
func TestWindowsProbeFixture_VersionFlagsAndPositionals(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "positional.log")
	versionLine := "grok 0.2.118 (abc) [stable]"
	bin := writeProbeScriptWithMarker(t, dir, "grok-fixture", versionLine, marker)

	t.Run("--version", func(t *testing.T) {
		out, err := exec.Command(bin, "--version").CombinedOutput()
		require.NoError(t, err, "stdout/stderr: %s", out)
		assert.Contains(t, string(out), versionLine)
	})

	t.Run("-v", func(t *testing.T) {
		out, err := exec.Command(bin, "-v").CombinedOutput()
		require.NoError(t, err, "stdout/stderr: %s", out)
		assert.Contains(t, string(out), versionLine)
	})

	t.Run("positional version", func(t *testing.T) {
		require.NoError(t, os.WriteFile(marker, nil, 0o644))
		out, err := exec.Command(bin, "version").CombinedOutput()
		require.Error(t, err, "positional version must fail; out=%s", out)
		data, readErr := os.ReadFile(marker)
		require.NoError(t, readErr)
		assert.Contains(t, string(data), "positional:version")
	})

	t.Run("positional random", func(t *testing.T) {
		require.NoError(t, os.WriteFile(marker, nil, 0o644))
		out, err := exec.Command(bin, "random-prompt").CombinedOutput()
		require.Error(t, err, "positional prompt must fail; out=%s", out)
		data, readErr := os.ReadFile(marker)
		require.NoError(t, readErr)
		assert.Contains(t, string(data), "positional:random-prompt")
		assert.False(t, strings.Contains(string(data), "positional:--version"),
			"version flags must not mark as positional")
	})
}
