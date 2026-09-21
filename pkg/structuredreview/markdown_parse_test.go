package structuredreview

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMarkdownRoundTripsRenderedDocuments(t *testing.T) {
	labels := []string{"codex", "claude-code (security)"}
	tests := map[string]struct {
		doc         Document
		minSeverity string
	}{
		"pass without findings": {doc: Document{
			SchemaVersion: 2, Summary: "The change renames a helper.", Verdict: VerdictPass, Findings: []Finding{},
		}},
		"version 1 without a verdict": {doc: Document{
			SchemaVersion: 1, Summary: "The change renames a helper.", Findings: []Finding{},
		}},
		"unable to review": {doc: Document{
			SchemaVersion: 2, Summary: "The diff could not be read.", Verdict: VerdictUnableToReview, Findings: []Finding{},
		}},
		"findings with and without a location": {doc: Document{
			SchemaVersion: 2, Summary: "The change adds a cache.\n\nIt also updates the docs.", Verdict: VerdictFail,
			Findings: []Finding{
				{Severity: "critical", Problem: "The cache is never invalidated.", Fix: "Invalidate on write.", Location: "cache/store.go:42"},
				{Severity: "low", Problem: "The comment is stale.", Fix: "Update the comment."},
			},
		}},
		"multi-line problem and fix": {doc: Document{
			SchemaVersion: 2, Summary: "The change adds retries.", Verdict: VerdictFail,
			Findings: []Finding{{
				Severity: "high", Location: "client/retry.go:10",
				Problem: "Retries ignore the context.\n\n- The loop never checks ctx.Done().\n- A canceled request keeps running.",
				Fix:     "Check the context before each attempt:\n\n```go\nif err := ctx.Err(); err != nil {\n\treturn err\n}\n```",
			}},
		}},
		"version 1 with findings": {doc: Document{
			SchemaVersion: 1, Summary: "The change adds a flag.",
			Findings: []Finding{{Severity: "medium", Problem: "The flag has no test.", Fix: "Add a test.", Location: "cmd/flag.go"}},
		}},
		"findings below the rendered threshold": {minSeverity: "high", doc: Document{
			SchemaVersion: 2, Summary: "The change adds a flag.", Verdict: VerdictPass,
			Findings: []Finding{{Severity: "low", Problem: "The flag has no test.", Fix: "Add a test."}},
		}},
		"synthesis with sources": {doc: Document{
			SchemaVersion: 2, Summary: "Two reviewers looked at the change.", Verdict: VerdictFail, SourceLabels: labels,
			Findings: []Finding{
				{Severity: "high", Problem: "The token is logged.", Fix: "Redact the token.", Location: "auth/login.go:7", Sources: []int{2, 1}},
				{Severity: "medium", Problem: "The error is dropped.", Fix: "Return the error.", Sources: []int{1}},
			},
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseMarkdown(tt.doc.Markdown(tt.minSeverity), ParseMarkdownOptions{SourceLabels: tt.doc.SourceLabels})
			require.NoError(t, err)
			assert.Equal(t, tt.doc, got)
		})
	}
}

func TestParseMarkdownReadsTheLegacyFindingsList(t *testing.T) {
	markdown := strings.Join([]string{
		"## Review Findings",
		"",
		"- **Severity**: High",
		"- **Location**: `store/save.go:88`",
		"- **Problem**: The write is not atomic.",
		"  A crash between the two steps leaves a partial file.",
		"- **Fix**: Write to a temporary file and rename it.",
		"",
		"  ```go",
		"  os.Rename(tmp, path)",
		"  ```",
		"---",
		"* **Severity:** medium",
		"* **Problem:** The error message omits the path.",
		"* **Fix:** Include the path.",
		"",
		"## Summary",
		"",
		"The change adds a save routine.",
	}, "\n")

	got, err := ParseMarkdown(markdown, ParseMarkdownOptions{})
	require.NoError(t, err)

	assert.Equal(t, Document{
		SchemaVersion: 1,
		Summary:       "The change adds a save routine.",
		Findings: []Finding{
			{
				Severity: "high", Location: "`store/save.go:88`",
				Problem: "The write is not atomic.\nA crash between the two steps leaves a partial file.",
				Fix:     "Write to a temporary file and rename it.\n\n```go\nos.Rename(tmp, path)\n```",
			},
			{Severity: "medium", Problem: "The error message omits the path.", Fix: "Include the path."},
		},
	}, got, "the agent stated no verdict, so the document is version 1 without one")
}

func TestParseMarkdownReadsNoIssuesReviews(t *testing.T) {
	tests := map[string]struct {
		markdown string
		summary  string
	}{
		"statement then labeled summary": {
			markdown: "No issues found.\n\nSummary: The change renames a helper.",
			summary:  "The change renames a helper.",
		},
		"summary then statement": {
			markdown: "The change renames a helper.\nIt touches two files.\n\nNo issues found.",
			summary:  "The change renames a helper.\nIt touches two files.",
		},
		"statement then unlabeled text": {
			markdown: "No issues found\n\nThe change renames a helper.",
			summary:  "The change renames a helper.",
		},
		"statement alone": {
			markdown: "No issues found.\n",
			summary:  "No issues found.",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseMarkdown(tt.markdown, ParseMarkdownOptions{})
			require.NoError(t, err)
			assert.Equal(t, Document{SchemaVersion: 1, Summary: tt.summary, Findings: []Finding{}}, got)
		})
	}
}

