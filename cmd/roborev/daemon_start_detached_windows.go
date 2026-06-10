//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	kitdaemon "go.kenn.io/kit/daemon"
)

const detachedProcess = 0x00000008

func startDetachedDaemon(ctx context.Context, opts detachedDaemonOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exe := opts.Executable
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return err
		}
	}
	if opts.RefuseEphemeral && kitdaemon.IsEphemeralExecutable(exe) {
		return fmt.Errorf("refusing to auto-start daemon from ephemeral binary %s", filepath.Base(exe))
	}
	cmd := exec.Command(exe, opts.Args...)
	cmd.Env = opts.Env
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start hidden daemon: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
