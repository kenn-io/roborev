package streamfmt

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	xwindows "github.com/charmbracelet/x/windows"
	"github.com/muesli/termenv"
	"golang.org/x/sys/windows"
)

// Use termenv v0.16.0's OSC timeout as a single foreground-query budget so
// partial replies cannot extend detection indefinitely:
// https://github.com/muesli/termenv/blob/v0.16.0/termenv_unix.go#L16-L19
const windowsBackgroundTimeout = 5 * time.Second

// Refinement and parallel fixes construct multiple formatters. Query only once,
// including failed attempts, so they cannot overlap or create stale reply pairs.
var platformHasDarkBackground = sync.OnceValue(queryWindowsBackground)

func queryWindowsBackground() bool {
	if os.Getenv("WT_SESSION") == "" {
		return true
	}
	stdin := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if windows.GetConsoleMode(stdin, &mode) != nil ||
		windows.GetConsoleMode(windows.Handle(os.Stdout.Fd()), &mode) != nil {
		return true
	}
	restore, err := termenv.EnableVirtualTerminalProcessing(termenv.NewOutput(os.Stdout))
	if err != nil {
		return true
	}
	defer func() { _ = restore() }()

	var records []xwindows.InputRecord
	var ends []int
	dark, ok := queryOSCBackground(os.Stdout, func() (string, error) {
		var n uint32
		if err := xwindows.GetNumberOfConsoleInputEvents(stdin, &n); err != nil || n == 0 {
			return "", err
		}
		records = make([]xwindows.InputRecord, n)
		if err := xwindows.PeekConsoleInput(stdin, &records[0], n, &n); err != nil {
			return "", err
		}
		records = records[:n]
		ends = ends[:0]
		var text strings.Builder
		for _, record := range records {
			key := record.KeyEvent()
			if record.EventType != xwindows.KEY_EVENT || !key.KeyDown {
				text.WriteByte(0) // Preserve non-text records as a mismatch.
			} else {
				text.WriteString(strings.Repeat(string(key.Char), int(max(key.RepeatCount, 1))))
			}
			ends = append(ends, text.Len())
		}
		return text.String(), nil
	}, func(bytes int) error {
		last := slices.Index(ends, bytes)
		if last < 0 {
			return fmt.Errorf("background reply ends inside a repeated input record")
		}
		var n uint32
		// Foreground callers own stdin; the TUI never enters this path.
		if err := xwindows.ReadConsoleInput(stdin, &records[0], uint32(last+1), &n); err != nil {
			return err
		}
		if int(n) != last+1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}, windowsBackgroundTimeout)
	if !ok {
		return true
	}
	return dark
}
