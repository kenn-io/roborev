package prompt

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestRenderSinglePromptBodyUsesNestedSections(t *testing.T) {
	view := singlePromptView{
		Optional: optionalSectionsView{
			ProjectGuidelines: &markdownSectionView{
				Heading: "## Project Guidelines",
				Body:    "Prefer composition over inheritance.",
			},
			AdditionalContext: "## Pull Request Discussion\n\nNewest comment first.\n\n",
		},
		Current: currentCommitSectionView{
			Commit:  "abc1234",
			Subject: "template prompt rendering",
			Author:  "Test User",
		},
		Diff: diffSectionView{
			Heading: "### Diff",
			Body:    "```diff\n+line\n```\n",
		},
	}

	body, err := renderSinglePrompt(view)
	require.NoError(t, err)

	assert.Contains(t, body, "## Project Guidelines")
	assert.Contains(t, body, "## Pull Request Discussion")
	assert.Contains(t, body, "## Current Commit")
	assert.Contains(t, body, "**Subject:** template prompt rendering")
	assert.Contains(t, body, "### Diff")
}

func TestRenderRangePromptUsesNestedSections(t *testing.T) {
	view := rangePromptView{
		Optional: optionalSectionsView{
			AdditionalContext: "## Pull Request Discussion\n\nNewest comment first.\n\n",
		},
		Current: commitRangeSectionView{
			Entries: []commitRangeEntryView{{Commit: "abc1234", Subject: "first"}, {Commit: "def5678", Subject: "second"}},
		},
		Diff: diffSectionView{
			Heading: "### Combined Diff",
			Body:    "```diff\n+line\n```\n",
		},
	}

	body, err := renderRangePrompt(view)
	require.NoError(t, err)

	assert.Contains(t, body, "## Pull Request Discussion")
	assert.Contains(t, body, "## Commit Range")
	assert.Contains(t, body, "- abc1234 first")
	assert.Contains(t, body, "- def5678 second")
	assert.Contains(t, body, "### Combined Diff")
}

func TestRenderCommitRangeSectionsFromTemplate(t *testing.T) {
	required, err := renderCommitRangeRequired(commitRangeSectionView{
		Entries: []commitRangeEntryView{{Commit: "abc1234", Subject: "first"}, {Commit: "def5678", Subject: "second"}},
	})
	require.NoError(t, err)
	assert.Contains(t, required, "## Commit Range")
	assert.Contains(t, required, "Reviewing 2 commits:")

	overflow, err := renderCommitRangeOverflow(commitRangeSectionView{
		Entries: []commitRangeEntryView{{Commit: "abc1234", Subject: "first"}, {Commit: "def5678", Subject: "second"}},
	})
	require.NoError(t, err)
	assert.Contains(t, overflow, "- abc1234 first")
	assert.Contains(t, overflow, "- def5678 second")
}

func TestRenderDirtyTruncatedDiffFallbackFromTemplate(t *testing.T) {
	fallback, err := renderDirtyTruncatedDiffFallback("+line\n... (truncated)\n")
	require.NoError(t, err)
	assert.Contains(t, fallback, "(Diff too large to include in full)")
	assert.Contains(t, fallback, "```diff")
	assert.Contains(t, fallback, "+line")
	assert.Contains(t, fallback, "... (truncated)")
}

func TestRenderDirtyPromptUsesNestedSections(t *testing.T) {
	view := dirtyPromptView{
		Optional: optionalSectionsView{
			AdditionalContext: "## Pull Request Discussion\n\nNewest comment first.\n\n",
		},
		Current: dirtyChangesSectionView{
			Description: "The following changes have not yet been committed.",
		},
		Diff: diffSectionView{
			Heading: "### Diff",
			Body:    "```diff\n+line\n```\n",
		},
	}

	body, err := renderDirtyPrompt(view)
	require.NoError(t, err)

	assert.Contains(t, body, "## Pull Request Discussion")
	assert.Contains(t, body, "## Uncommitted Changes")
	assert.Contains(t, body, "The following changes have not yet been committed.")
	assert.Contains(t, body, "### Diff")
}

