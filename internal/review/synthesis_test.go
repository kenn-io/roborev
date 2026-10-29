package review

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
)

func TestMain(m *testing.M) {
	// Re-invoked as a fake `kata` binary by tests that put a copy of the
	// test executable named kata on PATH (see
	// TestRunBatch_NoKataContextFromUntrustedCheckout).
	if os.Getenv("ROBOREV_TEST_FAKE_KATA") == "1" {
		fmt.Println(`{"issues":[{"short_id":"abc4","title":"Leaked task","body":"Secret kata body."}]}`)
		os.Exit(0)
	}
	os.Exit(testenv.RunIsolatedMain(m))
}

func assertContainsAll(t *testing.T, got string, wants []string) {
	t.Helper()
	for _, want := range wants {
		assert.Contains(t, got, want, "output missing expected substring")
	}
}

func TestIsQuotaFailure(t *testing.T) {
	tests := []struct {
		name string
		r    ReviewResult
		want bool
	}{
		{
			name: "quota failure",
			r: ReviewResult{
				Status: "failed",
				Error:  QuotaErrorPrefix + "exhausted",
			},
			want: true,
		},
		{
			name: "real failure",
			r: ReviewResult{
				Status: "failed",
				Error:  "agent crashed",
			},
			want: false,
		},
		{
			name: "success",
			r:    ReviewResult{Status: "done"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsQuotaFailure(tt.r)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCountQuotaFailures(t *testing.T) {
	reviews := []ReviewResult{
		{Status: "done"},
		{
			Status: "failed",
			Error:  QuotaErrorPrefix + "exhausted",
		},
		{Status: "failed", Error: "real error"},
		{
			Status: "failed",
			Error:  QuotaErrorPrefix + "limit reached",
		},
	}
	assert.Equal(t, 2, CountQuotaFailures(reviews))
}

func TestIsTimeoutCancellation(t *testing.T) {
	tests := []struct {
		name string
		r    ReviewResult
		want bool
	}{
		{
			name: "timeout canceled",
			r:    ReviewResult{Status: "canceled", Error: TimeoutErrorPrefix + "posted early"},
			want: true,
		},
		{
			name: "regular canceled",
			r:    ReviewResult{Status: "canceled", Error: "user canceled"},
			want: false,
		},
		{
			name: "failed with timeout prefix",
			r:    ReviewResult{Status: ResultFailed, Error: TimeoutErrorPrefix + "posted early"},
			want: false,
		},
		{
			name: "done",
			r:    ReviewResult{Status: ResultDone},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsTimeoutCancellation(tt.r))
		})
	}
}

func TestCountTimeoutCancellations(t *testing.T) {
	reviews := []ReviewResult{
		{Status: "canceled", Error: TimeoutErrorPrefix + "posted early"},
		{Status: ResultDone, Output: "ok"},
		{Status: "canceled", Error: "user canceled"},
		{Status: "canceled", Error: TimeoutErrorPrefix + "batch expired"},
	}
	assert.Equal(t, 2, CountTimeoutCancellations(reviews))
}

func TestBuildSynthesisPrompt_Basic(t *testing.T) {
	reviews := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found XSS vulnerability",
		},
		{
			Agent:      "gemini",
			ReviewType: "security",
			Status:     "done",
			Output:     "No issues found.",
		},
	}
	prompt := BuildSynthesisPrompt(reviews, false, "")

	assertContainsAll(t, prompt, []string{
		"combining multiple code review outputs into a single GitHub PR comment",
		"reviewers agree the code is clean",
		"Do not independently review source code",
		`"sources":[1]`,
		"### Review N",
		"Only combine the input review results according to these rules",
		"### Review 1",
		"### Review 2",
		"Found XSS vulnerability",
		"No issues found.",
	})
	assert.NotContains(t, prompt, "Agent=")
	assert.NotContains(t, prompt, "Type=")
	assert.NotContains(t, prompt, "Verify each finding")
	assert.NotContains(t, prompt, "current codebase")
	assert.NotContains(t, prompt, "Rubric:")
	assert.NotContains(t, prompt, "different rubric")
}

