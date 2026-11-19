package proctrack

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libSystem functions. x/sys/unix has no darwin waitid or libproc bindings, and
// the build stays CGO-free, so these are called through purego.
var libSystem = sync.OnceValues(func() (*libSystemFuncs, error) {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("load libSystem: %w", err)
	}
	var f libSystemFuncs
	for name, p := range map[string]*uintptr{
		"waitid": &f.waitid, "proc_listallpids": &f.listAllPids, "proc_pidinfo": &f.pidInfo,
	} {
		if *p, err = purego.Dlsym(lib, name); err != nil {
			return nil, fmt.Errorf("load libSystem %s: %w", name, err)
		}
	}
	return &f, nil
})

type libSystemFuncs struct {
	waitid, listAllPids, pidInfo uintptr
}

const (
	darwinPPID         = 1 // idtype_t P_PID
	darwinWEXITED      = 0x04
	darwinWNOWAIT      = 0x20
	procPidTaskAllInfo = 2
	siginfoSize        = 104
)

// waitExit blocks until pid has exited, without reaping it.
func waitExit(pid int) error {
	f, err := libSystem()
	if err != nil {
		return err
	}
	var info [siginfoSize]byte
	for {
		r, _, errno := purego.SyscallN(f.waitid, darwinPPID, uintptr(pid),
			uintptr(unsafe.Pointer(&info[0])), darwinWEXITED|darwinWNOWAIT)
		if int32(r) == 0 {
			return nil
		}
		if syscall.Errno(errno) != syscall.EINTR {
			return fmt.Errorf("waitid %d: %w", pid, syscall.Errno(errno))
		}
	}
}

func readTable() (map[int]Proc, map[int]struct{}, error) {
	f, err := libSystem()
	if err != nil {
		return nil, nil, err
	}
	n, _, errno := purego.SyscallN(f.listAllPids, 0, 0)
	if int32(n) <= 0 {
		return nil, nil, fmt.Errorf("proc_listallpids: %w", syscall.Errno(errno))
	}
	// Leave room for processes started between the two calls.
	pids := make([]int32, int32(n)+64)
	n, _, errno = purego.SyscallN(f.listAllPids,
		uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)*4))
	if int32(n) <= 0 {
		return nil, nil, fmt.Errorf("proc_listallpids: %w", syscall.Errno(errno))
	}
	pids = pids[:min(int(int32(n)), len(pids))]
	table := make(map[int]Proc, len(pids))
	unreadable := map[int]struct{}{}
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		p, err := readProc(int(pid))
		switch {
		case err == nil:
			table[int(pid)] = p
		case errors.Is(err, syscall.EPERM):
			unreadable[int(pid)] = struct{}{}
		}
	}
	return table, unreadable, nil
}

// readProc reads one process. A zombie has no task information, so it reads
// as gone.
func readProc(pid int) (Proc, error) {
	f, err := libSystem()
	if err != nil {
		return Proc{}, err
	}
	var buf [taskAllInfoSize]byte
	size, _, errno := purego.SyscallN(f.pidInfo, uintptr(pid), procPidTaskAllInfo, 0,
		uintptr(unsafe.Pointer(&buf[0])), taskAllInfoSize)
	p, err := parseTaskAllInfo(pid, buf[:], int(int32(size)))
	if err != nil && errno != 0 {
		return Proc{}, fmt.Errorf("%w: %w", err, syscall.Errno(errno))
	}
	return p, err
}
