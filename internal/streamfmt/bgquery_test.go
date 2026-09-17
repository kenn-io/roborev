package streamfmt

import (
	"bytes"
	"io"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadOSCBackground(t *testing.T) {
	const belReply = "\x1b]11;rgb:ffff/ffff/ffff\a"
	const stReply = "\x1b]11;rgb:ffff/ffff/ffff\x1b\\"
	for _, tt := range []struct {
		name, input, reply string
	}{
		{"BEL", belReply + "q", belReply},
		{"ST", stReply + "q", stReply},
		{"short channels", "\x1b]11;rgb:f/AB/cde\a", "\x1b]11;rgb:f/AB/cde\a"},
		{"key before reply", "q" + belReply, ""},
		{"arrow key before reply", "\x1b[A" + belReply, ""},
		{"non-text event", "\x00" + belReply, ""},
		{"event interrupts reply", "\x1b]11;rgb:ffff/\x00ffff/ffff\a", ""},
		{"key interrupts reply", "\x1b]11;rgb:ffff/q", ""},
		{"different OSC", "\x1b]10;rgb:ffff/ffff/ffff\a", ""},
		{"empty channel", "\x1b]11;rgb:f//f\a", ""},
		{"oversized channel", "\x1b]11;rgb:fffff/f/f\a", ""},
		{"incomplete color", "\x1b]11;rgb:f/f\a", ""},
		{"invalid ST", "\x1b]11;rgb:f/f/f\x1b[A", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := bytes.NewBufferString(tt.input)
			var offset int
			reply, err := readOSCBackground(func(time.Duration) (rune, error) {
				if offset == input.Len() {
					return 0, io.EOF
				}
				c := input.Bytes()[offset]
				offset++
				return rune(c), nil
			}, func() error {
				input.Next(offset)
				return nil
			})
			if tt.reply == "" {
				require.ErrorIs(t, err, errNotOSCBackground)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.reply, reply)
			assert.Equal(t, tt.input[len(tt.reply):], input.String())
		})
	}
}

func TestReadOSCBackgroundTimeout(t *testing.T) {
	for _, prefix := range []string{"", "\x1b]11;rgb:ffff/ffff/ffff\x1b"} {
		t.Run(prefix, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				input := bytes.NewBufferString(prefix)
				var offset int
				start := time.Now()
				_, err := readOSCBackground(func(remaining time.Duration) (rune, error) {
					if offset < input.Len() {
						c := input.Bytes()[offset]
						offset++
						time.Sleep(time.Millisecond)
						return rune(c), nil
					}
					time.Sleep(remaining)
					return 0, os.ErrDeadlineExceeded
				}, func() error {
					input.Next(offset)
					return nil
				})
				require.ErrorIs(t, err, os.ErrDeadlineExceeded)
				assert.Equal(t, windowsBackgroundQueryTimeout, time.Since(start))
				assert.Equal(t, prefix, input.String())
			})
		})
	}
}

func TestReadOSCBackgroundLateReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const reply = "\x1b]11;rgb:f/f/f\a"
		input := bytes.NewBufferString(reply)
		var offset int
		_, err := readOSCBackground(func(remaining time.Duration) (rune, error) {
			c := input.Bytes()[offset]
			offset++
			if c == '\a' {
				time.Sleep(remaining)
			}
			return rune(c), nil
		}, func() error {
			input.Next(offset)
			return nil
		})
		require.ErrorIs(t, err, os.ErrDeadlineExceeded)
		assert.Equal(t, reply, input.String())
	})
}
