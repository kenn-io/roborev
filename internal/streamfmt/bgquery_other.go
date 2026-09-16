//go:build !windows

package streamfmt

// platformHasDarkBackground has no override on non-Windows platforms:
// termenv's own OSC 11 query already handles background detection there.
func platformHasDarkBackground() (isDark bool, ok bool) {
	return false, false
}
