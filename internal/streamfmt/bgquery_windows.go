//go:build windows

package streamfmt

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

// windowsBackgroundQueryTimeout bounds how long we wait for a terminal to
// answer an OSC 11 background-color query before giving up.
const windowsBackgroundQueryTimeout = 300 * time.Millisecond

// oscBackgroundPattern matches an OSC 11 background-color reply, e.g.
// "\x1b]11;rgb:1e1e/1e1e/1e1e" (terminator, BEL or ST, is stripped by the
// caller before matching).
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
	defer windows.SetConsoleMode(stdin, oldMode)

	if _, err := os.Stdout.WriteString("\x1b]11;?\x1b\\"); err != nil {
		return false, false
	}

	type readResult struct {
		line string
		err  error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		// The reply is terminated by BEL (\a); a terminal that answers with
		// ST (ESC \) instead still contains a \a-free line here, which
		// oscBackgroundPattern still matches on the digits before the
		// terminator, so both forms parse correctly.
		reply, err := bufio.NewReader(os.Stdin).ReadString('\a')
		resultCh <- readResult{reply, err}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			return false, false
		}
		return parseOSCBackgroundIsDark(res.line)
	case <-time.After(windowsBackgroundQueryTimeout):
		// The query goroutine is left running; if the terminal answers late,
		// stray bytes may be consumed from stdin by it rather than by
		// whatever reads stdin next. This mirrors the same tradeoff termenv
		// itself makes on Unix (a bounded wait before giving up), and is why
		// this query must run once at startup, before bubbletea takes stdin.
		return false, false
	}
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
