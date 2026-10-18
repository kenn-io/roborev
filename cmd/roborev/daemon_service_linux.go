package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	address := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if address == "" {
		runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
		if runtimeDir == "" {
			runtimeDir = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
		}
		address = "unix:path=" + filepath.Join(runtimeDir, "bus")
	}
	var path string
	for field := range strings.SplitSeq(strings.TrimPrefix(address, "unix:"), ",") {
		if value, ok := strings.CutPrefix(field, "path="); ok {
			path, _ = url.PathUnescape(value)
		} else if value, ok := strings.CutPrefix(field, "abstract="); ok {
			path, _ = url.PathUnescape(value)
			path = "@" + path
		}
	}
	if !strings.HasPrefix(address, "unix:") || path == "" {
		return userServiceConnection{}, fmt.Errorf("user bus requires a Unix socket address")
	}
	// Socket deadlines cover authentication, Hello, and signal registration.
	var sockets []net.Conn
	var method *dbus.Conn
	conn, err := systemdbus.NewConnection(func() (*dbus.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err != nil {
			return nil, err
		}
		sockets = append(sockets, raw)
		if err := raw.SetDeadline(deadline); err != nil {
			return nil, err
		}
		bus, err := dbus.NewConn(raw)
		if err == nil {
			err = bus.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))})
		}
		if err == nil {
			err = bus.Hello()
		}
		if method == nil {
			method = bus
		}
		if err != nil && bus != nil {
			_ = bus.Close()
		}
		return bus, err
	})
	if err == nil && !conn.Connected() {
		err = fmt.Errorf("user bus setup did not complete")
	}
	if err == nil {
		for _, raw := range sockets {
			if err = raw.SetDeadline(time.Time{}); err != nil {
				break
			}
		}
	}
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		for _, raw := range sockets {
			_ = raw.Close()
		}
		return userServiceConnection{}, err
	}
	return userServiceConnection{conn, method.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")}, nil
}

// hold looks up the daemon's service and references it so a transient unit
// survives its main process exiting until recovery closes the connection.
func (conn userServiceConnection) hold(ctx context.Context, pid int, unit string) (*daemonService, error) {
	owner, err := conn.lookupUnit(ctx, pid, unit)
	if owner == nil || err != nil {
		return owner, err
	}
	if err := conn.systemd.CallWithContext(ctx, "org.freedesktop.systemd1.Manager.RefUnit", 0, owner.unit).Store(); err != nil {
		return nil, &managedRecoveryError{owner.unit, fmt.Errorf("reference unit: %w", err)}
	}
	return owner, nil
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

func lookupDaemonService(ctx context.Context, pid int) (*daemonService, error) {
	unit, err := serviceCgroup(pid)
	if err != nil || unit == "" {
		return nil, err
	}
	// The connection lasts until manager.close, across recovery's fresh budgets.
	conn, err := dialUserService(ctx)
	if err != nil {
		return nil, &managedRecoveryError{unit, fmt.Errorf("%w: %w", errUserBusUnavailable, err)}
	}
	owner, err := conn.hold(ctx, pid, unit)
	if owner == nil || err != nil {
		conn.Close()
	}
	return owner, err
}

func (conn userServiceConnection) lookup(ctx context.Context, pid int) (*daemonService, error) {
	unit, err := serviceCgroup(pid)
	if err != nil || unit == "" {
		return nil, err
	}
	return conn.lookupUnit(ctx, pid, unit)
}

func (conn userServiceConnection) lookupUnit(ctx context.Context, pid int, unit string) (*daemonService, error) {
	fail := func(err error) (*daemonService, error) { return nil, &managedRecoveryError{unit, err} }
	service, err := conn.GetUnitTypePropertiesContext(ctx, unit, "Service")
	if err != nil {
		return fail(err)
	}
	mainPID, ok := service["MainPID"].(uint32)
	if !ok {
		return fail(fmt.Errorf("service MainPID unavailable"))
	}
	if int64(mainPID) != int64(pid) {
		parent := pid
		for parent > 1 && int64(parent) != int64(mainPID) {
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
		if int64(parent) != int64(mainPID) {
			return nil, nil
		}
		data, err := readServiceProcessFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return fail(fmt.Errorf("read service child command: %w", err))
		}
		if !daemonRunArgs(strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")) {
			return fail(fmt.Errorf("service child must run daemon run"))
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
	serviceType, _ := service["Type"].(string)
	if !slices.Contains([]string{"simple", "exec", "notify"}, serviceType) {
		return fail(fmt.Errorf("unsupported service Type=%s", serviceType))
	}
	if int64(mainPID) == int64(pid) && !daemonRunArgs(commands[0].Args) {
		return fail(fmt.Errorf("service must launch daemon run directly"))
	}
	return &daemonService{unit: unit, executable: commands[0].Path, mainPID: int(mainPID), manager: &daemonServiceManager{conn.lookup, conn.start, conn.Close}}, nil
}

func daemonRunArgs(args []string) bool {
	if len(args) < 3 {
		return false
	}
	i := slices.Index(args[1:], "daemon") + 1
	return i > 0 && i+1 < len(args) && args[i+1] == "run"
}

func (conn userServiceConnection) start(ctx context.Context, unit string) error {
	result := make(chan string, 1)
	if _, err := conn.RestartUnitContext(ctx, unit, "replace", result); err != nil {
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
