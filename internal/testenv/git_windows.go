package testenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

func runFixtureGit(ctx context.Context, realGit string) int {
	// Nil security attributes create a non-inheritable handle. Children inherit
	// membership, not this handle: killing the launcher closes the last handle.
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(job)
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Keep the sole job handle open until the recognized launcher's syscall.Exit.
	// Closing it here would terminate this process before its exit status is set.
	cmd := exec.CommandContext(ctx, realGit, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
