package review

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTrimPartialRune(t *testing.T) {
	assert := assert.New(t)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"ascii only", "hello", "hello"},
		{
			"clean emoji boundary",
			"abc😀",
			"abc😀",
		},
		{
			"split 4-byte emoji after 1 byte",
			"abc" + string([]byte{0xF0}),
			"abc",
		},
		{
			"split 4-byte emoji after 2 bytes",
			"abc" + string([]byte{0xF0, 0x9F}),
			"abc",
		},
		{
			"split 4-byte emoji after 3 bytes",
			"abc" + string([]byte{0xF0, 0x9F, 0x98}),
			"abc",
		},
		{
			"split 2-byte char after 1 byte",
			"abc" + string([]byte{0xC3}),
			"abc",
		},
		{
			"interior invalid bytes preserved",
			"a" + string([]byte{0xFF}) + "b",
			"a" + string([]byte{0xFF}) + "b",
		},
		{
			"orphan continuation byte",
			"abc" + string([]byte{0x80}),
			"abc",
		},
		{
			"two orphan continuation bytes",
			"abc" + string([]byte{0x80, 0x80}),
			"abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimPartialRune(tt.in)
			assert.Equalf(tt.want, got, "TrimPartialRune(%q) = %q, want %q", tt.in, got, tt.want)
		})
	}
}

func TestTrimPartialRune_NoFullStringScan(t *testing.T) {
	assert := assert.New(t)

	// Verify that a string with interior invalid UTF-8 is NOT
	// stripped down to empty — only the trailing boundary matters.
	// This is the bug that utf8.ValidString would cause.
	interior := strings.Repeat("x", 1000) +
		string([]byte{0xFF}) +
		strings.Repeat("y", 1000)
	got := TrimPartialRune(interior)
	assert.Equalf(interior, got, "interior invalid bytes should be preserved, got len %d want len %d", len(got), len(interior))
}
