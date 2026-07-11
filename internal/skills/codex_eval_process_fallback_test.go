//go:build codexeval && (plan9 || js || wasip1)

package skills

import (
	"os/exec"
	"time"
)

func prepareEvalCommand(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
}