func TestDecodeSynthesisDocument(t *testing.T) {
	assert := assert.New(t)
	reviews := []ReviewResult{
		{Agent: "codex", ReviewType: "default"},
		{Agent: "gemini", ReviewType: "security"},
	}

	doc, err := DecodeSynthesisDocument(jsontext.Value(
		`{"schema_version":1,"summary":"One shared finding.","findings":[
		  {"severity":"high","problem":"Leak","fix":"Close it","location":"a.go:1","sources":[2,1,2]}
		]}`,
	), reviews)
	require.NoError(t, err)
	assert.Equal(storage.VerdictFail, SynthesisVerdict(doc, ""))
	assert.Equal([]string{"codex", "gemini (security)"}, doc.SourceLabels)
	md := doc.Markdown("")
	assert.Contains(md, "One shared finding.")
	assert.Contains(md, "**Reported by:** gemini (security), codex")

	clean, err := DecodeSynthesisDocument(jsontext.Value(
		`{"schema_version":1,"summary":"Clean.","findings":[]}`,
	), reviews)
	require.NoError(t, err)
	assert.Equal(storage.VerdictPass, SynthesisVerdict(clean, ""))
	assert.NotContains(clean.Markdown(""), "Reported by")

	tests := []struct {
		name string
		raw  string
	}{
		{name: "malformed", raw: `not json`},
		{name: "legacy verdict shape", raw: `{"schema_version":1,"verdict":"pass","markdown":"Clean"}`},
		{name: "empty summary", raw: `{"schema_version":1,"summary":" ","findings":[]}`},
		{name: "missing sources", raw: `{"schema_version":1,"summary":"S","findings":[{"severity":"low","problem":"P","fix":"F","location":null}]}`},
		{name: "empty sources", raw: `{"schema_version":1,"summary":"S","findings":[{"severity":"low","problem":"P","fix":"F","location":null,"sources":[]}]}`},
		{name: "source out of range", raw: `{"schema_version":1,"summary":"S","findings":[{"severity":"low","problem":"P","fix":"F","location":null,"sources":[3]}]}`},
		{name: "unknown field", raw: `{"schema_version":1,"summary":"S","findings":[],"extra":true}`},
		{name: "multiple values", raw: `{"schema_version":1,"summary":"S","findings":[]} {}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeSynthesisDocument(jsontext.Value(tt.raw), reviews)
			require.Error(t, err)
		})
	}
}

func TestSynthesisDocumentRoundTripsNullLocation(t *testing.T) {
	doc, err := DecodeSynthesisDocument(jsontext.Value(
		`{"schema_version":1,"summary":"S","findings":[{"severity":"low","problem":"P","fix":"F","location":null,"sources":[1]}]}`,
	), []ReviewResult{{Agent: "codex"}})
	require.NoError(t, err)

	encoded, err := json.Marshal(doc)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"location":null`)
	assert.Contains(t, string(encoded), "source_labels")

	again, err := DecodeStructuredReview(encoded)
	require.NoError(t, err, "the stored document must satisfy the storage validator")
	assert.Equal(t, doc.Findings, again.Findings)
}

func TestSynthesisSourceLabels(t *testing.T) {
	labels := SynthesisSourceLabels([]ReviewResult{
		{Agent: "codex"},
		{Agent: "claude-code", ReviewType: "review"},
		{Agent: "gemini", ReviewType: "design"},
		{},
	})
	assert.Equal(t, []string{"codex", "claude-code", "gemini (design)", "review 4"}, labels)
}

func TestBuildSynthesisPrompt_UsesNeutralReviewerLabels(t *testing.T) {
	reviews := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Finding A",
		},
		{
			Agent:      "gemini",
			ReviewType: "design",
			Status:     "done",
			Output:     "Finding B",
		},
	}

	prompt := BuildSynthesisPrompt(reviews, false, "")

	assert.Contains(t, prompt, "### Review 1")
	assert.Contains(t, prompt, "### Review 2")
	assert.NotContains(t, prompt, "Agent=codex")
	assert.NotContains(t, prompt, "Agent=gemini")
	assert.NotContains(t, prompt, "Type=security")
	assert.NotContains(t, prompt, "Type=design")
	assert.NotContains(t, prompt, "security")
	assert.NotContains(t, prompt, "design")
}