func TestRenderAddressPromptUsesNestedSections(t *testing.T) {
	view := addressPromptView{
		ProjectGuidelines: &markdownSectionView{
			Heading: "## Project Guidelines",
			Body:    "Keep it simple.",
		},
		ToolAttempts:   []addressAttemptView{{Responder: "roborev-fix", Response: "Tried a narrow fix", When: "2026-04-04 12:00"}},
		UserComments:   []addressAttemptView{{Responder: "alice", Response: "This is a false positive", When: "2026-04-04 13:00"}},
		SeverityFilter: "Only address medium and higher findings.\n\n",
		ReviewFindings: "- medium: do the thing",
		OriginalDiff:   "diff --git a/a b/a\n+line\n",
		JobID:          42,
	}

	body, err := renderAddressPrompt(view)
	require.NoError(t, err)

	assert.Contains(t, body, "## Project Guidelines")
	assert.Contains(t, body, "## Previous Addressing Attempts")
	assert.Contains(t, body, "roborev-fix")
	assert.Contains(t, body, "## User Comments")
	assert.Contains(t, body, "alice")
	assert.Contains(t, body, "false positive")
	assert.Contains(t, body, "## Review Findings to Address (Job 42)")
	assert.Contains(t, body, "## Original Commit Diff (for context)")
}

func TestRenderSystemPromptUsesTemplateData(t *testing.T) {
	body, err := renderSystemPrompt("default_review.md.gotmpl", systemPromptView{
		NoSkillsInstruction: noSkillsInstruction,
		CurrentDate:         "2030-06-15",
	})
	require.NoError(t, err)

	assert.Contains(t, body, "You are a code reviewer")
	assert.Contains(t, body, "Do NOT use any external skills")
	assert.Contains(t, body, "Current date: 2030-06-15 (UTC)")
}

func TestRenderSinglePromptPreservesRawText(t *testing.T) {
	view := singlePromptView{
		Optional: optionalSectionsView{
			ProjectGuidelines: &markdownSectionView{
				Heading: "## Project Guidelines",
				Body:    "context with <tag> & symbols > keep",
			},
		},
		Current: currentCommitSectionView{
			Commit:  "abc1234",
			Subject: "required <input> && output > all",
			Author:  "overflow uses <, >, and &",
		},
		Diff: diffSectionView{
			Heading: "### Diff",
			Body:    "```diff\n+ use <raw> & keep > unchanged\n```\n",
		},
	}

	body, err := renderSinglePrompt(view)
	require.NoError(t, err)
	assert.Contains(t, body, "context with <tag> & symbols > keep")
	assert.Contains(t, body, "required <input> && output > all")
	assert.Contains(t, body, "overflow uses <, >, and &")
	assert.Contains(t, body, "+ use <raw> & keep > unchanged")
}

func TestRenderSinglePromptUsesFallbackOverBody(t *testing.T) {
	body, err := renderSinglePrompt(singlePromptView{
		Current: currentCommitSectionView{Commit: "abc1234", Subject: "subject", Author: "author"},
		Diff:    diffSectionView{Heading: "### Diff", Body: "body", Fallback: "fallback"},
	})
	require.NoError(t, err)
	assert.Contains(t, body, "fallback")
	assert.NotContains(t, body, "body")
}

func TestRenderOptionalSectionsFromTypedData(t *testing.T) {
	body, err := renderOptionalSectionsFromView(optionalSectionsView{
		ProjectGuidelines: &markdownSectionView{
			Heading: "## Project Guidelines",
			Body:    "Prefer composition over inheritance.",
		},
		AdditionalContext: "## Pull Request Discussion\n\nNewest comment first.\n\n",
		PreviousReviews: []previousReviewView{{
			Commit:    "abc1234",
			Available: true,
			Output:    "Found a bug",
			Comments:  []reviewCommentView{{Responder: "alice", Response: "Known issue"}},
		}},
		PreviousAttempts: []reviewAttemptView{{
			Label:    "Review Attempt 1",
			Agent:    "test",
			When:     "2026-04-05 10:00",
			Output:   "Still failing",
			Comments: []reviewCommentView{{Responder: "bob", Response: "Will fix"}},
		}},
	})
	require.NoError(t, err)
	assert.Contains(t, body, "## Project Guidelines")
	assert.Contains(t, body, "These guidelines supplement the default review criteria")
	assert.Contains(t, body, "## Pull Request Discussion")
	assert.Contains(t, body, "## Previous Reviews")
	assert.Contains(t, body, "Found a bug")
	assert.Contains(t, body, "Known issue")
	assert.Contains(t, body, "## Previous Review Attempts")
	assert.Contains(t, body, "Review Attempt 1")
	assert.Contains(t, body, "Will fix")
}

