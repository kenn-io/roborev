package storage

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
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

func TestSanitizePostgresStructuredOutput(t *testing.T) {
	input := append([]byte(`{"schema_version":2,"summary":"summary\u0000 `), 0xff)
	input = append(input, []byte(`","verdict":"pass","findings":[{"title":"finding\u0000","description":"clean"}],"legacy":{"markdown":"legacy\u0000"}}`)...)
	original := append([]byte(nil), input...)

	got, err := sanitizePostgresStructuredOutput(jsontext.Value(input))
	require.NoError(t, err)

	var document struct {
		SchemaVersion json.Number `json:"schema_version"`
		Summary       string      `json:"summary"`
		Findings      []struct {
			Title string `json:"title"`
		} `json:"findings"`
		Legacy struct {
			Markdown string `json:"markdown"`
		} `json:"legacy"`
	}
	decoder := json.NewDecoder(bytes.NewReader(got))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&document))
	assert := assert.New(t)
	require.Len(t, document.Findings, 1)
	assert.Equal("summary\uFFFD \uFFFD", document.Summary)
	assert.Equal(json.Number("2"), document.SchemaVersion)
	assert.Equal("finding\uFFFD", document.Findings[0].Title)
	assert.Equal("legacy\uFFFD", document.Legacy.Markdown)
	assert.Equal(original, input)

	_, err = sanitizePostgresStructuredOutput(jsontext.Value(`{"schema_version":2} {}`))
	assert.Error(err)
}
