package tokens

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// agentsviewOutput puts the lookup in a job before its first instruction can
// launch children. Canceling only the direct process leaves grandchildren
// running behind Windows launchers such as Scoop shims.
func agentsviewOutput(cmd *exec.Cmd) ([]byte, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create agentsview process job: %w", err)
	}
	defer func() { _ = windows.CloseHandle(job) }()

	// Do not use KILL_ON_JOB_CLOSE: a successful agentsview command may start
	// its persistent server. Only cancellation terminates the process tree.
	var starting sync.Mutex
	cmd.Cancel = func() error {
		starting.Lock()
		defer starting.Unlock()
		if err := windows.TerminateJobObject(job, 1); err != nil {
			return errors.Join(fmt.Errorf("terminate agentsview process job: %w", err), cmd.Process.Kill())
		}
		return nil
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	// Cancellation must wait until the suspended process belongs to the job.
	starting.Lock()
	err = cmd.Start()
	if err != nil {
		starting.Unlock()
		return nil, err
	}
	err = startAgentsviewInJob(job, uint32(cmd.Process.Pid))
	if err != nil {
		// The process has not run, so it cannot have created descendants.
		_ = cmd.Process.Kill()
	}
	starting.Unlock()
	waitErr := cmd.Wait()
	if err != nil {
		return nil, err
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		exitErr.Stderr = stderr.Bytes()
	}
	return stdout.Bytes(), waitErr
}

func startAgentsviewInJob(job windows.Handle, pid uint32) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("open suspended agentsview process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(process) }()
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return fmt.Errorf("assign agentsview process job: %w", err)
	}

	// os/exec closes the primary thread handle returned by CreateProcess.
	// A newly created suspended process has not run any code or made threads.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("find suspended agentsview thread: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fmt.Errorf("open suspended agentsview thread: %w", err)
		}
		defer func() { _ = windows.CloseHandle(thread) }()
		if _, err := windows.ResumeThread(thread); err != nil {
			return fmt.Errorf("resume agentsview thread: %w", err)
		}
		return nil
	}
	return fmt.Errorf("find suspended agentsview thread: %w", err)
}