func TestRenderOptionalSectionsOmitsEmptySections(t *testing.T) {
	body, err := renderOptionalSectionsFromView(optionalSectionsView{})
	require.NoError(t, err)
	assert.Empty(t, body)
}

func TestBuildAdditionalContextSectionTrimsAndFormats(t *testing.T) {
	body := buildAdditionalContextSection("\n## Pull Request Discussion\n\nNewest comment first.\n")
	assert.Equal(t, "## Pull Request Discussion\n\nNewest comment first.\n\n", body)
}

func TestBuildProjectGuidelinesSectionViewTrimsAndFormats(t *testing.T) {
	section := buildProjectGuidelinesSectionView("\nPrefer composition over inheritance.\n")
	require.NotNil(t, section)
	assert.Equal(t, "## Project Guidelines", section.Heading)
	assert.Equal(t, "Prefer composition over inheritance.", section.Body)
}

func TestReviewOptionalContextTrimNextPreservesPriority(t *testing.T) {
	ctx := ReviewOptionalContext{
		ProjectGuidelines: &MarkdownSection{Heading: "## Project Guidelines", Body: "Keep it simple."},
		AdditionalContext: "## Pull Request Discussion\n\ncontext\n\n",
		PreviousReviews:   []PreviousReviewTemplateContext{{Commit: "abc1234", Output: "review", Available: true}},
		InRangeReviews:    []InRangeReviewTemplateContext{{Commit: "def5678", Output: "in-range"}},
		PreviousAttempts:  []ReviewAttemptTemplateContext{{Label: "Review Attempt 1", Output: "attempt"}},
	}

	require.True(t, ctx.TrimNext())
	assert.Empty(t, ctx.PreviousAttempts)
	require.True(t, ctx.TrimNext())
	assert.Empty(t, ctx.InRangeReviews)
	require.True(t, ctx.TrimNext())
	assert.Empty(t, ctx.PreviousReviews)
	require.True(t, ctx.TrimNext())
	assert.Empty(t, ctx.AdditionalContext)
	require.True(t, ctx.TrimNext())
	assert.Nil(t, ctx.ProjectGuidelines)
	assert.False(t, ctx.TrimNext())
}

// TestTrimOptionalSectionsPropagatesInRangeReviewsClear guards against a
// regression where trimOptionalSections ran TrimNext on a local copy but
// then rebuilt the caller's view field-by-field, omitting InRangeReviews
// so the cleared slice never made it back to the caller.
func TestTrimOptionalSectionsPropagatesInRangeReviewsClear(t *testing.T) {
	view := optionalSectionsView{
		PreviousReviews: []PreviousReviewTemplateContext{{Commit: "abc1234", Output: "prev", Available: true}},
		InRangeReviews:  []InRangeReviewTemplateContext{{Commit: "def5678", Output: "in-range"}},
	}

	require.True(t, trimOptionalSections(&view))
	assert.Empty(t, view.InRangeReviews, "TrimNext cleared InRangeReviews; the view must reflect the clear")
	assert.NotEmpty(t, view.PreviousReviews, "only one section should be trimmed per call")

	require.True(t, trimOptionalSections(&view))
	assert.Empty(t, view.PreviousReviews)

	assert.False(t, trimOptionalSections(&view))
}

// TestMeasureOptionalSectionsLossCountsInRangeReviews guards against the
// prior bug where selectRichestRangePromptView treated "kept in-range
// reviews" and "dropped in-range reviews" as equally good, so a richer diff
// fallback could silently discard the per-commit review context.
func TestMeasureOptionalSectionsLossCountsInRangeReviews(t *testing.T) {
	original := ReviewOptionalContext{
		InRangeReviews: []InRangeReviewTemplateContext{{Commit: "abc1234", Output: "in-range"}},
	}
	kept := original
	dropped := ReviewOptionalContext{}

	assert.Zero(t, measureOptionalSectionsLoss(original, kept),
		"keeping InRangeReviews must score zero loss")
	assert.Equal(t, 1, measureOptionalSectionsLoss(original, dropped),
		"dropping InRangeReviews must register as a loss")
}

