package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

const (
	// Give os/exec a short output-drain window, then close descriptors inherited
	// by descendants so they cannot keep an exited agent's stream open.
	streamingCLIWaitDelay = 250 * time.Millisecond
	// Bound unread stdout per agent so a stalled parser cannot exhaust daemon
	// memory. The total stream remains unlimited when the parser keeps up.
	streamingCLIMaxBufferedOutput = 1 << 20
)

var errStreamingCLIOutputBacklog = errors.New("agent stdout backlog exceeded 1 MiB limit")

// streamingBuffer lets os/exec drain the process pipe without waiting for the
// parser. That keeps WaitDelay focused on descriptors held by descendants.
type streamingBuffer struct {
	mu     sync.Mutex
	ready  *sync.Cond
	buf    bytes.Buffer
	closed bool
	err    error
}

func newStreamingBuffer() *streamingBuffer {
	b := &streamingBuffer{}
	b.ready = sync.NewCond(&b.mu)
	return b
}

func (b *streamingBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for b.buf.Len() == 0 && !b.closed {
		b.ready.Wait()
	}
	if b.buf.Len() == 0 {
		if b.err != nil {
			return 0, b.err
		}
		return 0, io.EOF
	}
	return b.buf.Read(p)
}

func (b *streamingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return 0, io.ErrClosedPipe
	}
	if len(p) > streamingCLIMaxBufferedOutput-b.buf.Len() {
		b.err = errStreamingCLIOutputBacklog
		b.closed = true
		b.ready.Broadcast()
		return 0, b.err
	}
	n, err := b.buf.Write(p)
	b.ready.Signal()
	return n, err
}

func (b *streamingBuffer) Close() error {
	b.mu.Lock()
	b.closed = true
	b.ready.Broadcast()
	b.mu.Unlock()
	return nil
}

func (b *streamingBuffer) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

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
	cmd.WaitDelay = streamingCLIWaitDelay

	sw := newSyncWriter(spec.Output)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if spec.StreamStderr && sw != nil {
		cmd.Stderr = io.MultiWriter(&stderrBuf, sw)
	}

	stdout := newStreamingBuffer()
	defer stdout.Close()
	cmd.Stdout = stdout

	stopClosingPipe := closeOnContextDone(ctx, stdout, tracker)
	defer stopClosingPipe()

	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start %s: %w", spec.Name, err)
	}

	var waitErr error
	waitDone := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		_ = stdout.Close()
		close(waitDone)
	}()

	reader := io.Reader(stdout)
	var stdoutBuf bytes.Buffer
	if spec.CaptureStdout {
		reader = io.TeeReader(stdout, &stdoutBuf)
	}

	result.Result, result.ParseErr = spec.Parse(reader, sw)

	if spec.DrainStdout {
		if spec.CaptureStdout {
			_, _ = io.Copy(&stdoutBuf, stdout)
		} else {
			_, _ = io.Copy(io.Discard, stdout)
		}
	}

	<-waitDone
	result.WaitErr = waitErr
	if stdoutErr := stdout.Err(); stdoutErr != nil {
		result.WaitErr = stdoutErr
	}
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
	// A descendant-held descriptor is not an agent failure when the direct
	// process exited successfully and all available output was consumed.
	if errors.Is(result.WaitErr, exec.ErrWaitDelay) && cmd.ProcessState.Success() {
		result.WaitErr = nil
	}

	return result, nil
}
