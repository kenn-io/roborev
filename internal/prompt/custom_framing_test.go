package prompt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
)

// Exercise all nine optional_sections through each assembly template. Supplied
// text deliberately contains domain words: only generated framing may change.
func TestCustomFramingOptionalSectionInventory(t *testing.T) {
	repo := newTestRepo(t)
	repo.writeFile("rubric.md", "Apply the supplied code rubric verbatim.")
	cfg := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{
		"citations": {Template: "rubric.md"},
	}}}
	builder := NewBuilder(nil).ForRepo(repo.dir, 0).WithRepoConfig(cfg, "")
	comments := []ReviewCommentTemplateContext{{Responder: "author", Response: "Keep the code quotation."}}
	optional := ReviewOptionalContext{
		KataContext:           &MarkdownSection{Heading: "## Task Context (kata)", Body: "Supplied task with code."},
		ProjectGuidelines:     &MarkdownSection{Heading: "## Project Guidelines", Body: "Preserve the code quotation."},
		AdditionalContext:     "## Additional Context\n\nSupplied code context.\n\n",
		DependencyMetadata:    "## Supporting File Metadata\n\n`go.mod`\n\n",
		Toolchain:             "## Format and Version Context\n\nGo 1.27\n\n",
		PreviousReviews:       []PreviousReviewTemplateContext{{Commit: "abc1234", Available: true, Output: "Finding about code.", Comments: comments}},
		InRangeReviews:        []InRangeReviewTemplateContext{{Commit: "def5678", Agent: "reviewer", Verdict: "fail", Output: "Range code finding.", Comments: comments}},
		PriorRangeReviewsFile: "history/reviews.xml",
		PreviousAttempts:      []ReviewAttemptTemplateContext{{Label: "Prior review", Agent: "reviewer", Output: "Earlier code finding.", Comments: comments}},
	}
	for _, kind := range []string{"single", "range", "dirty"} {
		t.Run(kind, func(t *testing.T) {
			ctx, err := builder.newPromptBuildContext("test", "citations", "", kind, optional)
			require.NoError(t, err)
			var body string
			switch kind {
			case "single":
				body, err = renderSinglePrompt(singlePromptView{Optional: ctx.optional})
			case "range":
				body, err = renderRangePrompt(rangePromptView{Optional: ctx.optional})
			case "dirty":
				body, err = renderDirtyPrompt(dirtyPromptView{Optional: ctx.optional})
			}
			require.NoError(t, err)
			got := ctx.requiredPrefix + body
			check := assert.New(t)
			for _, section := range []string{"Task Context (kata)", "Project Guidelines", "Additional Context", "Supporting File Metadata", "Format and Version Context", "Previous Reviews", "Per-Commit Reviews in This Range", "<prior-range-reviews", "Previous Review Attempts"} {
				check.Contains(got, section)
			}
			for _, supplied := range []string{"Apply the supplied code rubric verbatim.", "Supplied task with code.", "Preserve the code quotation.", "Supplied code context.", "Finding about code.", "Range code finding.", "Earlier code finding.", "Keep the code quotation."} {
				check.Contains(got, supplied)
			}
			check.Contains(got, "current material")
			check.Contains(got, "author responses")
			for _, generated := range []string{"current code", "final code", "similar code", "responses from developers", "a developer response"} {
				check.NotContains(got, generated)
			}
		})
	}
}

func TestCustomFramingBuilderIntroductions(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.fastCommitFile("rubric.md", "Review supplied code quotations.", "add rubric")
	head := repo.fastCommitFile("go.mod", "module example.test/draft\n\ngo 1.27\n", "update draft")
	global := &config.Config{}
	global.KataContext.Mode = config.KataModeCurrent
	fake := &katatest.FakeClient{BindingResult: kata.Binding{Project: "drafts"}, Issues: map[string]kata.Issue{
		"abc4": {ShortID: "abc4", Body: "Keep this code quotation."},
	}}
	cfg := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{
		"citations": {Template: "rubric.md"},
	}}}
	// Open-mode context works for all three target forms, including dirty.
	global.KataContext.Mode = config.KataModeOpen
	fake.ListResult = []kata.Issue{{ShortID: "abc4", Body: "Keep this code quotation."}}
	builder := NewBuilderWithConfig(nil, global).ForRepo(repo.dir, 0).WithRepoConfig(cfg, "").WithKataClient(fake)
	for _, target := range []string{head, base + ".." + head, "dirty"} {
		t.Run(target, func(t *testing.T) {
			var got string
			var err error
			if target == "dirty" {
				got, err = builder.BuildDirtyWithFiles("draft changes", []string{"go.mod"}, 0, "test", "citations", "")
			} else {
				got, err = builder.Build(target, 0, "test", "citations", "")
			}
			require.NoError(t, err)
			check := assert.New(t)
			check.Contains(got, "Keep this code quotation.")
			check.Contains(got, "Supporting File Metadata")
			check.Contains(got, "Format and Version Context")
			check.NotContains(got, "Judge version-gated language")
		})
	}
	global.KataContext.Mode = config.KataModeCurrent
	sha := repo.fastCommitFile("draft.md", "A quotation.", "revise draft\n\nCloses: drafts#abc4")
	got, err := builder.Build(sha, 0, "test", "citations", "")
	require.NoError(t, err)
	assert.Contains(t, got, "material that diverges")
	assert.NotContains(t, got, "code that diverges")
	assert.Contains(t, got, "Keep this code quotation.")
}
