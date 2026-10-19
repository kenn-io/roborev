package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"go.kenn.io/roborev/internal/procutil"
)

var (
	readServiceProcessFile = os.ReadFile
	runSystemctl           = runUserSystemctl
	// errUserManagerUnavailable leaves a responsive daemon in use without recovery.
	errUserManagerUnavailable = errors.New("user service manager unavailable")
)

// runUserSystemctl runs a user-manager command bounded by ctx.
func runUserSystemctl(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		// systemctl finds the user manager through the runtime directory.
		cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+filepath.Join("/run/user", strconv.Itoa(os.Getuid())))
	}
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), procutil.WithStderr(err))
	}
	return output, nil
}

func showUserService(ctx context.Context, unit string, properties string) (map[string]string, error) {
	output, err := runSystemctl(ctx, "show", "--property="+properties, "--", unit)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for line := range strings.SplitSeq(string(output), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	return values, nil
}

func userServiceMainPID(ctx context.Context, unit string) (int, error) {
	values, err := showUserService(ctx, unit, "MainPID")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(values["MainPID"])
}

// userManagerAvailable reads a manager property; it never starts a unit.
func userManagerAvailable(ctx context.Context) bool {
	_, err := runSystemctl(ctx, "show", "--property=Version")
	return err == nil
}

// startUserService leaves a starting or running unit alone.
func startUserService(ctx context.Context, unit string) error {
	_, err := runSystemctl(ctx, "start", "--", unit)
	return err
}

func serviceCgroup(pid int) (string, error) {
	data, err := readServiceProcessFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", &managedRecoveryError{"unknown", fmt.Errorf("cannot determine owner of daemon PID %d: %w", pid, err)}
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		_, belowUser, _ := strings.Cut(fields[2], fmt.Sprintf("/user@%d.service/", os.Getuid()))
		for part := range strings.SplitSeq(belowUser, "/") {
			if strings.HasSuffix(part, ".service") {
				return part, nil
			}
		}
	}
	return "", nil
}

// lookupDaemonService returns the user service that started pid or its parent.
// A daemon that only shares the unit's cgroup is unmanaged.
func lookupDaemonService(ctx context.Context, pid int) (*daemonService, error) {
	unit, err := serviceCgroup(pid)
	if err != nil || unit == "" {
		return nil, err
	}
	fail := func(err error) (*daemonService, error) { return nil, &managedRecoveryError{unit, err} }
	values, err := showUserService(ctx, unit, "MainPID,Type,RemainAfterExit,Transient,ExecStart")
	if err != nil {
		return fail(fmt.Errorf("%w: %w", errUserManagerUnavailable, err))
	}
	mainPID, err := strconv.Atoi(values["MainPID"])
	if err != nil {
		return fail(fmt.Errorf("service MainPID unavailable: %w", err))
	}
	for parent := pid; parent != mainPID; {
		if parent <= 1 {
			return nil, nil
		}
		data, err := readServiceProcessFile(fmt.Sprintf("/proc/%d/stat", parent))
		if err != nil {
			return fail(fmt.Errorf("read daemon parent: %w", err))
		}
		fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
		if len(fields) < 2 {
			return fail(errors.New("daemon parent unavailable"))
		}
		if parent, err = strconv.Atoi(fields[1]); err != nil {
			return fail(err)
		}
	}
	// systemctl separates the configured path from argv with a structured
	// delimiter, so spaces in the executable path remain part of the path.
	command, ok := strings.CutPrefix(values["ExecStart"], "{ path=")
	executable, _, complete := strings.Cut(command, " ; argv[]=")
	if !ok || !complete || executable == "" {
		return fail(errors.New("service ExecStart executable unavailable"))
	}
	return &daemonService{
		unit: unit, executable: executable, mainPID: mainPID,
		serviceType: values["Type"], remainAfterExit: values["RemainAfterExit"] == "yes", transient: values["Transient"] == "yes",
	}, nil
}