func TestBuildSynthesisPrompt_NeutralLabelsRubrics(t *testing.T) {
	assert := assert.New(t)
	reviews := []ReviewResult{
		{ReviewType: "citations", Status: ResultDone, Output: "Claim lacks a source."},
		{ReviewType: "review", Status: ResultDone, Output: "Finding B."},
	}

	prompt := BuildSynthesisPrompt(reviews, true, "")

	assert.Contains(prompt, "combined review of the reviewed material")
	assert.Contains(prompt, "Silence from a different rubric is not disagreement")
	assert.Contains(prompt, "### Review 1\nRubric: citations\nClaim lacks a source.")
	assert.Contains(prompt, "### Review 2\nRubric: default\nFinding B.")
	assert.NotContains(prompt, "code review")
	assert.NotContains(prompt, "GitHub PR comment")
	assert.NotContains(prompt, "the code is clean")
	assert.NotContains(prompt, "source code")
}

// TestBuildSynthesisPrompt_NoInstructionsIsMergeOnly pins the complete prompt
// a panel without synthesis_instructions gets, so adding instructions cannot
// change that prompt.
func TestBuildSynthesisPrompt_NoInstructionsIsMergeOnly(t *testing.T) {
	reviews := []ReviewResult{
		{Agent: "codex", Status: ResultDone, Output: "High: nil deref in foo.go:10"},
		{Agent: "claude-code", Status: ResultDone, Output: "No issues found."},
	}

	prompt := BuildSynthesisPrompt(reviews, false, "")

	assert.Equal(t, "You are combining multiple code review outputs into a single GitHub PR comment.\n"+
		`Return exactly one JSON object with this shape and no code fence: {"schema_version":3,"summary":"...","verdict":"pass|fail","findings":[{"severity":"critical|high|medium|low","kind":"defect|improvement","topic":"copied from the input finding, or null","confidence":0.8,"problem":"...","fix":"...","file":"path or null","start_line":12,"end_line":14,"sources":[1]}]}`+"\n"+
		"Put a one-line overall summary in `summary` and your overall judgment in `verdict`. List every finding that requires changes in `findings`; return an empty `findings` array if the reviewers agree the code is clean.\n"+
		"In `sources`, list the numbers of the reviews below (### Review N) that reported the finding.\n"+
		"Rules:\n"+
		"- Only combine the input review results according to these rules\n"+
		"- Do not independently review source code or inspect the repository\n"+
		"- Do not use tools except to read referenced prompt or review-input files in full\n"+
		"- Deduplicate findings reported by multiple reviewers and cite every source\n"+
		"- Order findings by severity (Critical > High > Medium > Low)\n"+
		"- Keep each finding's kind and topic; when reviewers disagree, use the value most of them gave\n"+
		"- Set confidence to the highest confidence any source gave, or 0.5 when the sources give none\n"+
		"- Keep the file, start_line, and end_line of the findings you combine; use null when there is none\n"+
		"- When an input finding has a location but no file, name that location in the problem\n"+
		"- No preamble about yourself\n"+
		"\n---\n### Review 1\nHigh: nil deref in foo.go:10\n"+
		"\n---\n### Review 2\nNo issues found.\n\n", prompt)
}

