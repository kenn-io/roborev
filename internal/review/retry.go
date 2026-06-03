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
// of attempts already made. Exponential (Base*2^(n-1)) capped at Cap.
func (s RetrySchedule) NextDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := s.Base
	for i := 1; i < attempt && d < s.Cap; i++ {
		d *= 2
	}
	if d > s.Cap {
		d = s.Cap
	}
	return d
}

// TransientExhausted reports whether transient retries have exceeded the wall
// clock since the first attempt.
func (s RetrySchedule) TransientExhausted(sinceFirst time.Duration) bool {
	return sinceFirst > s.TransientWall
}

// GenuineExhausted reports whether the consecutive-genuine streak hit the cap.
func (s RetrySchedule) GenuineExhausted(consecutiveGenuine int) bool {
	return consecutiveGenuine >= s.GenuineMax
}
