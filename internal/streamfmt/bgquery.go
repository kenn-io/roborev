package streamfmt

import (
	"os"
	"strings"
	"time"
)

// windowsBackgroundQueryTimeout bounds the entire reply, including partial
// responses and non-text console events.
const windowsBackgroundQueryTimeout = 300 * time.Millisecond

// readOSCBackground reads through BEL or ST without reading past the reply.
// read must return within the supplied timeout; a zero rune is a non-text event.
func readOSCBackground(read func(time.Duration) (rune, error)) (string, error) {
	deadline := time.Now().Add(windowsBackgroundQueryTimeout)
	var reply strings.Builder
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", os.ErrDeadlineExceeded
		}
		c, err := read(remaining)
		if err != nil {
			return "", err
		}
		if c == 0 {
			continue
		}
		reply.WriteRune(c)
		if c == '\a' || strings.HasSuffix(reply.String(), "\x1b\\") {
			return reply.String(), nil
		}
	}
}