func TestBuildSynthesisPrompt_InstructionsAllowRepositoryReads(t *testing.T) {
	reviews := []ReviewResult{
		{ReviewType: "citations", Status: ResultDone, Output: "Claim lacks a source."},
		{ReviewType: "review", Status: ResultDone, Output: "Finding B."},
	}
	const instructions = "Verify each finding against the code.\nStart each problem with \"Label: just fix\" or \"Label: needs the user\"."

	for _, neutral := range []bool{false, true} {
		t.Run(fmt.Sprintf("neutral=%t", neutral), func(t *testing.T) {
			assert := assert.New(t)
			prompt := BuildSynthesisPrompt(reviews, neutral, instructions)

			assert.Contains(prompt, "Rules:\n"+
				"- Combine the input review results according to these rules, then apply the additional synthesis instructions below\n"+
				"- You may read the repository with your read-only tools when the additional instructions call for it; never modify files\n"+
				"- Every finding must come from at least one input review; do not add findings of your own\n"+
				"- Deduplicate findings")
			assert.Contains(prompt, "- No preamble about yourself\n\n"+
				"## Additional synthesis instructions\n"+instructions+"\n\n---\n### Review 1\n")
			assert.NotContains(prompt, "Only combine the input review results")
			assert.NotContains(prompt, "inspect the repository")
			assert.NotContains(prompt, "Do not use tools")
		})
	}
}

func TestBuildSynthesisPrompt_QuotaAndFailed(t *testing.T) {
	reviews := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "looks good",
		},
		{
			Agent:      "gemini",
			ReviewType: "security",
			Status:     "failed",
			Error: QuotaErrorPrefix +
				"exhausted",
		},
		{
			Agent:      "droid",
			ReviewType: "security",
			Status:     "failed",
			Error:      "agent crashed",
		},
	}
	prompt := BuildSynthesisPrompt(reviews, false, "")

	assertContainsAll(t, prompt, []string{
		"[SKIPPED]",
		"[FAILED]",
		"quota exhausted",
	})
	assert.NotContains(t, prompt, "agent quota")
}

func TestBuildSynthesisPrompt_PreservesFullReviews(t *testing.T) {
	const promptLimit = 20000
	longOutput := strings.Repeat("x", promptLimit)
	reviews := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     longOutput,
		},
	}
	prompt := BuildSynthesisPrompt(reviews, false, "")

	assert.Contains(t, prompt, longOutput)
}

func TestFormatSingleResult_Truncation(t *testing.T) {
	r := ReviewResult{
		Agent:      "codex",
		ReviewType: "security",
		Status:     ResultDone,
		Output:     strings.Repeat("x", MaxCommentLen+500),
	}
	comment := formatSingleResult(r, "abc123456789", &CommentConfig{})

	// The header and footer add some overhead, but the output
	// portion must not exceed MaxCommentLen.
	require.LessOrEqual(t, len(comment), MaxCommentLen+200, "comment too long")
	assert.Contains(t, comment, "truncated", "expected truncation suffix")
}

func TestFormatSingleResult_TruncationUTF8Safe(t *testing.T) {
	// Place a 4-byte emoji so it straddles the actual cut boundary.
	// The cut point is maxLen = MaxCommentLen - len(CommentTruncSuffix)
	// applied to r.Output. Put the emoji starting 2 bytes before that
	// so a naive byte slice would land inside the 4-byte character.
	maxLen := MaxCommentLen - len(CommentTruncSuffix)
	paddingLen := maxLen - 2
	r := ReviewResult{
		Agent:      "codex",
		ReviewType: "security",
		Status:     ResultDone,
		Output:     strings.Repeat("x", paddingLen) + "😀" + strings.Repeat("y", 100),
	}
	comment := formatSingleResult(r, "abc123456789", &CommentConfig{})
	require.True(t, utf8.ValidString(comment), "truncated comment is not valid UTF-8")
	assert.Contains(t, comment, "truncated", "expected truncation suffix")
}

