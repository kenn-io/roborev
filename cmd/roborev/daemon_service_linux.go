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

type userServiceConnection struct {
	*systemdbus.Conn
	// systemd calls RefUnit on the method connection; Close releases the reference.
	systemd dbus.BusObject
}

// dialUserService keeps the method connection available for unit references.
func dialUserService(ctx context.Context) (userServiceConnection, error) {
	// NewConnection dials method calls first, then signals.
	var method *dbus.Conn
	conn, err := systemdbus.NewConnection(func() (*dbus.Conn, error) {
		bus, err := dbus.ConnectSessionBus(dbus.WithContext(ctx), dbus.WithAuth(dbus.AuthExternal(strconv.Itoa(os.Getuid()))))
		if method == nil {
			method = bus
		}
		return bus, err
	})
	if err != nil {
		return userServiceConnection{}, err
	}
	return userServiceConnection{conn, method.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")}, nil
}

// hold looks up the daemon's service and references it so a transient unit
// survives its main process exiting until recovery closes the connection.
func (conn userServiceConnection) hold(ctx context.Context, pid int) (*daemonService, error) {
	owner, err := conn.lookup(ctx, pid)
	if owner == nil || err != nil {
		return owner, err
	}
	if err := conn.systemd.CallWithContext(ctx, "org.freedesktop.systemd1.Manager.RefUnit", 0, owner.unit).Store(); err != nil {
		return nil, &managedRecoveryError{owner.unit, fmt.Errorf("reference unit: %w", err)}
	}
	return owner, nil
}

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
	conn, err := dialUserService(context.WithoutCancel(ctx))
	if err != nil {
		return nil, &managedRecoveryError{unit, fmt.Errorf("%w: %w", errUserBusUnavailable, err)}
	}
	owner, err := conn.hold(ctx, pid)
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
	return &daemonService{unit: unit, executable: commands[0].Path, mainPID: int(mainPID), manager: &daemonServiceManager{conn.lookup, conn.start, conn.Close}}, nil
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