func TestTemplateContextCloneIsolatesNestedState(t *testing.T) {
	ctx := TemplateContext{
		Review: &ReviewTemplateContext{
			Optional: ReviewOptionalContext{
				ProjectGuidelines: &MarkdownSection{Heading: "## Project Guidelines", Body: "Keep it simple."},
				PreviousAttempts:  []ReviewAttemptTemplateContext{{Label: "Review Attempt 1", Output: "attempt"}},
			},
			Subject: SubjectContext{
				Range: &RangeSubjectContext{Count: 2, Entries: []RangeEntryContext{{Commit: "abc1234", Subject: "first"}, {Commit: "def5678", Subject: "second"}}},
			},
			Fallback: FallbackContext{Mode: FallbackModeRange, Range: &RangeFallbackContext{DiffCmd: "git diff"}},
		},
	}

	cloned := ctx.Clone()
	require.NotNil(t, cloned.Review)
	require.True(t, cloned.Review.Optional.TrimNext())
	require.True(t, cloned.Review.Subject.BlankNextRangeSubject())
	cloned.Review.Fallback.Range.DiffCmd = "git diff --stat"

	require.NotNil(t, ctx.Review.Optional.ProjectGuidelines)
	require.Len(t, ctx.Review.Optional.PreviousAttempts, 1)
	require.NotNil(t, ctx.Review.Subject.Range)
	assert.Equal(t, "second", ctx.Review.Subject.Range.Entries[1].Subject)
	assert.Equal(t, "git diff", ctx.Review.Fallback.Range.DiffCmd)
}

func TestTemplateContextSubjectRangeTrimmingHelpers(t *testing.T) {
	ctx := SubjectContext{Range: &RangeSubjectContext{Count: 2, Entries: []RangeEntryContext{{Commit: "abc1234", Subject: "first"}, {Commit: "def5678", Subject: "second"}}}}

	require.True(t, ctx.BlankNextRangeSubject())
	require.NotNil(t, ctx.Range)
	assert.Empty(t, ctx.Range.Entries[1].Subject)
	assert.Equal(t, "first", ctx.Range.Entries[0].Subject)
	require.True(t, ctx.DropLastRangeEntry())
	require.Len(t, ctx.Range.Entries, 1)
	assert.Equal(t, "abc1234", ctx.Range.Entries[0].Commit)
}

func TestHistoricalReviewContextPreviousReviewViewsPreserveChronologicalOrder(t *testing.T) {
	views := previousReviewViews([]HistoricalReviewContext{
		{SHA: "bbbbbbb", Review: &storage.Review{Output: "second"}},
		{SHA: "aaaaaaa", Review: &storage.Review{Output: "first"}},
	})
	require.Len(t, views, 2)
	assert.Equal(t, "bbbbbbb", views[0].Commit)
	assert.Equal(t, "aaaaaaa", views[1].Commit)
}

