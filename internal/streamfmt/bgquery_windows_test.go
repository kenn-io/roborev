//go:build windows

package streamfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseOSCBackgroundIsDark(t *testing.T) {
	assert := assert.New(t)

	darkReply := "\x1b]11;rgb:1e1e/1e1e/1e1e\a"
	isDark, ok := parseOSCBackgroundIsDark(darkReply)
	assert.True(ok)
	assert.True(isDark)

	lightReply := "\x1b]11;rgb:fdf6/e3e3/cece\x1b\\"
	isDark, ok = parseOSCBackgroundIsDark(lightReply)
	assert.True(ok)
	assert.False(isDark)

	shortChannelReply := "\x1b]11;rgb:00/00/00\a"
	isDark, ok = parseOSCBackgroundIsDark(shortChannelReply)
	assert.True(ok)
	assert.True(isDark)

	isDark, ok = parseOSCBackgroundIsDark("not an OSC reply")
	assert.False(ok)
	assert.False(isDark)
}

func TestPlatformHasDarkBackgroundNoWindowsTerminal(t *testing.T) {
	assert := assert.New(t)
	t.Setenv("WT_SESSION", "")

	isDark, ok := platformHasDarkBackground()

	assert.False(ok)
	assert.False(isDark)
}
