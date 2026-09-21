package streamfmt

import (
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// XParseColor RGB components have one to four hexadecimal digits.
// https://xorg.freedesktop.org/archive/X11R7.5/doc/man/man3/XQueryColor.3.html
var oscBackgroundReply = regexp.MustCompile(`^\x1b\]11;rgb:([[:xdigit:]]{1,4})/([[:xdigit:]]{1,4})/([[:xdigit:]]{1,4})(?:\x07|\x1b\\)`)

// queryOSCBackground borrows otherwise idle foreground input. peek must not
// remove input; consume must remove exactly the matched prefix or return an error.
// A terminal reply cannot be canceled: after detection stops it may reach the next
// reader. TUI startup must use InitialGlamourStyle and let Bubble Tea query.
func queryOSCBackground(out io.Writer, peek func() (string, error), consume func(int) error, timeout time.Duration) (bool, bool) {
	if pending, err := peek(); err != nil || pending != "" {
		return false, false
	}
	if _, err := io.WriteString(out, "\x1b]11;?\x1b\\"); err != nil {
		return false, false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		reply, err := peek()
		if err != nil {
			return false, false
		}
		if match := oscBackgroundReply.FindStringSubmatch(reply); match != nil {
			if !time.Now().Before(deadline) {
				return false, false
			}
			if err := consume(len(match[0])); err != nil {
				return false, false
			}
			var rgb [3]float64
			for i, component := range match[1:] {
				value, _ := strconv.ParseUint(component, 16, 16)
				rgb[i] = float64(value) / float64(uint64(1)<<(4*len(component))-1)
			}
			// HSL lightness, matching termenv and Bubble Tea's IsDark.
			return min(rgb[0], rgb[1], rgb[2])+max(rgb[0], rgb[1], rgb[2]) < 1, true
		}
		const prefix = "\x1b]11;rgb:"
		if !strings.HasPrefix(prefix, reply) && !strings.HasPrefix(reply, prefix) {
			return false, false
		}
		time.Sleep(min(time.Millisecond, max(0, time.Until(deadline))))
	}
	return false, false
}
