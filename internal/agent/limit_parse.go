package agent

import (
	"strings"
	"time"
)

// minCooldown / maxCooldown clamp parsed durations to a sane range so a
// pathological message ("reset after 1ms" or "reset after 100h") does not
// produce a useless cooldown.
const (
	minCooldown = 1 * time.Minute
	maxCooldown = 24 * time.Hour
)

// ParseResetDuration extracts a Go-format duration from a "reset after
// <dur>" substring in errMsg (case-insensitive). Returns 0 if no such
// substring is present or the duration is unparseable. Clamps positive
// values to [minCooldown, maxCooldown].
func ParseResetDuration(errMsg string) time.Duration {
	lower := strings.ToLower(errMsg)
	_, after, ok := strings.Cut(lower, "reset after ")
	if !ok {
		return 0
	}
	// Slice lower (not errMsg): a few runes (e.g. "İ" U+0130 → "i")
	// shorten under strings.ToLower, so byte indices computed against
	// lower can land in the middle of a UTF-8 sequence in errMsg.
	// time.ParseDuration accepts the lowercase unit suffixes anyway.
	rest := after
	token := rest
	if sp := strings.IndexAny(rest, " \t\n,;)"); sp > 0 {
		token = rest[:sp]
	}
	token = strings.TrimRight(token, ".,;:)]}")
	d, err := time.ParseDuration(token)
	if err != nil || d <= 0 {
		return 0
	}
	if d < minCooldown {
		return minCooldown
	}
	if d > maxCooldown {
		return maxCooldown
	}
	return d
}

// ParseResetTime extracts an absolute reset time from messages like
// "resets at 5:42 PM", "resets 5:42pm", or "try again at 17:42". Interprets the parsed
// clock time in the local timezone. Returns the zero time.Time if no
// recognized phrase is present or the time is unparseable.
//
// If the parsed clock time is at or before now-on-the-same-day, the
// returned time rolls forward to the same wall-clock time on the next
// local calendar day so callers that compute "time until reset" never
// get a negative duration. Rollover is DST-safe via time.Date day
// arithmetic; on a 23-hour or 25-hour day, Go normalizes the offset
// so the returned wall-clock time matches the user's local clock.
func ParseResetTime(errMsg string) time.Time {
	return parseResetTimeAt(errMsg, time.Now())
}

// parseResetTimeAt is ParseResetTime with an injectable clock for tests.
func parseResetTimeAt(errMsg string, now time.Time) time.Time {
	lower := strings.ToLower(errMsg)
	var idx int
	switch {
	case strings.Contains(lower, "resets at "):
		idx = strings.Index(lower, "resets at ") + len("resets at ")
	case strings.Contains(lower, "resets "):
		idx = strings.Index(lower, "resets ") + len("resets ")
	case strings.Contains(lower, "try again at "):
		idx = strings.Index(lower, "try again at ") + len("try again at ")
	default:
		return time.Time{}
	}
	// Slice lower (not errMsg) so a rune that shortens under ToLower
	// can't shift byte offsets out of alignment with errMsg. The
	// token feeds time.Parse with lowercase formats below.
	rest := lower[idx:]
	end := len(rest)
	for i, r := range rest {
		// Stop at sentence-ending punctuation or newline.
		if r == '.' || r == ',' || r == ';' || r == '\n' || r == '(' || r == ')' {
			end = i
			break
		}
	}
	token := strings.TrimSpace(rest[:end])

	formats := []string{
		"3:04 pm",
		"3:04pm",
		"15:04",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, token); err == nil {
			candidate := time.Date(
				now.Year(), now.Month(), now.Day(),
				t.Hour(), t.Minute(), 0, 0, now.Location(),
			)
			// Roll forward when the parsed wall-clock time is at-or-before
			// now. Using time.Date with Day()+1 (instead of Add(24h)) is
			// DST-safe: Go normalizes lost/gained hours so the returned
			// time always means "same wall-clock time tomorrow" in
			// now.Location().
			if !candidate.After(now) {
				candidate = time.Date(
					now.Year(), now.Month(), now.Day()+1,
					t.Hour(), t.Minute(), 0, 0, now.Location(),
				)
			}
			return candidate
		}
	}
	return time.Time{}
}
