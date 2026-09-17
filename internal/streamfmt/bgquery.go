package streamfmt

import (
	"errors"
	"os"
	"strings"
	"time"
)

// windowsBackgroundQueryTimeout bounds the entire reply, including partial
// responses.
const windowsBackgroundQueryTimeout = 300 * time.Millisecond

var errNotOSCBackground = errors.New("input is not an OSC 11 background reply")

// readOSCBackground peeks through BEL or ST and consumes only a complete reply.
// peek must leave input queued and return within the supplied timeout. A zero
// rune represents a non-text event, which aborts the query without consuming it.
func readOSCBackground(peek func(time.Duration) (rune, error), consume func() error) (string, error) {
	const prefix = "\x1b]11;rgb:"
	deadline := time.Now().Add(windowsBackgroundQueryTimeout)
	var reply strings.Builder
	var channel, digits int
	var st bool
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", os.ErrDeadlineExceeded
		}
		c, err := peek(remaining)
		if err != nil {
			return "", err
		}
		switch {
		case reply.Len() < len(prefix):
			if c != rune(prefix[reply.Len()]) {
				return "", errNotOSCBackground
			}
		case st:
			if c != '\\' {
				return "", errNotOSCBackground
			}
		case strings.ContainsRune("0123456789abcdefABCDEF", c):
			// OSC rgb channels use the XParseColor format: 1-4 hex digits.
			// https://xorg.freedesktop.org/archive/X11R7.5/doc/man/man3/XQueryColor.3.html
			digits++
			if digits > 4 {
				return "", errNotOSCBackground
			}
		case c == '/' && channel < 2 && digits > 0:
			channel++
			digits = 0
		case (c == '\a' || c == '\x1b') && channel == 2 && digits > 0:
			st = c == '\x1b'
		default:
			return "", errNotOSCBackground
		}
		reply.WriteRune(c)
		if c == '\a' || (st && c == '\\') {
			if !time.Now().Before(deadline) {
				return "", os.ErrDeadlineExceeded
			}
			return reply.String(), consume()
		}
	}
}
