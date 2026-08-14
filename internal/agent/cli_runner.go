package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

type streamingCLISpec struct {
	Name          string
	Command       string
	Args          []string
	Dir           string
	Env           []string
	Stdin         io.Reader
	Output        io.Writer
	StreamStderr  bool
	CaptureStdout bool
	DrainStdout   bool
	Parse         func(io.Reader, *syncWriter) (string, error)
}

type streamingCLIResult struct {
	Result   string
	ParseErr error
	WaitErr  error
	Stderr   string
	Stdout   string
}

func runStreamingCLI(ctx context.Context, spec streamingCLISpec) (streamingCLIResult, error) {
	var result streamingCLIResult

	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	cmd.Dir = spec.Dir
	if spec.Env != nil {
		cmd.Env = append([]string(nil), spec.Env...)
	}
	cmd.Stdin = spec.Stdin
	tracker := configureSubprocess(cmd)

	sw := newSyncWriter(spec.Output)

	var stderrBuf bytes.Buffer
	if spec.StreamStderr && sw != nil {
		cmd.Stderr = io.MultiWriter(&stderrBuf, sw)
	} else {
		cmd.Stderr = &stderrBuf
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return result, fmt.Errorf("create stdout pipe: %w", err)
	}
	stopClosingPipe := closeOnContextDone(ctx, stdoutPipe, tracker)
	defer stopClosingPipe()

	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start %s: %w", spec.Name, err)
	}

	reader := io.Reader(stdoutPipe)
	var stdoutBuf bytes.Buffer
	if spec.CaptureStdout {
		reader = io.TeeReader(stdoutPipe, &stdoutBuf)
	}

	result.Result, result.ParseErr = spec.Parse(reader, sw)

	if spec.DrainStdout {
		if spec.CaptureStdout {
			_, _ = io.Copy(&stdoutBuf, stdoutPipe)
		} else {
			_, _ = io.Copy(io.Discard, stdoutPipe)
		}
	}

	result.WaitErr = cmd.Wait()
	result.Stderr = stderrBuf.String()
	result.Stdout = stdoutBuf.String()

	if ctxErr := contextProcessError(ctx, tracker, result.WaitErr, result.ParseErr); ctxErr != nil {
		return result, ctxErr
	}
	if result.WaitErr == nil {
		if ctxErr := contextProcessError(ctx, tracker, nil, result.ParseErr); ctxErr != nil {
			return result, ctxErr
		}
	}

	return result, nil
}
