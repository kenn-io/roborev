package structuredreview

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeReadsVersion3Findings(t *testing.T) {
	doc, err := Decode(jsontext.Value(`{
  "schema_version": 3,
  "summary": "Three findings.",
  "verdict": "fail",
  "findings": [
    {"severity":"High","kind":"Defect","topic":"Security","confidence":0.9,"problem":"p1","fix":"f1","file":" a.go ","start_line":12,"end_line":14},
    {"severity":"low","kind":"improvement","topic":null,"confidence":0,"problem":"p2","fix":"f2","file":"b.go","start_line":null,"end_line":null},
    {"severity":"low","kind":"improvement","confidence":1,"problem":"p3","fix":"f3"}
  ]
}`))
	require.NoError(t, err)
	require.Len(t, doc.Findings, 3)

	assert := assert.New(t)
	first := doc.Findings[0]
	assert.Equal("high", first.Severity)
	assert.Equal(KindDefect, first.Kind)
	assert.Equal(TopicSecurity, first.Topic)
	assert.Empty(doc.Findings[1].Topic, "a null topic reads as none")
	assert.Empty(doc.Findings[2].Topic, "a missing topic reads as none")
	assert.InDelta(0.9, first.Confidence, 1e-9)
	assert.Equal("a.go", first.File)
	assert.Equal(12, first.StartLine)
	assert.Equal(14, first.EndLine)
	assert.Equal("a.go:12-14", first.Location)
	assert.Equal("b.go", doc.Findings[1].Location, "a file without lines covers the whole file")
	assert.Empty(doc.Findings[2].Location, "missing file fields read as null")
}

func TestDecodeNormalizesVersion3LineRanges(t *testing.T) {
	tests := []struct {
		name string
		file string
		want string
	}{
		{name: "single line", file: `"file":"a.go","start_line":7,"end_line":7`, want: "a.go:7"},
		{name: "end before start", file: `"file":"a.go","start_line":9,"end_line":4`, want: "a.go:9"},
		{name: "end line only", file: `"file":"a.go","start_line":null,"end_line":5`, want: "a.go:5"},
		{name: "lines without a file", file: `"file":null,"start_line":3,"end_line":4`, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := Decode(jsontext.Value(`{"schema_version":3,"summary":"s","verdict":"fail","findings":[` +
				`{"severity":"low","kind":"defect","confidence":0.5,"problem":"p","fix":"f",` + tc.file + `}]}`))
			require.NoError(t, err)
			assert.Equal(t, tc.want, doc.Findings[0].Location)
		})
	}
}

func TestDecodeRejectsInvalidVersion3Findings(t *testing.T) {
	tests := []struct {
		name    string
		finding string
		want    string
	}{
		{name: "no kind", finding: `"confidence":0.5`, want: "has no kind"},
		{name: "unknown kind", finding: `"kind":"nit","confidence":0.5`, want: `invalid kind "nit"`},
		{name: "unknown topic", finding: `"kind":"defect","topic":"style","confidence":0.5`, want: `invalid topic "style"`},
		{name: "no confidence", finding: `"kind":"defect"`, want: "has no confidence"},
		{name: "confidence above 1", finding: `"kind":"defect","confidence":1.5`, want: "outside 0 to 1"},
		{name: "negative line", finding: `"kind":"defect","confidence":0.5,"file":"a.go","start_line":-1`, want: "negative line"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(jsontext.Value(`{"schema_version":3,"summary":"s","verdict":"fail","findings":[` +
				`{"severity":"low","problem":"p","fix":"f",` + tc.finding + `}]}`))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestDecodeRejectsVersion3FieldsInOlderDocuments(t *testing.T) {
	_, err := Decode(jsontext.Value(`{"schema_version":2,"summary":"s","verdict":"fail","findings":[` +
		`{"severity":"low","kind":"defect","problem":"p","fix":"f","location":"a.go:1"}]}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "need schema_version 3")
}

func TestEncodedDocumentsDecodeToTheSameDocument(t *testing.T) {
	located := Finding{Severity: "high", Kind: KindDefect, Confidence: 0.75, Problem: "p", Fix: "f", Sources: []int{2}}
	located.SetLocation("a.go", 3, 8)
	unlocated := Finding{Severity: "low", Kind: KindImprovement, Topic: TopicTests, Confidence: 0, Problem: "p2", Fix: "f2"}
	tests := []struct {
		name string
		doc  Document
	}{
		{name: "version 3", doc: Document{SchemaVersion: 3, Summary: "s", Verdict: VerdictFail, Findings: []Finding{located, unlocated}}},
		{name: "version 2", doc: Document{SchemaVersion: 2, Summary: "s", Verdict: VerdictFail, Findings: []Finding{{Severity: "low", Problem: "p", Fix: "f", Location: "a.go:3"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.doc)
			require.NoError(t, err)
			got, err := Decode(raw)
			require.NoError(t, err)
			assert.Equal(t, tc.doc, got)
		})
	}
}

func TestVersion3FindingsKeepALocationForOlderReaders(t *testing.T) {
	finding := Finding{Severity: "high", Kind: KindDefect, Confidence: 0.5, Problem: "p", Fix: "f"}
	finding.SetLocation("a.go", 3, 8)
	raw, err := json.Marshal(Document{SchemaVersion: 3, Summary: "s", Verdict: VerdictFail, Findings: []Finding{finding}})
	require.NoError(t, err)

	var reader struct {
		Findings []struct {
			Location *string `json:"location"`
		} `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(raw, &reader, json.RejectUnknownMembers(false)))
	require.Len(t, reader.Findings, 1)
	require.NotNil(t, reader.Findings[0].Location)
	assert.Equal(t, "a.go:3-8", *reader.Findings[0].Location)
}

func TestMarkdownLeavesKindTopicAndConfidenceOut(t *testing.T) {
	defect := Finding{Severity: "high", Kind: KindDefect, Topic: TopicSecurity, Confidence: 0.9, Problem: "p1", Fix: "f1"}
	defect.SetLocation("a.go", 4, 4)
	improvement := Finding{Severity: "low", Kind: KindImprovement, Topic: TopicTests, Confidence: 0.4, Problem: "p2", Fix: "f2"}
	got := Document{SchemaVersion: 3, Summary: "s", Verdict: VerdictFail, Findings: []Finding{defect, improvement}}.Markdown("")

	assert := assert.New(t)
	assert.Contains(got, "### 1. High\n\n**Location:** a.go:4\n")
	assert.Contains(got, "### 2. Low\n")
	for _, word := range []string{"security", "tests", "defect", "improvement", "0.9", "0.4"} {
		assert.NotContains(got, word, "rendered reviews are posted to pull requests and must not carry kind, topic, or confidence")
	}
}
