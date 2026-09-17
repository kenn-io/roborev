package streamfmt

import (
	"io"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadOSCBackground(t *testing.T) {
	for _, terminator := range []string{"\a", "\x1b\\"} {
		t.Run(terminator, func(t *testing.T) {
			want := "\x1b]11;rgb:ffff/ffff/ffff" + terminator
			input := strings.NewReader(want + "q")
			reply, err := readOSCBackground(func(time.Duration) (rune, error) {
				c, _, err := input.ReadRune()
				return c, err
			})
			require.NoError(t, err)
			assert.Equal(t, want, reply)
			remaining, err := io.ReadAll(input)
			require.NoError(t, err)
			assert.Equal(t, "q", string(remaining))
		})
	}
}

func TestReadOSCBackgroundTimeout(t *testing.T) {
	for _, prefix := range []string{"", "\x1b]11;rgb:ffff/ffff/ffff\x1b"} {
		t.Run(prefix, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				input := strings.NewReader(prefix)
				start := time.Now()
				_, err := readOSCBackground(func(remaining time.Duration) (rune, error) {
					c, _, err := input.ReadRune()
					if err == nil {
						time.Sleep(time.Millisecond)
						return c, nil
					}
					time.Sleep(remaining)
					return 0, os.ErrDeadlineExceeded
				})
				require.ErrorIs(t, err, os.ErrDeadlineExceeded)
				assert.Equal(t, windowsBackgroundQueryTimeout, time.Since(start))
			})
		})
	}
}

func TestReadOSCBackgroundNonTextEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		_, err := readOSCBackground(func(time.Duration) (rune, error) {
			time.Sleep(time.Millisecond)
			return 0, nil
		})
		require.ErrorIs(t, err, os.ErrDeadlineExceeded)
		assert.Equal(t, windowsBackgroundQueryTimeout, time.Since(start))
	})
}
