//go:build integration

package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxSelfCheck(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	if _, err := exec.LookPath("nono"); err != nil {
		t.Skip("nono is required")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "nested-read-path"}[nested], func(t *testing.T) {
			work := t.TempDir()
			job := Job{
				WorkDir: work, ProxyPort: listener.Addr().(*net.TCPAddr).Port,
				Env: []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}, MemoryMax: 1 << 30,
			}
			if nested {
				read := filepath.Join(work, "read-only")
				require.NoError(t, os.Mkdir(read, 0o700))
				job.ReadPaths = []string{read}
			}
			marker := filepath.Join(work, "executed")
			cmd, env, cleanup, err := job.Command(context.Background(), []string{"bash", "-c", "touch '" + marker + "'"})
			if nested {
				require.ErrorContains(t, err, "read-path-write:")
				assert.NoFileExists(t, marker)
				return
			}
			require.NoError(t, err)
			defer cleanup()
			process := exec.CommandContext(t.Context(), cmd[0], cmd[1:]...)
			process.Env, process.Dir = env, work
			out, err := process.CombinedOutput()
			require.NoError(t, err, "%s", out)
			assert.FileExists(t, marker)
		})
	}
}
