package agent

import (
	"context"
	"io"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunStreamingCLIPreservesOutputWhenParentExitsFirst(t *testing.T) {
	skipIfWindows(t)

	cmdPath := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
(sleep 3 2>/dev/null) &
printf 'complete\n'
exit 0
`)
	startedAt := time.Now()
	result, err := runStreamingCLI(context.Background(), streamingCLISpec{
		Name:    "test",
		Command: cmdPath,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			time.Sleep(50 * time.Millisecond)
			data, readErr := io.ReadAll(r)
			return string(data), readErr
		},
	})

	require.NoError(t, err)
	require.NoError(t, result.WaitErr)
	require.NoError(t, result.ParseErr)
	assert.Equal(t, "complete\n", result.Result)
	assert.Less(t, time.Since(startedAt), 1500*time.Millisecond,
		"runner waited for a descendant-held stdout pipe")
}

func TestRunStreamingCLIPreservesOutputWhenParserIsSlow(t *testing.T) {
	skipIfWindows(t)

	cmdPath := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
dd if=/dev/zero bs=65536 count=1 2>/dev/null
`)
	result, err := runStreamingCLI(context.Background(), streamingCLISpec{
		Name:    "test",
		Command: cmdPath,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			time.Sleep(2 * streamingCLIWaitDelay)
			data, readErr := io.ReadAll(r)
			return string(data), readErr
		},
	})

	require.NoError(t, err)
	require.NoError(t, result.WaitErr)
	require.NoError(t, result.ParseErr)
	resultLen := len(result.Result)
	assert.Equal(t, 65536, resultLen)
}

func TestRunStreamingCLIFailsWhenStdoutBacklogExceedsLimit(t *testing.T) {
	skipIfWindows(t)

	cmdPath := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
dd if=/dev/zero bs=2097152 count=1 2>/dev/null
`)
	result, err := runStreamingCLI(context.Background(), streamingCLISpec{
		Name:    "test",
		Command: cmdPath,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			time.Sleep(2 * streamingCLIWaitDelay)
			data, readErr := io.ReadAll(r)
			return string(data), readErr
		},
	})

	require.NoError(t, err)
	require.Error(t, result.WaitErr)
	assert.ErrorContains(t, result.WaitErr, "stdout backlog exceeded 1 MiB limit")
}

func TestRunStreamingCLIPreservesWaitErrWhenContextCancelsAfterParse(t *testing.T) {
	skipIfWindows(t)

	cmdPath := writeTempCommand(t, "#!/bin/sh\ncase \"$1\" in *etxtbsy*) exit 0;; esac\nprintf 'ok\\n'\nexit 1\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result, err := runStreamingCLI(ctx, streamingCLISpec{
		Name:    "test",
		Command: cmdPath,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			data, readErr := io.ReadAll(r)
			require.NoError(t, readErr)
			cancel()
			return string(data), fs.ErrClosed
		},
	})

	require.NoError(t, err)
	require.Error(t, result.WaitErr)
	assert.Equal(t, "ok\n", result.Result)
	require.ErrorIs(t, result.ParseErr, fs.ErrClosed)
	assert.Contains(t, result.WaitErr.Error(), "exit status 1")
}
