package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"go.kenn.io/roborev/internal/procutil"
)

type launchdProcess struct {
	parent  int
	started unix.Timeval
}

var (
	runLaunchctl        = runLaunchctlCommand
	launchdEUID         = os.Geteuid
	readLaunchdProcArgs = func(pid int) ([]byte, error) {
		data, err := unix.SysctlRaw("kern.procargs2", pid)
		return data, launchdProcessError(pid, err)
	}
	readLaunchdProcess = func(pid int) (launchdProcess, error) {
		proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			return launchdProcess{}, launchdProcessError(pid, err)
		}
		return launchdProcess{parent: int(proc.Eproc.Ppid), started: proc.Proc.P_starttime}, nil
	}
)

// Darwin can return EINVAL or EIO for a vanished process, but those errors
// also mean other things. Only a separate existence check proves it exited.
func launchdProcessError(pid int, err error) error {
	if err != nil && errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
		return fmt.Errorf("process %d exited: %w", pid, os.ErrNotExist)
	}
	return err
}

func runLaunchctlCommand(ctx context.Context, args ...string) ([]byte, int, error) {
	output, err := exec.CommandContext(ctx, "launchctl", args...).Output()
	code := 0
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			code = exit.ExitCode()
		}
		err = fmt.Errorf("launchctl %s: %w", strings.Join(args, " "), procutil.WithStderr(err))
	}
	return output, code, err
}

func launchdServiceLabel(pid int) (string, error) {
	data, err := readLaunchdProcArgs(pid)
	if err == nil {
		_, label, parseErr := parseProcArgs2(data)
		if parseErr == nil {
			return label, nil
		}
		err = parseErr
	}
	return "", &managedRecoveryError{"unknown", fmt.Errorf("cannot determine owner of daemon PID %d: %w", pid, err)}
}

// processServiceUnit is the cheap pre-probe hint. An absent inherited label
// cannot prove a process is outside launchd: a wrapper may have removed it.
func processServiceUnit(pid int) (string, error) {
	label, err := launchdServiceLabel(pid)
	if err == nil && label == "" {
		err = errors.New("missing launchd label leaves ownership unknown")
	}
	return label, err
}

func lookupDaemonService(ctx context.Context, pid int) (*daemonService, error) {
	initial, err := readLaunchdProcess(pid)
	if err != nil {
		return nil, &managedRecoveryError{"unknown", err}
	}
	label, err := launchdServiceLabel(pid)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*daemonService, error) { return nil, &managedRecoveryError{label, err} }
	// Snapshot ancestry and process start times, then revalidate before using
	// the result. An inherited label alone never establishes ownership.
	parents := map[int]launchdProcess{}
	for parent := pid; parent > 1; {
		if _, seen := parents[parent]; seen {
			return fail(errors.New("daemon parent cycle"))
		}
		proc := initial
		if parent != pid {
			proc, err = readLaunchdProcess(parent)
			if err != nil {
				// A vanished ancestor does not prove that the daemon exited.
				return fail(fmt.Errorf("read daemon ancestor: %v", err)) //nolint:errorlint // Preserve uncertainty: an exited ancestor does not mean the daemon exited.
			}
		}
		parents[parent] = proc
		parent = proc.parent
	}
	verify := func() error {
		for ancestor, before := range parents {
			after, err := readLaunchdProcess(ancestor)
			if err != nil {
				if ancestor != pid {
					return fmt.Errorf("recheck daemon ancestor: %v", err) //nolint:errorlint // Only the daemon PID may unwrap to os.ErrNotExist.
				}
				return err
			}
			if before != after {
				return errors.New("daemon ancestry changed during ownership lookup")
			}
		}
		return nil
	}
	uid := strconv.Itoa(os.Getuid())
	var lookupErr error
	for _, domain := range []string{"gui/" + uid, "user/" + uid, "system"} {
		labels := []string{label}
		if label == "" {
			// Shell wrappers can remove XPC_SERVICE_NAME. Match loaded job
			// main PIDs to the same ancestry before declaring this unmanaged.
			output, code, err := runLaunchctl(ctx, "print", domain)
			if err != nil {
				if code != 113 {
					lookupErr = err
				}
				continue
			}
			services, err := parseLaunchctlDomain(output)
			if err != nil {
				lookupErr = err
				continue
			}
			labels = nil
			for candidate, mainPID := range services {
				if _, owned := parents[mainPID]; owned {
					labels = append(labels, candidate)
				}
			}
			slices.Sort(labels)
		}
		for _, candidate := range labels {
			unit := domain + "/" + candidate
			output, code, err := runLaunchctl(ctx, "print", unit)
			if err != nil {
				if code != 113 {
					lookupErr = err
				}
				continue
			}
			mainPID, executable, err := parseLaunchctlService(output)
			if err != nil {
				lookupErr = err
				continue
			}
			if _, owned := parents[mainPID]; !owned {
				continue
			}
			if err := verify(); err != nil {
				return fail(err)
			}
			return &daemonService{unit: unit, executable: executable, mainPID: mainPID, serviceType: "simple"}, nil
		}
	}
	if err := verify(); err != nil {
		return fail(err)
	}
	if lookupErr != nil {
		return fail(lookupErr)
	}
	return nil, nil
}

func userServiceMainPID(ctx context.Context, unit string) (int, error) {
	output, _, err := runLaunchctl(ctx, "print", unit)
	if err != nil {
		return 0, err
	}
	pid, _, err := parseLaunchctlService(output)
	return pid, err
}

func userManagerAvailable(ctx context.Context) bool {
	_, _, err := runLaunchctl(ctx, "managername")
	return err == nil
}

func startUserService(ctx context.Context, unit string) error {
	if err := servicePermissionError(unit); err != nil {
		return err
	}
	_, _, err := runLaunchctl(ctx, "kickstart", unit)
	return err
}

func serviceDescription(unit string) string { return "launchd job " + unit }
func launchctlHint(unit, action string) string {
	prefix := ""
	if strings.HasPrefix(unit, "system/") && launchdEUID() != 0 {
		prefix = "sudo "
	}
	return prefix + "launchctl " + action + " " + unit
}
func serviceRestartHint(unit string) string { return launchctlHint(unit, "kickstart -k") }
func serviceStartHint(unit string) string   { return launchctlHint(unit, "kickstart") }
func serviceStopHint(unit string) string    { return launchctlHint(unit, "bootout") }
func servicePermissionError(unit string) error {
	if strings.HasPrefix(unit, "system/") && launchdEUID() != 0 {
		return fmt.Errorf("system job requires root; run %s", serviceRestartHint(unit))
	}
	return nil
}
func currentExecutable() (string, error) { return os.Executable() }
