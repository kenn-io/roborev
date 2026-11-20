package proctrack

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// waitExit blocks until pid has exited, without reaping it. It also returns at
// once for a process that exited before the call.
func waitExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			if err != nil {
				return fmt.Errorf("waitid %d: %w", pid, err)
			}
			return nil
		}
	}
}

// killIdentity sends SIGKILL to the process only if it is still the observed
// one. A pidfd pins the process, so a PID reused after the start-time check
// cannot receive the signal. It returns errNoPidfd on kernels before 5.3.
func killIdentity(id ident) error {
	fd, err := unix.PidfdOpen(id.pid, 0)
	switch {
	case errors.Is(err, unix.ESRCH):
		return nil
	case errors.Is(err, unix.ENOSYS):
		return errNoPidfd
	case err != nil:
		return err
	}
	defer unix.Close(fd)
	p, err := readProc(id.pid)
	if err != nil || p.Start != id.start {
		return nil // exited, or the PID now names another process
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}
