package proctrack

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

var pageSize = uint64(os.Getpagesize())

// readTable reads every process from /proc. Processes that exit during the
// scan are skipped; unreadable ones are reported by PID.
func readTable() (map[int]Proc, map[int]struct{}, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil, fmt.Errorf("read /proc: %w", err)
	}
	table := make(map[int]Proc, len(entries))
	unreadable := map[int]struct{}{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, err := readProc(pid)
		switch {
		case err == nil:
			table[pid] = p
		case errors.Is(err, fs.ErrPermission):
			unreadable[pid] = struct{}{}
		}
	}
	return table, unreadable, nil
}

func readProc(pid int) (Proc, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return Proc{}, err
	}
	return parseStat(pid, data)
}

// parseStat parses /proc/<pid>/stat. The command name is in parentheses and
// may itself contain spaces and parentheses, so fields start after the last ')'.
func parseStat(pid int, data []byte) (Proc, error) {
	open := bytes.IndexByte(data, '(')
	end := bytes.LastIndexByte(data, ')')
	if open < 0 || end < open {
		return Proc{}, fmt.Errorf("parse /proc/%d/stat: no command name", pid)
	}
	// Fields 3 onward: state ppid pgrp session tty_nr tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority nice num_threads
	// itrealvalue starttime vsize rss.
	f := strings.Fields(string(data[end+1:]))
	if len(f) < 22 {
		return Proc{}, fmt.Errorf("parse /proc/%d/stat: %d fields", pid, len(f))
	}
	ppid, err1 := strconv.Atoi(f[1])
	pgid, err2 := strconv.Atoi(f[2])
	start, err3 := strconv.ParseUint(f[19], 10, 64)
	rss, err4 := strconv.ParseInt(f[21], 10, 64)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return Proc{}, fmt.Errorf("parse /proc/%d/stat: %w", pid, err)
	}
	return Proc{
		PID: pid, PPID: ppid, PGID: pgid, Start: start,
		RSS:  uint64(max(rss, 0)) * pageSize,
		Name: string(data[open+1 : end]),
	}, nil
}
