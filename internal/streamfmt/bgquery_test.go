package streamfmt

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/glamour/v2/styles"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryOSCBackground(t *testing.T) {
	for _, tt := range []struct {
		name    string
		pending string
		reply   string
		delay   time.Duration
		dark    bool
		ok      bool
		left    string
	}{
		{"light BEL", "", "\x1b]11;rgb:ffff/ffff/ffff\aq", 0, false, true, "q"},
		{"dark ST", "", "\x1b]11;rgb:0000/0000/0000\x1b\\q", 0, true, true, "q"},
		{"short RGB", "", "\x1b]11;rgb:f/f/f\a", 0, false, true, ""},
		{"delayed reply", "", "\x1b]11;rgb:ffff/ffff/ffff\x1b\\", time.Second, false, true, ""},
		{"queued key", "q", "", 0, false, false, "q"},
		{"queued event", "\x00", "", 0, false, false, "\x00"},
		{"key during query", "", "q", 0, false, false, "q"},
		{"interrupted reply", "", "\x1b]11;rgb:ffqf/ffff/ffff\a", 0, false, false, "\x1b]11;rgb:ffqf/ffff/ffff\a"},
		{"partial reply", "", "\x1b]11;rgb:ffff/", 0, false, false, "\x1b]11;rgb:ffff/"},
		{"no reply", "", "", 0, false, false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var out bytes.Buffer
				input := tt.pending
				start := time.Now()
				replied := false
				dark, ok := queryOSCBackground(&out, func() (string, error) {
					if out.Len() > 0 && !replied && time.Since(start) >= tt.delay {
						input += tt.reply
						replied = true
					}
					return input, nil
				}, func(n int) error {
					input = input[n:]
					return nil
				}, 2*time.Second)
				assert := assert.New(t)
				assert.Equal(tt.dark, dark)
				assert.Equal(tt.ok, ok)
				assert.Equal(tt.left, input)
				if tt.pending == "" && tt.reply == "" {
					assert.Equal(2*time.Second, time.Since(start))
				}
				if tt.pending != "" {
					assert.Empty(out.String(), "do not send a query with queued input")
				} else {
					assert.Equal("\x1b]11;?\x1b\\", out.String())
				}
			})
		})
	}
}

func TestQueryOSCBackgroundSplitTerminator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		start := time.Now()
		reply := "\x1b]11;rgb:ffff/ffff/ffff\x1b\\"
		dark, ok := queryOSCBackground(&out, func() (string, error) {
			if out.Len() == 0 {
				return "", nil
			}
			return reply[:min(int(time.Since(start)/time.Millisecond), len(reply))], nil
		}, func(n int) error {
			require.Equal(t, len(reply), n)
			return nil
		}, time.Second)
		assert.True(t, ok)
		assert.False(t, dark)
	})
}

func TestQueryOSCBackgroundDeadlineDuringPeek(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		consumed := false
		dark, ok := queryOSCBackground(&out, func() (string, error) {
			if out.Len() == 0 {
				return "", nil
			}
			time.Sleep(time.Second)
			return "\x1b]11;rgb:ffff/ffff/ffff\a", nil
		}, func(int) error {
			consumed = true
			return nil
		}, time.Second)
		assert.False(t, ok)
		assert.False(t, dark)
		assert.False(t, consumed)
	})
}

func TestInitialGlamourStyleRespectsColorMode(t *testing.T) {
	t.Setenv("COLORFGBG", "")
	for _, mode := range []string{"auto", "dark", "light", "none"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("ROBOREV_COLOR_MODE", mode)
			t.Setenv("NO_COLOR", "")
			style := InitialGlamourStyle()
			want := styles.DarkStyleConfig.Document.Color
			if mode == "light" {
				want = styles.LightStyleConfig.Document.Color
			}
			assert.Equal(t, want, style.Document.Color)
		})
	}
}
