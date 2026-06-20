package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseResetDuration(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want time.Duration
	}{
		{"none", "agent failed", 0},
		{"reset after seconds clamped to min", "quota will reset after 5s.", 1 * time.Minute},
		{"reset after minutes", "quota will reset after 48m20s.", 48*time.Minute + 20*time.Second},
		{"reset after hours", "reset after 2h13m.", 2*time.Hour + 13*time.Minute},
		{"reset after with trailing punct", "reset after 1h.. retrying", 1 * time.Hour},
		{"reset after invalid", "reset after notaduration", 0},
		{"reset after negative", "reset after -5m", 0},
		{"reset after huge clamped to max", "reset after 100h", 24 * time.Hour},
		{"case insensitive", "RESET AFTER 30m", 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ParseResetDuration(tc.msg))
		})
	}
}

func TestParseResetTime(t *testing.T) {
	// Use a fixed "now" so same-day vs next-day rollover is deterministic.
	loc := time.FixedZone("test", 0)
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, loc) // noon UTC
	cases := []struct {
		name    string
		msg     string
		wantErr bool
		want    time.Time
	}{
		{"none", "agent failed", true, time.Time{}},
		{
			name: "resets at later today",
			msg:  "limit resets at 5:42 PM",
			want: time.Date(2026, 5, 5, 17, 42, 0, 0, loc),
		},
		{
			name: "try again at later today 24h",
			msg:  "try again at 17:42",
			want: time.Date(2026, 5, 5, 17, 42, 0, 0, loc),
		},
		{
			name: "resets without at and utc suffix",
			msg:  "You've hit your session limit · resets 5:50am (UTC)",
			want: time.Date(2026, 5, 6, 5, 50, 0, 0, loc),
		},
		{
			name: "resets at earlier today rolls to next day",
			msg:  "limit resets at 9:00 AM",
			want: time.Date(2026, 5, 6, 9, 0, 0, 0, loc),
		},
		{
			name: "case insensitive",
			msg:  "LIMIT RESETS AT 6:00 pm",
			want: time.Date(2026, 5, 5, 18, 0, 0, 0, loc),
		},
		{"unparseable token", "resets at moonrise", true, time.Time{}},
		{
			// Trailing content after the time defeats time.Parse and
			// produces zero. Documenting current conservative behavior
			// — extending the parser to handle "today" / "UTC" suffixes
			// is deferred until a real Claude message is captured.
			name:    "trailing word causes miss",
			msg:     "limit resets at 5:42 PM today",
			wantErr: true,
			want:    time.Time{},
		},
		{
			// 24h with a timezone abbreviation: same conservative miss.
			name:    "24h with trailing zone causes miss",
			msg:     "resets at 17:42 UTC",
			wantErr: true,
			want:    time.Time{},
		},
		{
			// Same instant as now: should roll forward, not return now.
			name: "exactly now rolls to next day",
			msg:  "limit resets at 12:00 PM",
			want: time.Date(2026, 5, 6, 12, 0, 0, 0, loc),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseResetTimeAt(tc.msg, now)
			if tc.wantErr {
				assert.True(t, got.IsZero(), "expected zero time, got %v", got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseResetTimeRespectsLocation(t *testing.T) {
	// Non-UTC fixture: confirms now.Location() is honored end-to-end.
	est := time.FixedZone("EST", -5*60*60)
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, est) // noon EST

	got := parseResetTimeAt("limit resets at 5:42 PM", now)
	want := time.Date(2026, 5, 5, 17, 42, 0, 0, est)
	assert.Equal(t, want, got)
	assert.Equal(t, est, got.Location())
}

// TestParseResetDurationToLowerByteSafety guards the ToLower
// byte-index pitfall: U+0130 "İ" lowercases to "i" — a 2→1 byte
// change. Slicing the original errMsg with indices computed against
// strings.ToLower(errMsg) would extract the wrong bytes (or panic
// inside a multi-byte sequence). Slicing the lowercased copy is
// safe and parses cleanly.
func TestParseResetDurationToLowerByteSafety(t *testing.T) {
	got := ParseResetDuration("İ reset after 30m")
	assert.Equal(t, 30*time.Minute, got)
}

func TestParseResetTimeToLowerByteSafety(t *testing.T) {
	loc := time.FixedZone("test", 0)
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, loc) // noon
	got := parseResetTimeAt("İ resets at 5:42 PM", now)
	want := time.Date(2026, 5, 5, 17, 42, 0, 0, loc)
	assert.Equal(t, want, got)
}

// TestParseResetTimeAcrossDST exercises the rollover code path through
// real DST transitions where Add(24*time.Hour) would land an hour off.
// time.FixedZone has no DST rules, so it cannot exercise this branch —
// only a real IANA zone with a transition does.
func TestParseResetTimeAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("America/New_York tzdata unavailable: %v", err)
	}

	cases := []struct {
		name string
		now  time.Time
		msg  string
		want time.Time
	}{
		{
			// Spring forward: 2026-03-08 02:00 EST does not exist; the
			// clock jumps to 03:00 EDT. A reset advertised at 9:00 AM on
			// the morning of the transition, evaluated when "now" is the
			// previous evening, must land at 9:00 AM EDT — same wall-clock
			// time, despite the day having only 23 hours.
			name: "spring forward rollover preserves wall-clock",
			now:  time.Date(2026, 3, 7, 21, 0, 0, 0, ny), // 9pm EST
			msg:  "limit resets at 9:00 AM",
			want: time.Date(2026, 3, 8, 9, 0, 0, 0, ny), // 9am EDT next day
		},
		{
			// Fall back: 2026-11-01 has 25 hours. A reset advertised at
			// 9:00 AM, evaluated the prior evening, must land at 9:00 AM
			// EST — same wall-clock time, longer day notwithstanding.
			name: "fall back rollover preserves wall-clock",
			now:  time.Date(2026, 10, 31, 22, 0, 0, 0, ny), // 10pm EDT
			msg:  "limit resets at 9:00 AM",
			want: time.Date(2026, 11, 1, 9, 0, 0, 0, ny), // 9am EST next day
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseResetTimeAt(tc.msg, tc.now)
			assert.Equal(t, tc.want, got,
				"DST-safe rollover should preserve wall-clock time")
		})
	}
}
