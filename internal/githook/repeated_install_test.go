package githook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepeatedPrePushInstallPreservesShell(t *testing.T) {
	t.Parallel()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("requires a POSIX shell")
	}
	for _, legacy := range []bool{false, true} {
		name := "fresh"
		if legacy {
			name = "legacy without end marker"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			hooksDir := t.TempDir()
			hookPath := filepath.Join(hooksDir, hookPrePush)
			binary := writeExecutableFile(t, filepath.Join(t.TempDir(), "roborev"))
			custom := "printf '%s\\n' \"custom hook\"\nif [ -n \"$1\" ]; then\n    printf '%s\\n' \"$1\"\nfi\n"
			initial := shebang + custom
			if legacy {
				snippet := strings.ReplaceAll(generateEmbeddablePrePushWithBinary(binary), roborevHookEndMarker+"\n", "")
				snippet = strings.ReplaceAll(snippet, PrePushVersionMarker, "pre-push hook v1")
				initial = shebang + snippet + custom
			}
			require.NoError(t, os.WriteFile(hookPath, []byte(initial), 0o755))
			var previous []byte
			for i := range 3 {
				require.NoError(t, InstallWithOptions(hooksDir, hookPrePush, InstallOptions{BinaryPath: binary}))
				content, err := os.ReadFile(hookPath)
				require.NoError(t, err)
				assert.Equal(1, strings.Count(string(content), "# roborev pre-push hook"))
				assert.Contains(string(content), custom)
				if i > 0 {
					assert.Equal(previous, content)
				}
				previous = content
				// Parse without running the hook or its baked binary.
				cmd := exec.Command(shell, "-n")
				cmd.Stdin = strings.NewReader(string(content))
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", output)
			}
			require.NoError(t, Uninstall(hookPath))
			content, err := os.ReadFile(hookPath)
			require.NoError(t, err)
			assert.Equal(shebang+custom, string(content))
		})
	}
}
