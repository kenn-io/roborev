package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuietHoursResolve(t *testing.T) {
	tests := []struct {
		name         string
		cfg          QuietHoursConfig
		wantNil      bool
		wantErr      string
		wantInterval time.Duration
	}{
		{
			name:    "unset is disabled",
			cfg:     QuietHoursConfig{},
			wantNil: true,
		},
		{
			name:    "only start set is an error",
			cfg:     QuietHoursConfig{Start: "23:00"},
			wantErr: "start and end must both be set",
		},
		{
			name:    "only end set is an error",
			cfg:     QuietHoursConfig{End: "05:00"},
			wantErr: "start and end must both be set",
		},
		{
			name:    "start equals end is disabled",
			cfg:     QuietHoursConfig{Start: "23:00", End: "23:00"},
			wantNil: true,
		},
		{
			name:    "invalid start",
			cfg:     QuietHoursConfig{Start: "25:00", End: "05:00"},
			wantErr: "start: invalid clock time",
		},
		{
			name:    "non-time start",
			cfg:     QuietHoursConfig{Start: "bedtime", End: "05:00"},
			wantErr: "start: invalid clock time",
		},
		{
			name:    "invalid end",
			cfg:     QuietHoursConfig{Start: "23:00", End: "5pm"},
			wantErr: "end: invalid clock time",
		},
		{
			name: "invalid timezone",
			cfg: QuietHoursConfig{
				Start: "23:00", End: "05:00",
				Timezone: "US/Nowhere",
			},
			wantErr: "timezone:",
		},
		{
			name: "invalid throttle interval",
			cfg: QuietHoursConfig{
				Start: "23:00", End: "05:00",
				ThrottleInterval: "not-a-duration",
			},
			wantErr: "throttle_interval:",
		},
		{
			name: "negative throttle interval",
			cfg: QuietHoursConfig{
				Start: "23:00", End: "05:00",
				ThrottleInterval: "-1h",
			},
			wantErr: "throttle_interval: negative duration",
		},
		{
			name: "zero throttle interval is a valid no-op",
			cfg: QuietHoursConfig{
				Start: "23:00", End: "05:00",
				ThrottleInterval: "0",
			},
			wantInterval: 0,
		},
		{
			name:         "interval defaults to 1h",
			cfg:          QuietHoursConfig{Start: "23:00", End: "05:00"},
			wantInterval: time.Hour,
		},
		{
			name: "explicit interval",
			cfg: QuietHoursConfig{
				Start: "23:00", End: "05:00",
				Timezone: "US/Central", ThrottleInterval: "2h",
			},
			wantInterval: 2 * time.Hour,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := tt.cfg.Resolve()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, w)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, w)
				return
			}
			require.NotNil(t, w)
			assert.Equal(t, tt.wantInterval, w.Interval)
		})
	}
}

func TestQuietHoursResolveEmptyTimezoneIsLocal(t *testing.T) {
	// time.LoadLocation("") returns UTC; the resolver must special-case
	// empty to machine local time.
	w, err := (&QuietHoursConfig{Start: "23:00", End: "05:00"}).Resolve()
	require.NoError(t, err)
	require.NotNil(t, w)
	assert.Same(t, time.Local, w.loc)
}

func TestQuietHoursWindowActive(t *testing.T) {
	utc := func(hour, minute int) time.Time {
		return time.Date(2026, 7, 14, hour, minute, 0, 0, time.UTC)
	}
	resolve := func(t *testing.T, cfg QuietHoursConfig) *QuietHoursWindow {
		t.Helper()
		w, err := cfg.Resolve()
		require.NoError(t, err)
		require.NotNil(t, w)
		return w
	}

	t.Run("same-day window", func(t *testing.T) {
		assert := assert.New(t)
		w := resolve(t, QuietHoursConfig{
			Start: "09:00", End: "17:00", Timezone: "UTC",
		})
		assert.False(w.Active(utc(8, 59)), "before start")
		assert.True(w.Active(utc(9, 0)), "start is inclusive")
		assert.True(w.Active(utc(12, 0)), "mid-window")
		assert.True(w.Active(utc(16, 59)), "last minute")
		assert.False(w.Active(utc(17, 0)), "end is exclusive")
		assert.False(w.Active(utc(23, 0)), "after end")
	})

	t.Run("window wrapping midnight", func(t *testing.T) {
		assert := assert.New(t)
		w := resolve(t, QuietHoursConfig{
			Start: "23:00", End: "05:00", Timezone: "UTC",
		})
		assert.False(w.Active(utc(22, 59)), "before start")
		assert.True(w.Active(utc(23, 0)), "start is inclusive")
		assert.True(w.Active(utc(0, 0)), "midnight")
		assert.True(w.Active(utc(4, 59)), "last minute")
		assert.False(w.Active(utc(5, 0)), "end is exclusive")
		assert.False(w.Active(utc(12, 0)), "midday")
	})

	t.Run("timezone conversion", func(t *testing.T) {
		assert := assert.New(t)
		w := resolve(t, QuietHoursConfig{
			Start: "23:00", End: "05:00", Timezone: "US/Central",
		})
		// 2026-07-14 is CDT (UTC-5): 04:00Z = 23:00 previous day
		// CDT (active), 15:00Z = 10:00 CDT (inactive).
		assert.True(w.Active(utc(4, 0)), "23:00 CDT")
		assert.True(w.Active(utc(9, 59)), "04:59 CDT")
		assert.False(w.Active(utc(10, 0)), "05:00 CDT")
		assert.False(w.Active(utc(15, 0)), "10:00 CDT")
	})
}

func TestQuietHoursBypassUsers(t *testing.T) {
	q := QuietHoursConfig{BypassUsers: []string{"Trusted-User"}}
	assert.True(t, q.IsBypassed("trusted-user"))
	assert.True(t, q.IsBypassed("TRUSTED-USER"))
	assert.False(t, q.IsBypassed("other-user"))
}

func TestQuietHoursConfigTOMLParsing(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	err := os.WriteFile(configPath, []byte(`
[ci]
enabled = true

[ci.quiet_hours]
start = "23:00"
end = "05:00"
timezone = "US/Central"
throttle_interval = "90m"
bypass_users = ["trusted-user"]
`), 0o644)
	require.NoError(t, err)

	cfg, err := LoadGlobalFrom(configPath)
	require.NoError(t, err)
	q := cfg.CI.QuietHours
	assert.Equal(t, "23:00", q.Start)
	assert.Equal(t, "05:00", q.End)
	assert.Equal(t, "US/Central", q.Timezone)
	assert.Equal(t, "90m", q.ThrottleInterval)
	assert.Equal(t, []string{"trusted-user"}, q.BypassUsers)
}