func TestFormatSynthesizedComment(t *testing.T) {
	reviews := []ReviewResult{
		{Agent: "codex", ReviewType: "security"},
		{Agent: "gemini", ReviewType: "design"},
	}
	comment := FormatSynthesizedComment(
		"Combined findings here", reviews,
		"abc123456789")

	assertContainsAll(t, comment, []string{
		"## roborev: Combined Review (`abc1234`)",
		"Combined findings here",
		"Synthesized from 2 reviews",
	})
	assert.NotContains(t, comment, "agents:")
	assert.NotContains(t, comment, "types:")
	assert.NotContains(t, comment, "security")
	assert.NotContains(t, comment, "design")
}

func TestFullReviewContentPreservesUTF8(t *testing.T) {
	// Keep multibyte content intact across the former 15,000-byte cutoff.
	const maxPerReview = 15000
	oversized := strings.Repeat("x", maxPerReview-2) + "😀" + strings.Repeat("y", 100)
	reviews := []ReviewResult{{
		Agent:      "codex",
		ReviewType: "security",
		Status:     ResultDone,
		Output:     oversized,
	}}

	t.Run("FormatRawBatchComment", func(t *testing.T) {
		comment := FormatRawBatchComment(CommentConfig{}, reviews, "def456789012")
		require.True(t, utf8.ValidString(comment),
			"posted comment must not contain a split rune")
		assert.Contains(t, comment, oversized)
	})

	t.Run("BuildSynthesisPrompt", func(t *testing.T) {
		prompt := BuildSynthesisPrompt(reviews, false, "")
		require.True(t, utf8.ValidString(prompt),
			"synthesis prompt must not contain a split rune")
		assert.Contains(t, prompt, oversized)
	})
}

func TestFormatRawBatchComment(t *testing.T) {
	reviews := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found issue X",
		},
		{
			Agent:      "gemini",
			ReviewType: "security",
			Status:     "failed",
			Error:      "crashed",
		},
	}
	comment := FormatRawBatchComment(CommentConfig{},
		reviews, "def456789012")

	assertContainsAll(t, comment, []string{
		"## roborev: Combined Review (`def4567`)",
		"Synthesis unavailable",
		"### Review 1 (done)",
		"Found issue X",
		"### Review 2 (failed)",
		"Review failed",
		"---",
	})
	assert.NotContains(t, comment, "codex")
	assert.NotContains(t, comment, "gemini")
	assert.NotContains(t, comment, "security")

	assert.NotContains(t, comment, "<details>", "raw batch comment should not use <details> blocks")
}

