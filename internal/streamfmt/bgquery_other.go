//go:build !windows

package streamfmt

import "github.com/muesli/termenv"

func platformHasDarkBackground() bool {
	return termenv.HasDarkBackground()
}
