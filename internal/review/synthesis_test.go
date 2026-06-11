package review

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	prompt := BuildSynthesisPrompt(reviews, "")

	assertContainsAll(t, prompt, []string{
		"combining multiple code review outputs",
		"Do not call tools or run commands",
		"Only combine the input review results according to these rules",
		"Agent=codex",
		"Agent=gemini",
		"Found XSS vulnerability",
		"No issues found.",
	})
	assert.NotContains(t, prompt, "Verify each finding")
	assert.NotContains(t, prompt, "current codebase")
}

func TestBuildSynthesisPrompt_Severity(t *testing.T) {
	reviews := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "test output",
		},
	}

	tests := []struct {
		name            string
		severity        string
		wantContains    string
		wantNotContains string
	}{
		{"high severity", "high", "Only include High and Critical", ""},
		{"low severity", "low", "", "Omit findings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := BuildSynthesisPrompt(reviews, tt.severity)
			if tt.wantContains != "" {
				assert.Contains(t, prompt, tt.wantContains)
			}
			if tt.wantNotContains != "" {
				assert.NotContains(t, prompt, tt.wantNotContains)
			}
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
	prompt := BuildSynthesisPrompt(reviews, "")

	assertContainsAll(t, prompt, []string{
		"[SKIPPED]",
		"[FAILED]",
		"agent quota exhausted",
	})
}

func TestBuildSynthesisPrompt_Truncation(t *testing.T) {
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
	prompt := BuildSynthesisPrompt(reviews, "")

	assertContainsAll(t, prompt, []string{"...(truncated)"})
	assert.LessOrEqual(t, len(prompt), promptLimit, "prompt should be truncated")
}

func TestFormatSingleResult_Truncation(t *testing.T) {
	r := ReviewResult{
		Agent:      "codex",
		ReviewType: "security",
		Status:     ResultDone,
		Output:     strings.Repeat("x", MaxCommentLen+500),
	}
	comment := formatSingleResult(r, "abc123456789")

	// The header and footer add some overhead, but the output
	// portion must not exceed MaxCommentLen.
	require.LessOrEqual(t, len(comment), MaxCommentLen+200, "comment too long")
	assert.Contains(t, comment, "truncated", "expected truncation suffix")
}

func TestFormatSingleResult_TruncationUTF8Safe(t *testing.T) {
	// Place a 4-byte emoji so it straddles the actual cut boundary.
	// The cut point is maxLen = MaxCommentLen - len("\n\n...(truncated)")
	// applied to r.Output. Put the emoji starting 2 bytes before that
	// so a naive byte slice would land inside the 4-byte character.
	const truncSuffix = "\n\n...(truncated)"
	maxLen := MaxCommentLen - len(truncSuffix)
	paddingLen := maxLen - 2
	r := ReviewResult{
		Agent:      "codex",
		ReviewType: "security",
		Status:     ResultDone,
		Output:     strings.Repeat("x", paddingLen) + "😀" + strings.Repeat("y", 100),
	}
	comment := formatSingleResult(r, "abc123456789")
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
		"codex",
		"gemini",
		"security",
		"design",
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
	comment := FormatRawBatchComment(
		reviews, "def456789012")

	assertContainsAll(t, comment, []string{
		"## roborev: Combined Review (`def4567`)",
		"Synthesis unavailable",
		"### codex — security (done)",
		"Found issue X",
		"### gemini — security (failed)",
		"Review failed",
		"---",
	})

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
		assert.Contains(comment, "**gemini** (default): failed")
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
		assertContainsAll(t, note, []string{"gemini", "review skipped"})
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
		assertContainsAll(t, note, []string{"reviews skipped"})
	})
}

func TestGiveUpAndSoftNoteComments(t *testing.T) {
	assert := assert.New(t)
	g := FormatTransientGiveUpComment("abc1234def", "429 too many requests")
	assert.Contains(g, "## roborev: Review Unavailable (`abc1234`)")
	assert.Contains(g, "3 days")
	assert.Contains(g, "429 too many requests")

	s := FormatGenuineSoftNoteComment("abc1234def", "model not supported")
	assert.Contains(s, "## roborev: Review Unavailable (`abc1234`)")
	assert.Contains(s, "next commit")
	assert.Contains(s, "model not supported")
}

func TestGiveUpAndSoftNoteCommentsSuppressEmptyExcerpt(t *testing.T) {
	assert := assert.New(t)
	g := FormatTransientGiveUpComment("abc1234def", "   ")
	assert.Contains(g, "## roborev: Review Unavailable (`abc1234`)")
	assert.NotContains(g, "Last error")

	s := FormatGenuineSoftNoteComment("abc1234def", "")
	assert.Contains(s, "## roborev: Review Unavailable (`abc1234`)")
	assert.NotContains(s, "Last error")
}

func TestTransientMemberRendersSkipped(t *testing.T) {
	r := ReviewResult{
		Agent: "codex", ReviewType: "default",
		Status: ResultFailed, Error: OutageErrorPrefix + "429",
	}
	out := FormatRawBatchComment([]ReviewResult{r}, "abc1234def")
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
	prompt := BuildSynthesisPrompt(reviews, "")
	assertContainsAll(t, prompt, []string{
		"Add type for foo",
		"Auto-design-review skipped",
		"trivial diff",
		"[SKIPPED]",
	})
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
	prompt := BuildSynthesisPrompt(reviews, "")
	assertContainsAll(t, prompt, []string{
		"[SKIPPED]",
		"provider unavailable",
	})
}