func TestRenderPreviousReviewsFromContexts(t *testing.T) {
	body, err := renderPreviousReviewsFromContexts([]HistoricalReviewContext{
		{
			SHA:    "abc1234",
			Review: &storage.Review{Output: "Found a bug"},
			Responses: []storage.Response{{
				Responder: "alice",
				Response:  "Known issue",
			}},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, body, "## Previous Reviews")
	assert.Contains(t, body, "Found a bug")
	assert.Contains(t, body, "Known issue")
}

func TestReviewAttemptViewsPreserveOrderAndMetadata(t *testing.T) {
	views := reviewAttemptViews([]storage.Review{{
		Agent:     "test",
		Output:    "first",
		CreatedAt: mustParsePromptTestTime(t, "2026-04-05 10:00"),
	}})
	require.Len(t, views, 1)
	assert.Equal(t, "Review Attempt 1", views[0].Label)
	assert.Equal(t, "test", views[0].Agent)
	assert.Equal(t, "2026-04-05 10:00", views[0].When)
	assert.Equal(t, "first", views[0].Output)
}

func TestRenderPreviousAttemptsFromReviews(t *testing.T) {
	body, err := renderPreviousAttemptsFromReviews([]storage.Review{{
		Agent:     "test",
		Output:    "first",
		CreatedAt: mustParsePromptTestTime(t, "2026-04-05 10:00"),
	}})
	require.NoError(t, err)
	assert.Contains(t, body, "## Previous Review Attempts")
	assert.Contains(t, body, "Review Attempt 1")
	assert.Contains(t, body, "first")
}

func mustParsePromptTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02 15:04", value)
	require.NoError(t, err)
	return parsed
}

func TestRenderAddressPromptOmitsOptionalSectionsWhenEmpty(t *testing.T) {
	body, err := renderAddressPrompt(addressPromptView{ReviewFindings: "finding", JobID: 1})
	require.NoError(t, err)
	assert.NotContains(t, body, "## Previous Addressing Attempts")
	assert.NotContains(t, body, "## Original Commit Diff")
}

func TestFitSinglePromptTrimsOptionalSectionsBeforeCurrentMetadata(t *testing.T) {
	view := singlePromptView{
		Optional: optionalSectionsView{
			AdditionalContext: strings.Repeat("g", 128),
		},
		Current: currentCommitSectionView{
			Commit:  "abc1234",
			Subject: "large change",
			Author:  "Test User",
		},
		Diff: diffSectionView{
			Heading:  "### Diff",
			Fallback: "(Diff too large; for Codex run `git show abc1234 --` locally.)\n",
		},
	}
	trimmed := view
	trimmed.Optional = optionalSectionsView{}
	trimmedBody, err := renderSinglePrompt(trimmed)
	require.NoError(t, err)

	body, err := fitSinglePrompt(len(trimmedBody), view)
	require.NoError(t, err)
	assert.Contains(t, body, "## Current Commit")
	assert.Contains(t, body, "**Subject:** large change")
	assert.NotContains(t, body, strings.Repeat("g", 128))
}

func TestFitRangePromptTrimsOptionalSectionsBeforeRangeMetadata(t *testing.T) {
	view := rangePromptView{
		Optional: optionalSectionsView{
			AdditionalContext: strings.Repeat("g", 128),
		},
		Current: commitRangeSectionView{
			Entries: []commitRangeEntryView{{Commit: "abc1234", Subject: "first change"}, {Commit: "def5678", Subject: "second change"}},
		},
		Diff: diffSectionView{
			Heading:  "### Combined Diff",
			Fallback: "(Diff too large; for Codex run `git diff abc1234..def5678 --` locally.)\n",
		},
	}
	trimmed := view
	trimmed.Optional = optionalSectionsView{}
	trimmedBody, err := renderRangePrompt(trimmed)
	require.NoError(t, err)

	body, err := fitRangePrompt(len(trimmedBody), view)
	require.NoError(t, err)
	assert.Contains(t, body, "## Commit Range")
	assert.Contains(t, body, "- abc1234 first change")
	assert.NotContains(t, body, strings.Repeat("g", 128))
}

func TestFitRangePromptDropsTrailingEntriesBeforeCombinedDiff(t *testing.T) {
	entries := make([]commitRangeEntryView, 0, 80)
	for i := range 80 {
		entries = append(entries, commitRangeEntryView{
			Commit:  fmt.Sprintf("%07x", i),
			Subject: "very large subject that should be removed before the combined diff is dropped",
		})
	}
	view := rangePromptView{
		Current: commitRangeSectionView{Count: 80, Entries: entries},
		Diff: diffSectionView{
			Heading:  "### Combined Diff",
			Fallback: "(Diff too large; for Codex run `git diff base..head --` locally.)\n",
		},
	}
	required, err := renderCommitRangeRequired(commitRangeSectionView{Count: 80, Entries: entries})
	require.NoError(t, err)
	diffBlock, err := renderDiffBlock(view.Diff)
	require.NoError(t, err)
	limit := len(required) + len(diffBlock) + (12 * len("- 0000000\n"))

	body, err := fitRangePrompt(limit, view)
	require.NoError(t, err)

	assert.Contains(t, body, "Reviewing 80 commits:")
	assert.Contains(t, body, "### Combined Diff")
	assert.Contains(t, body, "git diff base..head")
	assert.NotContains(t, body, "- 000004f",
		"range fitting should trim trailing entries before hard-capping away the combined diff")
}

func TestFitDirtyPromptTrimsOptionalSectionsBeforeFallbackDiff(t *testing.T) {
	view := dirtyPromptView{
		Optional: optionalSectionsView{
			AdditionalContext: strings.Repeat("g", 128),
		},
		Current: dirtyChangesSectionView{
			Description: "The following changes have not yet been committed.",
		},
		Diff: diffSectionView{
			Heading:  "### Diff",
			Fallback: "(Diff too large to include in full)\n```diff\n+line\n... (truncated)\n```\n",
		},
	}
	trimmed := view
	trimmed.Optional = optionalSectionsView{}
	trimmedBody, err := renderDirtyPrompt(trimmed)
	require.NoError(t, err)

	body, err := fitDirtyPrompt(len(trimmedBody), view)
	require.NoError(t, err)
	assert.Contains(t, body, "## Uncommitted Changes")
	assert.Contains(t, body, "(Diff too large to include in full)")
	assert.NotContains(t, body, strings.Repeat("g", 128))
}
