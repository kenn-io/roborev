package autotype

import (
	"fmt"

	"github.com/bmatcuk/doublestar/v4"
)

// AnyMatch reports whether any of the given patterns matches path using
// doublestar (bash-globstar) semantics. Returns an error if a pattern is
// malformed.
func AnyMatch(patterns []string, path string) (bool, error) {
	for _, p := range patterns {
		ok, err := doublestar.Match(p, path)
		if err != nil {
			return false, fmt.Errorf("invalid glob %q: %w", p, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// AllMatch reports whether every path in paths matches at least one pattern
// in patterns. Returns false for an empty path list (nothing to match).
// Returns an error if any pattern is malformed.
func AllMatch(patterns, paths []string) (bool, error) {
	if len(paths) == 0 {
		return false, nil
	}
	for _, path := range paths {
		ok, err := AnyMatch(patterns, path)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}
