package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	systemdbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

func serviceCgroup(pid int) string {
	data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		for part := range strings.SplitSeq(fields[2], "/") {
			if strings.HasSuffix(part, ".service") && !strings.HasPrefix(part, "user@") {
				return part
			}
		}
	}
	return ""
}

func lookupDaemonService(ctx context.Context, pid int) (*daemonService, error) {
	unit := serviceCgroup(pid)
	if unit == "" {
		return nil, nil
	}
	fail := func(err error) (*daemonService, error) { return nil, &managedRecoveryError{unit, err} }
	conn, err := systemdbus.NewUserConnectionContext(ctx)
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	path, err := conn.GetUnitByPID(ctx, uint32(pid))
	if err != nil {
		return fail(err)
	}
	if path != dbus.ObjectPath("/org/freedesktop/systemd1/unit/"+systemdbus.PathBusEscape(unit)) {
		return fail(fmt.Errorf("inconsistent service ownership"))
	}
	service, err := conn.GetUnitTypePropertiesContext(ctx, unit, "Service")
	if err != nil {
		return fail(err)
	}
	mainPID, ok := service["MainPID"].(uint32)
	if !ok {
		return fail(fmt.Errorf("service MainPID unavailable"))
	}
	if mainPID != uint32(pid) {
		// A detached daemon can inherit the launching service's cgroup.
		return nil, nil
	}
	// ExecStart is a(ssbttttuii), including execution timestamps and exit status.
	var commands []struct {
		Path                                                       string
		Args                                                       []string
		IgnoreFailure                                              bool
		StartRealtime, StartMonotonic, ExitRealtime, ExitMonotonic uint64
		PID                                                        uint32
		Code, Status                                               int32
	}
	if err := dbus.Store([]any{service["ExecStart"]}, &commands); err != nil {
		return fail(fmt.Errorf("read ExecStart: %w", err))
	}
	if len(commands) != 1 || len(commands[0].Args) < 3 || commands[0].Args[1] != "daemon" || commands[0].Args[2] != "run" {
		return nil, nil
	}
	return &daemonService{unit: unit, executable: commands[0].Path}, nil
}

func startDaemonService(ctx context.Context, unit string) error {
	conn, err := systemdbus.NewUserConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	result := make(chan string, 1)
	if _, err := conn.StartUnitContext(ctx, unit, "replace", result); err != nil {
		return err
	}
	select {
	case outcome := <-result:
		if outcome != "done" {
			return fmt.Errorf("unit start: %s", outcome)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
