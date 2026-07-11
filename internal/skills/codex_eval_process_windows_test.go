//go:build codexeval && windows

package skills

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func prepareEvalCommand(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
			if err == nil {
				return nil
			}
			err = cmd.Process.Kill()
			if errors.Is(err, os.ErrProcessDone) {
				return os.ErrProcessDone
			}
			return err
		}
	}
}
