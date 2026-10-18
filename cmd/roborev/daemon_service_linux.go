package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	systemdbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

var readServiceProcessFile = os.ReadFile

type userServiceConnection struct{ *systemdbus.Conn }

func serviceCgroup(pid int) string {
	data, _ := readServiceProcessFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		_, belowUser, _ := strings.Cut(fields[2], fmt.Sprintf("/user@%d.service/", os.Getuid()))
		for part := range strings.SplitSeq(belowUser, "/") {
			if strings.HasSuffix(part, ".service") {
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
	// The connection lasts until manager.close, across recovery's fresh budgets.
	conn, err := systemdbus.NewUserConnectionContext(context.WithoutCancel(ctx))
	if err != nil {
		return nil, &managedRecoveryError{unit, fmt.Errorf("%w: %w", errUserBusUnavailable, err)}
	}
	manager := userServiceConnection{conn}
	owner, err := manager.lookup(ctx, pid)
	if owner == nil || err != nil {
		conn.Close()
	}
	return owner, err
}

func (conn userServiceConnection) lookup(ctx context.Context, pid int) (*daemonService, error) {
	unit := serviceCgroup(pid)
	if unit == "" {
		return nil, nil
	}
	fail := func(err error) (*daemonService, error) { return nil, &managedRecoveryError{unit, err} }
	service, err := conn.GetUnitTypePropertiesContext(ctx, unit, "Service")
	if err != nil {
		return fail(err)
	}
	mainPID, ok := service["MainPID"].(uint32)
	if !ok {
		return fail(fmt.Errorf("service MainPID unavailable"))
	}
	if mainPID != uint32(pid) {
		parent := pid
		for parent > 1 && uint32(parent) != mainPID {
			data, err := readServiceProcessFile(fmt.Sprintf("/proc/%d/stat", parent))
			if err != nil {
				return fail(fmt.Errorf("read daemon parent: %w", err))
			}
			fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
			if len(fields) < 2 {
				return fail(fmt.Errorf("daemon parent unavailable"))
			}
			parent, err = strconv.Atoi(fields[1])
			if err != nil {
				return fail(err)
			}
		}
		if uint32(parent) != mainPID {
			return nil, nil
		}
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
	if len(commands) != 1 {
		return fail(fmt.Errorf("ExecStart must select one executable"))
	}
	return &daemonService{unit: unit, executable: commands[0].Path, mainPID: int(mainPID), simple: service["Type"] == "simple", manager: &daemonServiceManager{conn.lookup, conn.start, conn.Close}}, nil
}

func (conn userServiceConnection) start(ctx context.Context, unit string) error {
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
