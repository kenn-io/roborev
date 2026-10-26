//go:build !windows

package tokens

import "os/exec"

func agentsviewOutput(cmd *exec.Cmd) ([]byte, error) {
	return cmd.Output()
}
