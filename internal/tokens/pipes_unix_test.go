//go:build !windows

package tokens

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func installPipeHoldingAgentsview(t *testing.T, returnsUsage bool) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Setenv("FAKE_AGENTSVIEW_HOLDER_PID", pidFile)
	body := "exec sleep 30\n"
	if returnsUsage {
		body = `echo '{"session_id":"s","total_output_tokens":42,"peak_context_tokens":900,"cost_usd":0.07,"has_cost":true}'
exit 0
`
	}
	installFakeAgentsview(t, "#!/bin/sh\nsleep 30 &\necho $! > \"$FAKE_AGENTSVIEW_HOLDER_PID\"\n"+body)
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		var pid int
		if _, err := fmt.Sscan(string(data), &pid); err != nil {
			return
		}
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
	})
}