func TestFormatAllFailedComment(t *testing.T) {
	t.Run("real failures", func(t *testing.T) {
		reviews := []ReviewResult{
			{
				Agent:      "codex",
				ReviewType: "security",
				Status:     "failed",
				Error:      "crashed",
			},
		}
		comment := FormatAllFailedComment(
			reviews, "aaa111222333")

		assertContainsAll(t, comment, []string{
			"Review Failed",
			"Check CI logs",
		})
	})

	t.Run("all quota", func(t *testing.T) {
		reviews := []ReviewResult{
			{
				Agent:      "codex",
				ReviewType: "security",
				Status:     "failed",
				Error: QuotaErrorPrefix +
					"exhausted",
			},
		}
		comment := FormatAllFailedComment(
			reviews, "bbb222333444")

		assertContainsAll(t, comment, []string{"Review Skipped"})
		assert.NotContains(t, comment, "Check CI logs", "all-quota should not mention CI logs")
	})

	t.Run("all transient renders as skipped", func(t *testing.T) {
		assert := assert.New(t)
		reviews := []ReviewResult{
			{
				Agent:      "codex",
				ReviewType: "default",
				Status:     ResultFailed,
				Error:      OutageErrorPrefix + "429",
			},
		}
		comment := FormatAllFailedComment(
			reviews, "ccc333444555")

		assert.Contains(comment, "skipped (provider unavailable)")
		// A batch where every member is a transient skip must read as
		// "Review Skipped", not the contradictory "Review Failed" header.
		assert.Contains(comment, "Review Skipped")
		assert.NotContains(comment, "Review Failed")
		assert.NotContains(comment, "Check CI logs")
	})

	t.Run("mixed skips (quota, timeout, transient)", func(t *testing.T) {
		assert := assert.New(t)
		reviews := []ReviewResult{
			{
				Agent: "codex", ReviewType: "default", Status: ResultFailed,
				Error: QuotaErrorPrefix + "exhausted",
			},
			{
				Agent: "gemini", ReviewType: "security", Status: "canceled",
				Error: TimeoutErrorPrefix + "deadline",
			},
			{
				Agent: "claude-code", ReviewType: "default", Status: ResultFailed,
				Error: OutageErrorPrefix + "503 service unavailable",
			},
		}
		comment := FormatAllFailedComment(
			reviews, "ddd444555666")

		assert.Contains(comment, "Review Skipped")
		assert.NotContains(comment, "Review Failed")
		assert.NotContains(comment, "Check CI logs")
	})

	t.Run("transient plus genuine failure stays failed", func(t *testing.T) {
		assert := assert.New(t)
		reviews := []ReviewResult{
			{
				Agent: "codex", ReviewType: "default", Status: ResultFailed,
				Error: OutageErrorPrefix + "429",
			},
			{
				Agent: "gemini", ReviewType: "default", Status: ResultFailed,
				Error: "crashed",
			},
		}
		comment := FormatAllFailedComment(
			reviews, "eee555666777")

		// A genuine failure alongside a transient skip is not all-skipped.
		assert.Contains(comment, "Review Failed")
		assert.Contains(comment, "Check CI logs")
		// The transient member is still labelled as a skip, the genuine one as failed.
		assert.Contains(comment, "skipped (provider unavailable)")
		assert.Contains(comment, "Review 2: failed")
		assert.NotContains(comment, "**gemini** (default): failed")
	})
}

func TestSkippedAgentNote(t *testing.T) {
	t.Run("no skips", func(t *testing.T) {
		reviews := []ReviewResult{
			{Status: "done"},
		}
		assert.Empty(t, SkippedAgentNote(reviews))
	})

	t.Run("one skip", func(t *testing.T) {
		reviews := []ReviewResult{
			{
				Agent:  "gemini",
				Status: "failed",
				Error: QuotaErrorPrefix +
					"exhausted",
			},
		}
		note := SkippedAgentNote(reviews)
		assertContainsAll(t, note, []string{"1 review skipped", "quota exhausted"})
		assert.NotContains(t, note, "gemini")
		assert.NotContains(t, note, "agent quota")
	})

	t.Run("multiple skips", func(t *testing.T) {
		reviews := []ReviewResult{
			{
				Agent:  "codex",
				Status: "failed",
				Error:  QuotaErrorPrefix + "x",
			},
			{
				Agent:  "gemini",
				Status: "failed",
				Error:  QuotaErrorPrefix + "y",
			},
		}
		note := SkippedAgentNote(reviews)
		assertContainsAll(t, note, []string{"2 reviews skipped", "quota exhausted"})
		assert.NotContains(t, note, "codex")
		assert.NotContains(t, note, "gemini")
	})
}

func TestTransientMemberRendersSkipped(t *testing.T) {
	r := ReviewResult{
		Agent: "codex", ReviewType: "default",
		Status: ResultFailed, Error: OutageErrorPrefix + "429",
	}
	out := FormatRawBatchComment(CommentConfig{}, []ReviewResult{r}, "abc1234def")
	assert.Contains(t, out, "provider unavailable")
	assert.NotContains(t, out, "Review failed. Check CI logs")
}

