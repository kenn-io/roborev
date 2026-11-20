package proctrack

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// struct proc_taskallinfo from macOS libproc: proc_bsdinfo (136 bytes) then
// proc_taskinfo (96 bytes).
const taskAllInfoSize = 232

// parseTaskAllInfo decodes proc_pidinfo(PROC_PIDTASKALLINFO). A short or zero
// return is a failed read, never a process with zero RSS.
func parseTaskAllInfo(pid int, buf []byte, size int) (Proc, error) {
	if size != taskAllInfoSize || len(buf) < taskAllInfoSize {
		return Proc{}, fmt.Errorf("proc_pidinfo %d: returned %d bytes, want %d", pid, size, taskAllInfoSize)
	}
	le := binary.LittleEndian
	comm, _, _ := bytes.Cut(buf[48:64], []byte{0})
	sec, usec := le.Uint64(buf[120:]), le.Uint64(buf[128:])
	return Proc{
		PID:   pid,
		PPID:  int(le.Uint32(buf[16:])),
		PGID:  int(le.Uint32(buf[100:])),
		Start: sec*1_000_000 + usec,
		RSS:   le.Uint64(buf[136+8:]),
		Name:  string(comm),
	}, nil
}
