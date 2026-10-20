package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertWritesJudgingInput(t *testing.T) {
	pr := pullRequest{Repo: "https://github.com/o/r", PRNumber: 7, Base: "b", Head: "h"}
	changed := changedLines([]byte("diff --git a/pkg/a.go b/pkg/a.go\n--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -10,2 +20,4 @@ func f\n"))
	document := []byte(`{"schema_version":3,"findings":[
  {"problem":"Nil map write.","fix":"Make the map.","file":"pkg/a.go","start_line":21,"end_line":22},
  {"problem":"Whole file.","fix":"","file":"pkg/a.go","start_line":null,"end_line":null},
  {"problem":"Nowhere.","fix":"f","file":null,"start_line":null,"end_line":null},
  {"problem":"Old range.","fix":"f","location":"` + "`b/pkg/b.go:3-5`" + `"},
  {"problem":"Old line.","fix":"f","location":"./pkg/c.go:9 (in init)"},
  {"problem":"Old prose.","fix":"f","location":"the retry loop"}
]}`)

	got, dropped, err := convert(pr, "roborev", document, changed, 1500*time.Millisecond)
	require.NoError(t, err)

	assert := assert.New(t)
	assert.Equal(2, dropped, "findings without a file cannot be matched")
	assert.Equal(findingsPR{Repo: "https://github.com/o/r", PRNumber: 7, Base: "b", Head: "h"}, got.PR)
	assert.Equal(int64(1500), got.Usage.TimeInMS)
	assert.Equal([]finding{
		{Producer: "roborev", File: "pkg/a.go", StartLine: 21, EndLine: 22, Message: "Nil map write.\n\nFix: Make the map."},
		{Producer: "roborev", File: "pkg/a.go", StartLine: 20, EndLine: 20, Message: "Whole file."},
		{Producer: "roborev", File: "pkg/b.go", StartLine: 3, EndLine: 5, Message: "Old range.\n\nFix: f"},
		{Producer: "roborev", File: "pkg/c.go", StartLine: 9, EndLine: 9, Message: "Old line.\n\nFix: f"},
	}, got.Findings)
}

func TestConvertWritesAnEmptyListForACleanReview(t *testing.T) {
	got, dropped, err := convert(pullRequest{}, "roborev", []byte(`{"findings":[]}`), nil, 0)
	require.NoError(t, err)
	assert.Zero(t, dropped)
	assert.NotNil(t, got.Findings, "an empty list, not null, tells the judge the review found nothing")
}
