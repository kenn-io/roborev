//go:build !linux && !darwin

package proctrack

import (
	"errors"
	"os/exec"
)

// errUnsupported reports a platform without process groups or a process table.
var errUnsupported = errors.New("process tracking is not supported on this platform")

const supported = false

func setProcessGroup(*exec.Cmd)                          {}
func waitExit(int) error                                 { return errUnsupported }
func killIdentity(ident) error                           { return errUnsupported }
func readTable() (map[int]Proc, map[int]struct{}, error) { return nil, nil, errUnsupported }

var killGroup = func(int) error { return errUnsupported }
