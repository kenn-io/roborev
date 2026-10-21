package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExpandInstructions(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		lines int
		want  string
	}{
		{"rate", "{{changed_lines}} lines, about {{expected_findings:3}} findings", 300, "300 lines, about 9 findings"},
		{"fractional rate rounds", "{{expected_findings:1.5}}", 100, "2"},
		{"at least one", "{{expected_findings:3}}", 10, "1"},
		{"capped", "{{expected_findings:6}}", 13575, "40"},
		{"no placeholders", "plain text", 500, "plain text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, expandInstructions(tt.text, tt.lines))
		})
	}
}

func TestCountChangedLines(t *testing.T) {
	diff := `diff --git a/x.sql b/x.sql
index 1111111..2222222 100644
--- a/x.sql
+++ b/x.sql
@@ -1,3 +1,3 @@
 select 1;
--- old comment
+-- new comment
+select 2;
diff --git a/y.go b/y.go
new file mode 100644
--- /dev/null
+++ b/y.go
@@ -0,0 +1 @@
+package y
`
	assert.Equal(t, 4, countChangedLines(diff))
}
