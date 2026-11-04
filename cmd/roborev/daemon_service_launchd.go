package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// parseProcArgs2 reads Darwin's native int argc, executable path and padding,
// exactly argc argument strings, then environment strings. Arguments may be
// empty and must never be mistaken for environment variables.
func parseProcArgs2(data []byte) (executable, label string, err error) {
	invalid := errors.New("process arguments or environment unavailable")
	if len(data) < 4 {
		return "", "", invalid
	}
	argc := int(int32(binary.NativeEndian.Uint32(data[:4])))
	data = data[4:]
	if argc <= 0 || argc > len(data) {
		return "", "", invalid
	}
	path, rest, ok := bytes.Cut(data, []byte{0})
	if !ok || len(path) == 0 {
		return "", "", invalid
	}
	executable = string(path)
	data = bytes.TrimLeft(rest, "\x00")
	for range argc {
		_, rest, ok = bytes.Cut(data, []byte{0})
		if !ok {
			return "", "", invalid
		}
		data = rest
	}
	seenEnv, seenLabel := false, false
	for len(data) > 0 && data[0] != 0 {
		entry, rest, ok := bytes.Cut(data, []byte{0})
		if !ok {
			return "", "", invalid
		}
		seenEnv = true
		if value, found := strings.CutPrefix(string(entry), "XPC_SERVICE_NAME="); found {
			if seenLabel {
				return "", "", errors.New("ambiguous process service label")
			}
			label, seenLabel = value, true
		}
		data = rest
	}
	// The kernel may omit the environment for a restricted process. That is
	// unknown ownership, not evidence that launchd did not start it.
	if !seenEnv {
		return "", "", invalid
	}
	if label == "0" {
		label = ""
	}
	return executable, label, nil
}

// launchctl print is explicitly not an API. Recognize only the observed
// top-level fields and reject incomplete/ambiguous output rather than allow
// an uncertain owner to fall back to detached startup.
func parseLaunchctlService(data []byte) (pid int, executable string, err error) {
	seen := map[string]bool{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line, top := strings.CutPrefix(line, "\t")
		if !top || strings.HasPrefix(line, "\t") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if key != "pid" && key != "program" && key != "state" {
			continue
		}
		if !found || !strings.HasPrefix(line, key+" = ") || seen[key] || strings.TrimSpace(value) == "" {
			return 0, "", fmt.Errorf("launchd %s unavailable or ambiguous", key)
		}
		seen[key] = true
		value = strings.TrimSpace(value)
		switch key {
		case "pid":
			for _, char := range value {
				if char < '0' || char > '9' {
					return 0, "", errors.New("launchd PID unavailable")
				}
			}
			pid, err = strconv.Atoi(value)
			if err != nil || pid <= 0 {
				return 0, "", errors.New("launchd PID unavailable")
			}
		case "program":
			executable = value
		}
	}
	if !seen["state"] || !seen["program"] {
		return 0, "", errors.New("launchd state or program unavailable")
	}
	return pid, executable, nil
}

// parseLaunchctlDomain reads only the direct services table. A missing table,
// changed row format or duplicate label leaves ownership uncertain.
func parseLaunchctlDomain(data []byte) (map[string]int, error) {
	services := map[string]int{}
	seen, inside, closed := false, false, false
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "\tservices = {" {
			if seen {
				return nil, errors.New("ambiguous launchd services table")
			}
			seen, inside = true, true
			continue
		}
		if !inside {
			continue
		}
		if line == "\t}" {
			inside, closed = false, true
			continue
		}
		fields := strings.Fields(line)
		if !strings.HasPrefix(line, "\t\t") || len(fields) != 3 {
			return nil, errors.New("launchd services table unavailable")
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid < 0 || strings.HasPrefix(fields[0], "+") {
			return nil, errors.New("launchd service PID unavailable")
		}
		if _, duplicate := services[fields[2]]; duplicate {
			return nil, errors.New("ambiguous launchd service label")
		}
		services[fields[2]] = pid
	}
	if !seen || !closed {
		return nil, errors.New("launchd services table unavailable")
	}
	return services, nil
}
