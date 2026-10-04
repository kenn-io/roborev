package procutil

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// WithStderr appends the trimmed stderr that exec.Cmd.Output records on an
// *exec.ExitError to err's message. ExitError.Error reports only the exit
// status, so without this a failed command reads "exit status 128" with the
// command's own explanation dropped. The result wraps err, so errors.Is and
// errors.AsType still find the *exec.ExitError. Errors with no captured stderr
// are returned unchanged.
func WithStderr(err error) error {
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return err
	}
	stderr := strings.TrimSpace(string(exitErr.Stderr))
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}
