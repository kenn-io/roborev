//go:build windows

package streamfmt

import (
	"os"
	"regexp"
	"strconv"
	"time"

	xwindows "github.com/charmbracelet/x/windows"
	"golang.org/x/sys/windows"
)

// oscBackgroundPattern matches an OSC 11 background-color reply, e.g.
// "\x1b]11;rgb:1e1e/1e1e/1e1e" followed by BEL or ST.
var oscBackgroundPattern = regexp.MustCompile(`\x1b\]11;rgb:([0-9a-fA-F]+)/([0-9a-fA-F]+)/([0-9a-fA-F]+)`)

// platformHasDarkBackground queries the terminal's background color via
// OSC 11. termenv does not implement this on Windows — its backgroundColor()
// is hardcoded to black (see termenv_windows.go), so termenv.HasDarkBackground()
// always reports "dark" there regardless of the actual console theme. This is
// roborev's own best-effort fallback, and is only attempted inside Windows
// Terminal (WT_SESSION set), the one Windows console host known to answer
// OSC 11; legacy conhost does not, so ok is false there and callers keep
// termenv's existing (always-dark) result.
func platformHasDarkBackground() (isDark bool, ok bool) {
	if os.Getenv("WT_SESSION") == "" {
		return false, false
	}

	stdin := windows.Handle(os.Stdin.Fd())
	var oldMode uint32
	if err := windows.GetConsoleMode(stdin, &oldMode); err != nil {
		return false, false
	}
	rawMode := oldMode &^ (windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT)
	if err := windows.SetConsoleMode(stdin, rawMode); err != nil {
		return false, false
	}
	defer func() { _ = windows.SetConsoleMode(stdin, oldMode) }()

	if _, err := os.Stdout.WriteString("\x1b]11;?\x1b\\"); err != nil {
		return false, false
	}

	// Query before bubbletea takes stdin. Keep all records queued until a
	// complete reply is recognized, including prefixes shared with user input.
	var records []xwindows.InputRecord
	var offset, repeats int
	reply, err := readOSCBackground(func(remaining time.Duration) (rune, error) {
		deadline := time.Now().Add(remaining)
		// ponytail: re-scan the short OSC reply; batch decoding if this grows
		// beyond background-color queries. Re-peeking also sees repeat counts
		// that grow as more characters arrive in the same console record.
		records = make([]xwindows.InputRecord, offset+1)
		for {
			var n uint32
			if err := xwindows.PeekConsoleInput(stdin, &records[0], uint32(len(records)), &n); err != nil {
				return 0, err
			}
			pos := offset
			for i, record := range records[:n] {
				if record.EventType != xwindows.KEY_EVENT || !record.KeyEvent().KeyDown {
					return 0, nil
				}
				key := record.KeyEvent()
				count := int(max(key.RepeatCount, 1))
				if pos < count {
					records = records[:i+1]
					repeats = count - pos - 1
					offset++
					return key.Char, nil
				}
				pos -= count
			}
			remaining = time.Until(deadline)
			if remaining <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			// A partial reply keeps the input handle signaled. Poll without
			// removing it, so timeout leaves the entire input sequence intact.
			time.Sleep(min(time.Millisecond, remaining))
		}
	}, func() error {
		if repeats != 0 {
			return errNotOSCBackground
		}
		var n uint32
		return xwindows.ReadConsoleInput(stdin, &records[0], uint32(len(records)), &n)
	})
	if err != nil {
		return false, false
	}
	return parseOSCBackgroundIsDark(reply)
}

func parseOSCBackgroundIsDark(reply string) (isDark bool, ok bool) {
	m := oscBackgroundPattern.FindStringSubmatch(reply)
	if m == nil {
		return false, false
	}
	r := hexChannel(m[1])
	g := hexChannel(m[2])
	b := hexChannel(m[3])
	lum := 0.2126*r + 0.7152*g + 0.0722*b
	return lum < 0.5, true
}

// hexChannel normalizes a 1-4 digit hex color channel (as OSC 11 may report
// either 8-bit or 16-bit-per-channel values) to a 0-1 float.
func hexChannel(s string) float64 {
	if len(s) == 0 || len(s) > 4 {
		return 0
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0
	}
	maxVal := uint64(1)<<uint(4*len(s)) - 1
	return float64(v) / float64(maxVal)
}
