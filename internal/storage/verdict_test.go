package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	VerdictPass = "P"
	VerdictFail = "F"
)

type verdictTestCase struct {
	name   string
	output string
	want   string
}

func runVerdictTests(t *testing.T, tests []verdictTestCase) {
	t.Helper()
	assert := assert.New(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseVerdict(tt.output)
			assert.Equal(tt.want, got, "ParseVerdict() = %q, want %q", got, tt.want)
		})
	}
}

var verdictTests = []verdictTestCase{
	// --- SimplePass: basic "no issues found" phrasing ---
	{
		name:   "SimplePass/no issues found at start",
		output: "No issues found. This commit adds a new feature.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no issues found on own line",
		output: "Review complete.\n\nNo issues found.\n\nThe code looks good.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no issues found with leading whitespace",
		output: "  No issues found. Great work!",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no issues found lowercase",
		output: "no issues found. The code is clean.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no issues found mixed case",
		output: "NO ISSUES FOUND. Excellent!",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no issues with period",
		output: "No issues. The code is clean.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no issues standalone",
		output: "No issues",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no findings at start of line",
		output: "No findings to report.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/compact no verified findings remain",
		output: "No verified findings remain.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/compact no remaining findings",
		output: "No remaining findings.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/compact zero findings",
		output: "0 findings.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/compact zero findings remain",
		output: "0 findings remain.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/compact zero word findings",
		output: "zero findings.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/compact zero word findings remain",
		output: "zero findings remain.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/compact all previous findings addressed",
		output: "All previous findings have been addressed.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/compact all findings resolved",
		output: "All findings have been resolved.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/bullet no issues found",
		output: "- No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/asterisk bullet no issues",
		output: "* No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no issues with this commit",
		output: "No issues with this commit.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no issues in this change",
		output: "No issues in this change. Looks good.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/numbered list no issues",
		output: "1. No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/bullet with extra spaces",
		output: "-   No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/large numbered list",
		output: "100. No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no issues remain is pass",
		output: "No issues found. No issues remain.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no problems exist is pass",
		output: "No issues found. No problems exist.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/doesn't have issues is pass",
		output: "No issues found. The code doesn't have issues.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/doesn't have any problems is pass",
		output: "No issues found. Code doesn't have any problems.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/don't have vulnerabilities is pass",
		output: "No issues found. We don't have vulnerabilities.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no significant issues remain is pass",
		output: "No issues found. No significant issues remain.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no known issues exist is pass",
		output: "No issues found. No known issues exist.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/no open issues remain is pass",
		output: "No issues found. No open issues remain.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/found no critical issues with module is pass",
		output: "No issues found. Found no critical issues with the module.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/didn't find any major issues in code is pass",
		output: "No issues found. I didn't find any major issues in the code.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/not finding issues with is pass",
		output: "No issues found. Not finding issues with the code.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/did not see issues in module is pass",
		output: "No issues found. I did not see issues in the module.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/can't find issues with is pass",
		output: "No issues found. I can't find issues with the code.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/cannot find issues in is pass",
		output: "No issues found. Cannot find issues in the module.",
		want:   VerdictPass,
	},
	{
		name:   "SimplePass/couldn't find issues with is pass",
		output: "No issues found. I couldn't find issues with the implementation.",
		want:   VerdictPass,
	},

	// --- FieldLabels: pass phrases after structured field labels ---
	{
		name:   "FieldLabels/review findings label",
		output: "1. **Summary**: Adds features.\n2. **Review Findings**: No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "FieldLabels/findings label",
		output: "**Findings**: No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "FieldLabels/verdict label pass",
		output: "**Verdict**: No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "FieldLabels/verdict label explicit PASS value",
		output: "**Verdict**: PASS",
		want:   VerdictPass,
	},
	{
		name:   "FieldLabels/verdict label explicit FAIL value",
		output: "**Verdict**: FAIL",
		want:   VerdictFail,
	},
	{
		name:   "FieldLabels/verdict label no space after colon",
		output: "**Verdict**:No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "FieldLabels/review result label no space after colon",
		output: "**Review Result**:No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "FieldLabels/review findings label no space after colon",
		output: "2. **Review Findings**:No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "FieldLabels/findings label no space after colon",
		output: "**Findings**:No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "FieldLabels/result label no space after colon",
		output: "**Result**:No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "FieldLabels/review label no space after colon",
		output: "**Review**:No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "FieldLabels/verdict label tab after colon",
		output: "**Verdict**:\tNo issues found.",
		want:   VerdictPass,
	},

	// --- MarkdownFormatting: pass phrases wrapped in markdown ---
	{
		name:   "MarkdownFormatting/bold no issues found",
		output: "**No issues found.**",
		want:   VerdictPass,
	},
	{
		name:   "MarkdownFormatting/bold no issues in sentence",
		output: "**No issues found.** The code looks good.",
		want:   VerdictPass,
	},
	{
		name:   "MarkdownFormatting/markdown header no issues",
		output: "## No issues found",
		want:   VerdictPass,
	},
	{
		name:   "MarkdownFormatting/markdown h3 no issues",
		output: "### No issues found.",
		want:   VerdictPass,
	},
	{
		name:   "MarkdownFormatting/underscore bold no issues",
		output: "__No issues found.__",
		want:   VerdictPass,
	},

	// --- PhrasingVariations: alternate pass wordings and context ---
	{
		name:   "PhrasingVariations/no tests failed is pass",
		output: "No issues found; no tests failed.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/zero errors is pass",
		output: "No issues found, 0 errors.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/without bugs is pass",
		output: "No issues found, without bugs.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/avoid panics is pass",
		output: "No issues found. This commit hardens the code to avoid slicing panics.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/fix errors is pass",
		output: "No issues found. The changes fix potential errors in the parser.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/prevents crashes is pass",
		output: "No issues found. This update prevents crashes when input is nil.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/will avoid is pass",
		output: "No issues found. This will avoid panics.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/no tests have failed",
		output: "No issues found; no tests have failed.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/none of the tests failed",
		output: "No issues found. None of the tests failed.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/never fails",
		output: "No issues found. Build never fails.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/didn't fail contraction",
		output: "No issues found. Tests didn't fail.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/hasn't crashed",
		output: "No issues found. Code hasn't crashed.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/i didn't find any issues",
		output: "I didn't find any issues in this commit.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/i didnt find any issues curly apostrophe",
		output: "I didn\u2019t find any issues in this commit.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/i didn't find any issues with checked for",
		output: "I didn't find any issues. I checked for bugs, security issues, testing gaps, regressions, and code quality concerns.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/i didn't find any issues multiline",
		output: "I didn't find any issues. I checked for bugs, security issues, testing gaps, regressions, and code\nquality concerns.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/exact review 583 text",
		output: "I didn't find any issues. I checked for bugs, security issues, testing gaps, regressions, and code\nquality concerns.\nThe change updates selection revalidation during job refresh to respect visibility (repo filter and\n`hideAddressed`) in `cmd/roborev/tui.go`, and adds a focused set of `hideAddressed` tests (toggle,\nfiltering, selection movement, refresh, navigation, and repo filter interaction) in\n`cmd/roborev/tui_test.go`.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/i did not find any issues",
		output: "I did not find any issues with the code.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/i found no issues",
		output: "I found no issues.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/i found no issues in this commit",
		output: "I found no issues in this commit. The changes are well-structured.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/no issues with checked for context",
		output: "No issues found. I checked for bugs, security issues, testing gaps, regressions, and code quality concerns.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/no issues with looking for context",
		output: "No issues. I was looking for bugs and errors but found none.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/no issues with looked for context",
		output: "No issues found. I looked for crashes and panics.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/checked for and found no issues",
		output: "No issues found. I checked for bugs and found no issues.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/checked for and found no bugs",
		output: "No issues found. I checked for security issues and found no bugs.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/checked for and found nothing",
		output: "No issues found. I checked for errors and found nothing.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/checked for and found none",
		output: "No issues found. I checked for crashes and found none.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/checked for and found 0 issues",
		output: "No issues found. I checked for bugs and found 0 issues.",
		want:   VerdictPass,
	},
	{
		name:   "PhrasingVariations/checked for and found zero errors",
		output: "No issues found. I looked for problems and found zero errors.",
		want:   VerdictPass,
	},

	// --- BenignPhrases: negative-sounding words in benign context ---
	{
		name:   "BenignPhrases/benign problem statement",
		output: "No issues found. The problem statement is clear.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/benign issue tracker",
		output: "No issues found. Issue tracker updated.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/benign vulnerability disclosure",
		output: "No issues found. Vulnerability disclosure policy reviewed.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/benign problem domain",
		output: "No issues found. The problem domain is well understood.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/error handling in description is pass",
		output: "No issues found. The commit hardens the test setup with error handling around filesystem operations.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/error messages in description is pass",
		output: "No issues found. The code improves error messages for better debugging.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/multiple occurrences all positive is pass",
		output: "No issues found. Error handling added to auth. Error handling also improved in utils.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/partial word match problem domains is pass",
		output: "No issues found. The problem domains are well-defined.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/partial word match errorhandling is pass",
		output: "No issues found. The errorhandling module works well.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/problem domain vs problem domains mixed is pass",
		output: "No issues found. The problem domain is clear, and the problem domains are complex.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/found a way is benign",
		output: "No issues found. I checked for bugs and found a way to improve the docs.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/severity in prose not a finding",
		output: "No issues found. I rate this as Medium importance for the project.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/medium without separator not a finding",
		output: "No issues found. The medium was oil on canvas.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/severity legend not a finding",
		output: "No issues found.\n\nSeverity levels:\nHigh - immediate action required.\nMedium - should be addressed.\nLow - minor concern.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/priority scale not a finding",
		output: "No issues found.\n\nPriority scale:\nCritical: system down\nHigh: major feature broken",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/severity legend with descriptions not a finding",
		output: "No issues found.\n\nSeverity levels:\nHigh - immediate action required.\n  These issues block release.\nMedium - should be addressed.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/high-level overview not a finding",
		output: "No issues found. This is a high-level overview of the changes.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/low-level details not a finding",
		output: "No issues found. The commit adds low-level optimizations.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/severity label value in legend is not finding",
		output: "No issues found.\n\nSeverity levels:\nSeverity: High - immediate action required.\nSeverity: Low - minor concern.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/markdown legend header not a finding",
		output: "No issues found.\n\n**Severity levels:**\n- **High** - immediate action required.\n- **Medium** - should be addressed.\n- **Low** - minor concern.",
		want:   VerdictPass,
	},
	{
		name:   "BenignPhrases/markdown legend header with severity label not a finding",
		output: "No issues found.\n\n**Severity levels:**\nSeverity: High - immediate action required.\nSeverity: Low - minor concern.",
		want:   VerdictPass,
	},

	// These cases are intentionally PASS. Once an agent emits a clear pass phrase,
	// verdict parsing does not try to interpret the rest of the prose for caveats
	// or contradictory narrative. That output-shaping problem belongs in the review
	// prompt, not in a brittle natural-language verdict parser.
	{
		name:   "PassPhraseWins/historical broken path after pass is benign",
		output: "Review #8609 roborev (codex: gpt-5.4)\nVerdict: Fail\n\nNo issues found. The guard on cmd.Flags().Changed(\"sha\") matches the intended behavior, and the added test exercises the previously broken quiet-mode path.",
		want:   VerdictPass,
	},
	{
		name:   "PassPhraseWins/explicit fail label before later pass phrase is still pass",
		output: "**Verdict**: FAIL\n\nNo issues found.",
		want:   VerdictPass,
	},
	{
		name:   "PassPhraseWins/caveat prose after pass phrase is still pass",
		output: "No issues found, but consider refactoring.",
		want:   VerdictPass,
	},
	{
		name:   "PassPhraseWins/review findings label with caveat prose is still pass",
		output: "2. **Review Findings**: No issues found, but consider refactoring.",
		want:   VerdictPass,
	},
	{
		name:   "PassPhraseWins/process narration after explicit pass phrase is still pass",
		output: "No issues found. I checked for bugs, security issues, testing gaps, regressions, and code quality concerns.",
		want:   VerdictPass,
	},

	// Failures should come from clear structured findings or from the absence of a
	// clear pass phrase. We intentionally avoid sentence-level caveat parsing.
	{
		name:   "FailFallback/empty output",
		output: "",
		want:   VerdictFail,
	},
	{
		name:   "FailFallback/ambiguous language",
		output: "The commit looks mostly fine but could use some cleanup.",
		want:   VerdictFail,
	},
	{
		name:   "FailFallback/narrative front matter without final verdict defaults to fail",
		output: "Reviewing the diff in context first. I'm opening the touched storage parsing code and adjacent tests to check for regressions.",
		want:   VerdictFail,
	},
	{
		name:   "FailFallback/unstructured issue statement defaults to fail",
		output: "The code has issues.",
		want:   VerdictFail,
	},

	// --- StructuredFail: severity-labelled findings override pass phrases ---
	{
		name:   "StructuredFail/findings before no issues mention",
		output: "Medium - Security issue\nOtherwise no issues found.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label medium em dash",
		output: "**Findings**\n- Medium — Possible regression in deploy.\nNo issues found beyond the notes above.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label low with colon",
		output: "- Low: Minor style issue.\nOtherwise no issues.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label high with dash",
		output: "* High - Security vulnerability found.\nNo issues found.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label critical with bullet",
		output: "- Critical — Data loss possible.\nNo issues otherwise.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label critical without bullet",
		output: "Critical — Data loss possible.\nNo issues otherwise.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label high without bullet",
		output: "High: Security vulnerability in auth module.\nNo issues found.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label value format high",
		output: "- **Severity**: High\n- **Location**: file.go\n- **Problem**: Bug found.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label value format low",
		output: "- **Severity**: Low\n- **Problem**: Minor style issue.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label value with no issues in other file",
		output: "- **Severity**: High\n- **Problem**: Bug found.\n\n- **No issues found** in test_file.go.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label value plain text",
		output: "Severity: High\nLocation: file.go\nProblem: Bug found.",
		want:   VerdictFail,
	},
	{
		name:   "StructuredFail/severity label value hyphen separator",
		output: "Severity - High\nLocation: file.go\nProblem: Bug found.",
		want:   VerdictFail,
	},

	// --- SeverityThresholdMarker: SEVERITY_THRESHOLD_MET handling ---
	// The marker only signals pass when it appears as the entire
	// substantive output. A loose substring check would let prose
	// findings without severity labels flip to pass just because
	// the agent echoed the marker in narration.
	{
		name:   "ThresholdMarker/marker alone is pass",
		output: "SEVERITY_THRESHOLD_MET",
		want:   VerdictPass,
	},
	{
		name:   "ThresholdMarker/marker with surrounding whitespace is pass",
		output: "\n\n  SEVERITY_THRESHOLD_MET  \n\n",
		want:   VerdictPass,
	},
	{
		name:   "ThresholdMarker/marker with bold decoration is pass",
		output: "**SEVERITY_THRESHOLD_MET**",
		want:   VerdictPass,
	},
	{
		name:   "ThresholdMarker/marker with italic star is pass",
		output: "*SEVERITY_THRESHOLD_MET*",
		want:   VerdictPass,
	},
	{
		name:   "ThresholdMarker/marker with italic underscore is pass",
		output: "_SEVERITY_THRESHOLD_MET_",
		want:   VerdictPass,
	},
	{
		name:   "ThresholdMarker/marker with bold italic is pass",
		output: "***SEVERITY_THRESHOLD_MET***",
		want:   VerdictPass,
	},
	{
		name:   "ThresholdMarker/marker with trailing period is pass",
		output: "SEVERITY_THRESHOLD_MET.",
		want:   VerdictPass,
	},
	{
		name:   "ThresholdMarker/marker in code fence is pass",
		output: "```\nSEVERITY_THRESHOLD_MET\n```",
		want:   VerdictPass,
	},
	{
		name:   "ThresholdMarker/marker as bullet item is pass",
		output: "- SEVERITY_THRESHOLD_MET",
		want:   VerdictPass,
	},
	{
		name:   "ThresholdMarker/marker with chatty narration is fail",
		output: "All findings are below medium severity.\n\nSEVERITY_THRESHOLD_MET\n\nNo code changes needed.",
		want:   VerdictFail,
	},
	{
		name:   "ThresholdMarker/marker plus prose finding without severity label is fail",
		output: "SEVERITY_THRESHOLD_MET\n\nThe auth module leaks session tokens to logs.",
		want:   VerdictFail,
	},
	{
		name:   "ThresholdMarker/marker plus severity labels is fail",
		output: "- Medium — some issue\n\nSEVERITY_THRESHOLD_MET",
		want:   VerdictFail,
	},
	{
		name:   "ThresholdMarker/marker plus high severity is fail",
		output: "SEVERITY_THRESHOLD_MET\n\n- High: critical bug found",
		want:   VerdictFail,
	},
}

func TestParseVerdict(t *testing.T) {
	runVerdictTests(t, verdictTests)
}