func TestBuildSynthesisPrompt_IncludesSkipped(t *testing.T) {
	reviews := []ReviewResult{
		{
			Agent:      "claude-code",
			ReviewType: "design",
			Output:     "## Findings\n\n- Add type for foo\n",
			Status:     ResultDone,
		},
		{
			Agent:      "auto",
			ReviewType: "design",
			Status:     ResultSkipped,
			Skipped:    true,
			SkipReason: "trivial diff",
		},
	}
	prompt := BuildSynthesisPrompt(reviews, false, "")
	assertContainsAll(t, prompt, []string{
		"Add type for foo",
		"Review skipped: trivial diff",
		"trivial diff",
		"[SKIPPED]",
	})
	assert.NotContains(t, prompt, "Auto-design-review")
	assert.NotContains(t, prompt, "design")
}

func TestBuildSynthesisPrompt_TransientSkipped(t *testing.T) {
	reviews := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "default",
			Status:     ResultFailed,
			Error:      OutageErrorPrefix + "429",
		},
	}
	prompt := BuildSynthesisPrompt(reviews, false, "")
	assertContainsAll(t, prompt, []string{
		"[SKIPPED]",
		"provider unavailable",
	})
}

func TestBuildSynthesisPromptRendersStructuredMembersWithoutThreshold(t *testing.T) {
	structured := StructuredReview{
		SchemaVersion: storage.StructuredReviewSchemaVersion,
		Summary:       "One nit.",
		Verdict:       "pass",
		Findings:      []StructuredFinding{{Severity: "low", Problem: "Nit.", Fix: "Tidy."}},
	}
	member := ReviewResult{
		Agent: "codex", ReviewType: "review", Status: ResultDone, Structured: &structured,
	}.ApplyMinSeverity("medium")
	require.Contains(t, member.Output, "No findings at or above medium severity.")

	prompt := BuildSynthesisPrompt([]ReviewResult{member}, false, "")
	assert.Contains(t, prompt, "Nit.")
	assert.NotContains(t, prompt, "at or above")
	assert.NotContains(t, prompt, "medium severity")
}

// A panel member that finished before a daemon update can be stored as a
// version 2 review, whose findings have only a free-text location.
func TestBuildSynthesisPromptAnchorsVersion2Locations(t *testing.T) {
	structured, err := DecodeStructuredReview(jsontext.Value(`{"schema_version":2,"summary":"Two findings.","verdict":"fail","findings":[` +
		`{"severity":"high","problem":"Ranged.","fix":"f","location":"internal/a.go:12-14"},` +
		`{"severity":"low","problem":"Prose.","fix":"f","location":"the retry loop"}]}`))
	require.NoError(t, err)
	member := ReviewResult{Agent: "codex", ReviewType: "review", Status: ResultDone, Structured: &structured}

	prompt := BuildSynthesisPrompt([]ReviewResult{member}, false, "")

	assert := assert.New(t)
	assert.Contains(prompt, `"file": "internal/a.go"`)
	assert.Contains(prompt, `"start_line": 12`)
	assert.Contains(prompt, `"end_line": 14`)
	assert.Contains(prompt, `"location": "the retry loop"`)
	assert.Empty(structured.Findings[0].File, "the stored member is not changed")
}

func TestParseLocation(t *testing.T) {
	tests := []struct {
		location   string
		file       string
		start, end int
		ok         bool
	}{
		{"a.go:12", "a.go", 12, 12, true},
		{" dir/a.go:12-20 ", "dir/a.go", 12, 20, true},
		{"C:/repo/a.go:3", "C:/repo/a.go", 3, 3, true},
		{"a.go", "", 0, 0, false},
		{"a.go:20-12", "", 0, 0, false},
		{"a.go:0", "", 0, 0, false},
		{"see the retry loop: 3", "", 0, 0, false},
		{"near line 3:x", "", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.location, func(t *testing.T) {
			file, start, end, ok := parseLocation(tt.location)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.file, file)
			assert.Equal(t, tt.start, start)
			assert.Equal(t, tt.end, end)
		})
	}
}
