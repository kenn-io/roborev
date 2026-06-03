package review

import "time"

// RetrySchedule defines CI review retry backoff and give-up bounds.
type RetrySchedule struct {
	Base          time.Duration // first delay
	Cap           time.Duration // max single delay
	TransientWall time.Duration // give up transient retries after this since first attempt
	GenuineMax    int           // max consecutive genuine attempts before soft note
}

// DefaultRetrySchedule: 2m, 4m, 8m ... capped at 1h then hourly; transient
// give-up at 3 days; genuine give-up after 3 consecutive genuine attempts.
var DefaultRetrySchedule = RetrySchedule{
	Base:          2 * time.Minute,
	Cap:           time.Hour,
	TransientWall: 72 * time.Hour,
	GenuineMax:    3,
}

// NextDelay returns the backoff before the next attempt given the 1-based count
// of attempts already made. The delay grows exponentially as Base*2^(n-1) but is
// clamped to Cap, so once the raw formula would exceed Cap, Cap is returned.
func (s RetrySchedule) NextDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := s.Base
	// Stop doubling once d reaches Cap. The `d < s.Cap` guard also bounds the
	// iteration count, preventing int64 time.Duration overflow for large attempt.
	for i := 1; i < attempt && d < s.Cap; i++ {
		d *= 2
	}
	if d > s.Cap {
		d = s.Cap
	}
	return d
}

// TransientExhausted reports whether transient retries have exceeded the wall
// clock since the first attempt. TransientWall is a threshold to exceed, so the
// comparison is strict `>`: exactly at TransientWall is not yet exhausted.
func (s RetrySchedule) TransientExhausted(sinceFirst time.Duration) bool {
	return sinceFirst > s.TransientWall
}

// GenuineExhausted reports whether the consecutive-genuine streak hit the cap.
// GenuineMax is an inclusive count of allowed attempts, so the comparison is
// `>=`: reaching GenuineMax exhausts the streak.
func (s RetrySchedule) GenuineExhausted(consecutiveGenuine int) bool {
	return consecutiveGenuine >= s.GenuineMax
}
