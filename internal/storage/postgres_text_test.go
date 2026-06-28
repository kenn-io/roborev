package storage

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizePostgresTextReplacesInvalidUTF8(t *testing.T) {
	invalid := "ReportLab PDF marker: " + string([]byte{0x93}) + " after header"
	require.False(t, utf8.ValidString(invalid))

	got := sanitizePostgresText(invalid)

	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, "ReportLab PDF marker: \uFFFD after header", got)
}

func TestSanitizePostgresTextReplacesNUL(t *testing.T) {
	input := "binary marker: \x00 after header"
	require.True(t, utf8.ValidString(input))

	got := sanitizePostgresText(input)

	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, "binary marker: \uFFFD after header", got)
	assert.NotContains(t, got, "\x00")
}

func TestSanitizePostgresTextPointer(t *testing.T) {
	invalid := "diff " + string([]byte{0x93})
	got := sanitizePostgresTextPointer(&invalid)

	require.NotNil(t, got)
	assert.True(t, utf8.ValidString(*got))
	assert.Equal(t, "diff \uFFFD", *got)
	assert.Nil(t, sanitizePostgresTextPointer(nil))
}