func TestParseMarkdownRefusesInsteadOfGuessing(t *testing.T) {
	finding := func(fields ...string) string {
		return "## Review Findings\n\n" + strings.Join(fields, "\n") + "\n\n## Summary\n\nThe change adds a save routine."
	}
	rendered := Document{
		SchemaVersion: 2, Summary: "Two reviewers looked at the change.", Verdict: VerdictFail,
		SourceLabels: []string{"codex", "codex"},
		Findings:     []Finding{{Severity: "high", Problem: "The token is logged.", Fix: "Redact the token.", Sources: []int{1}}},
	}.Markdown("")
	tests := map[string]struct {
		markdown string
		labels   []string
		reason   MarkdownRefusalReason
	}{
		"missing severity": {
			markdown: finding("- **Location**: store/save.go", "- **Problem**: The write is not atomic.", "- **Fix**: Rename a temporary file."),
			reason:   RefusalMissingSeverity,
		},
		"severity that is not one level": {
			markdown: finding("- **Severity**: High/Medium", "- **Problem**: The write is not atomic.", "- **Fix**: Rename a temporary file."),
			reason:   RefusalInvalidSeverity,
		},
		"missing problem": {
			markdown: finding("- **Severity**: High", "- **Fix**: Rename a temporary file."),
			reason:   RefusalMissingProblem,
		},
		"missing fix": {
			markdown: finding("- **Severity**: High", "- **Problem**: The write is not atomic."),
			reason:   RefusalMissingFix,
		},
		"empty fix": {
			markdown: finding("- **Severity**: High", "- **Problem**: The write is not atomic.", "- **Fix**:"),
			reason:   RefusalMissingFix,
		},
		"stray prose between findings": {
			markdown: finding("- **Severity**: High", "- **Problem**: The write is not atomic.", "- **Fix**: Rename a temporary file.",
				"", "I also noticed the tests are slow."),
			reason: RefusalUnrecognizedContent,
		},
		"a field stated twice": {
			markdown: finding("- **Severity**: High", "- **Problem**: The write is not atomic.", "- **Problem**: It also leaks a handle.", "- **Fix**: Rename a temporary file."),
			reason:   RefusalUnrecognizedContent,
		},
		"more structure inside the summary": {
			markdown: finding("- **Severity**: High", "- **Problem**: The write is not atomic.", "- **Fix**: Rename a temporary file.") +
				"\n\n## Other Concerns\n\nThe tests are slow.",
			reason: RefusalUnrecognizedContent,
		},
		"findings list without a summary": {
			markdown: "## Review Findings\n\n- **Severity**: High\n- **Problem**: The write is not atomic.\n- **Fix**: Rename a temporary file.",
			reason:   RefusalMissingSummary,
		},
		"free-form prose": {
			markdown: "The save routine looks risky because the write is not atomic. Consider a rename.",
			reason:   RefusalUnrecognizedFormat,
		},
		"sections roborev did not write": {
			markdown: "## Summary\n\nThe change adds a save routine.\n\n## Risks\n\nThe write is not atomic.\n\n## Verdict\n\nFail",
			reason:   RefusalUnrecognizedFormat,
		},
		"no issues statement in the middle of other sections": {
			markdown: "The change adds a save routine.\n\nNo issues found.\n\n## Risks\n\nThe write is not atomic.",
			reason:   RefusalUnrecognizedFormat,
		},
		"headings around a no issues statement": {
			markdown: "No issues found.\n\n## Risks\n\nThe write is not atomic.",
			reason:   RefusalUnrecognizedContent,
		},
		"rendered finding without a fix": {
			markdown: "## Summary\n\nThe change adds a save routine.\n\n## Findings\n\n### 1. High\n\n**Problem:** The write is not atomic.\n",
			reason:   RefusalMissingFix,
		},
		"reporter that is not an input review": {
			markdown: strings.Replace(rendered, "**Reported by:** codex", "**Reported by:** gemini", 1),
			labels:   []string{"codex", "claude-code"},
			reason:   RefusalUnrecoverableSources,
		},
		"reporter label shared by two input reviews": {
			markdown: rendered,
			labels:   []string{"codex", "codex"},
			reason:   RefusalUnrecoverableSources,
		},
		"empty text": {markdown: " \n", reason: RefusalUnrecognizedFormat},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseMarkdown(tt.markdown, ParseMarkdownOptions{SourceLabels: tt.labels})
			var refusal *MarkdownRefusal
			require.ErrorAs(t, err, &refusal)
			assert.Equal(t, tt.reason, refusal.Reason, refusal.Detail)
			assert.Equal(t, Document{}, got, "a refusal converts nothing")
		})
	}
}

func TestParseMarkdownRefusesFindingsHiddenInARenderedSummary(t *testing.T) {
	markdown := "## Summary\n\nThe change adds a save routine.\n\n## Findings\n\nThe write is not atomic.\n\nNo issues found."

	_, err := ParseMarkdown(markdown, ParseMarkdownOptions{})

	var refusal *MarkdownRefusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, RefusalUnrecognizedContent, refusal.Reason)
}
