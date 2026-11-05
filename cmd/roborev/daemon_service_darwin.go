package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"

	"go.kenn.io/roborev/internal/procutil"
)

var runLaunchctl = runUserLaunchctl

func init() {
	serviceDescription = "launchd job"
	serviceRestartCommand = "launchctl kickstart"
	serviceStartCommand = "launchctl kickstart"
	// Preserve the running CLI's identity if its installed path is replaced.
	path, err := os.Executable()
	var info os.FileInfo
	if err == nil {
		info, err = os.Stat(path)
	}
	cliExecutableInfo = func() (os.FileInfo, error) { return info, err }
}

func processAncestry(ctx context.Context, pid int) ([]int, error) {
	var chain []int
	for pid > 1 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chain = append(chain, pid)
		proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			// macOS can report EIO when a PID has disappeared.
			if errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
				err = os.ErrNotExist
			}
			return nil, fmt.Errorf("read daemon parent: %w", err)
		}
		pid = int(proc.Eproc.Ppid)
	}
	return chain, nil
}

func lookupDaemonService(ctx context.Context, pid int) (*daemonService, error) {
	return lookupLaunchdService(ctx, pid, launchdDeps{ancestors: processAncestry, run: runLaunchctl, uid: os.Getuid()})
}

func processServiceUnit(int) (string, error) { return "", errors.ErrUnsupported }

func runUserLaunchctl(ctx context.Context, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "launchctl", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("launchctl %s: %w", strings.Join(args, " "), procutil.WithStderr(err))
	}
	return output, nil
}

func userServiceMainPID(ctx context.Context, unit string) (int, error) {
	output, err := runLaunchctl(ctx, "print", unit)
	if err != nil {
		return 0, err
	}
	service, _, err := parseLaunchctlService(output)
	if err != nil {
		return 0, err
	}
	return service.mainPID, nil
}

func startUserService(ctx context.Context, unit string) error {
	_, err := runLaunchctl(ctx, "kickstart", unit)
	return err
}

func userManagerAvailable(context.Context) bool { return true }
