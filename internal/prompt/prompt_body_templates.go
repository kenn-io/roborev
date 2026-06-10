package prompt

import (
	"bytes"
	"strconv"
	"strings"
	"text/template"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/storage"
)

type markdownSectionView = MarkdownSection

type reviewCommentView = ReviewCommentTemplateContext

type previousReviewView = PreviousReviewTemplateContext

type reviewAttemptView = ReviewAttemptTemplateContext

type optionalSectionsView = ReviewOptionalContext

type currentCommitSectionView = SingleSubjectContext

type commitRangeEntryView = RangeEntryContext

type commitRangeSectionView = RangeSubjectContext

type dirtyChangesSectionView = DirtySubjectContext

type diffSectionView struct {
	Heading  string
	Body     string
	Fallback string
}

type singlePromptView struct {
	Optional optionalSectionsView
	Current  currentCommitSectionView
	Diff     diffSectionView
}

type rangePromptView struct {
	Optional optionalSectionsView
	Current  commitRangeSectionView
	Diff     diffSectionView
}

type dirtyPromptView struct {
	Optional optionalSectionsView
	Current  dirtyChangesSectionView
	Diff     diffSectionView
}

type addressAttemptView = AddressAttemptTemplateContext

type addressPromptView struct {
	ProjectGuidelines *markdownSectionView
	ToolAttempts      []addressAttemptView
	UserComments      []addressAttemptView
	SeverityFilter    string
	ReviewFindings    string
	OriginalDiff      string
	JobID             int64
}

type reviewAttemptContext struct {
	Review    storage.Review
	Responses []storage.Response
}

type systemPromptView = SystemTemplateContext

type inlineDiffView struct {
	Body string
}

type dirtyTruncatedDiffFallbackView struct {
	Body string
}

var promptTemplates = template.Must(template.New("prompt-templates").ParseFS(
	templateFS,
	"templates/*.md.gotmpl",
))

func markdownSectionFromView(view *markdownSectionView) *MarkdownSection {
	if view == nil {
		return nil
	}
	return &MarkdownSection{Heading: view.Heading, Body: view.Body}
}

func reviewCommentsFromView(views []reviewCommentView) []ReviewCommentTemplateContext {
	comments := make([]ReviewCommentTemplateContext, 0, len(views))
	for _, view := range views {
		comments = append(comments, ReviewCommentTemplateContext(view))
	}
	return comments
}

func previousReviewsFromView(views []previousReviewView) []PreviousReviewTemplateContext {
	reviews := make([]PreviousReviewTemplateContext, 0, len(views))
	for _, view := range views {
		reviews = append(reviews, PreviousReviewTemplateContext{
			Commit:    view.Commit,
			Output:    view.Output,
			Comments:  reviewCommentsFromView(view.Comments),
			Available: view.Available,
		})
	}
	return reviews
}

func reviewAttemptsFromView(views []reviewAttemptView) []ReviewAttemptTemplateContext {
	attempts := make([]ReviewAttemptTemplateContext, 0, len(views))
	for _, view := range views {
		attempts = append(attempts, ReviewAttemptTemplateContext{
			Label:    view.Label,
			Agent:    view.Agent,
			When:     view.When,
			Output:   view.Output,
			Comments: reviewCommentsFromView(view.Comments),
		})
	}
	return attempts
}

func reviewOptionalContextFromView(view optionalSectionsView) ReviewOptionalContext {
	return ReviewOptionalContext{
		ProjectGuidelines:  markdownSectionFromView(view.ProjectGuidelines),
		KataContext:        markdownSectionFromView(view.KataContext),
		AdditionalContext:  view.AdditionalContext,
		DependencyMetadata: view.DependencyMetadata,
		PreviousReviews:    previousReviewsFromView(view.PreviousReviews),
		InRangeReviews:     inRangeReviewsFromView(view.InRangeReviews),
		PreviousAttempts:   reviewAttemptsFromView(view.PreviousAttempts),
	}
}

func inRangeReviewsFromView(views []InRangeReviewTemplateContext) []InRangeReviewTemplateContext {
	reviews := make([]InRangeReviewTemplateContext, 0, len(views))
	for _, view := range views {
		reviews = append(reviews, InRangeReviewTemplateContext{
			Commit:   view.Commit,
			Agent:    view.Agent,
			Verdict:  view.Verdict,
			Output:   view.Output,
			Comments: reviewCommentsFromView(view.Comments),
		})
	}
	return reviews
}

type commitInspectionFallbackView struct {
	SHA         string
	StatCmd     string
	DiffCmd     string
	FilesCmd    string
	ShowPathCmd string
}

type rangeInspectionFallbackView struct {
	RangeRef string
	LogCmd   string
	StatCmd  string
	DiffCmd  string
	FilesCmd string
	ViewCmd  string
}

type genericDiffFallbackView struct {
	ViewCmd string
}

func fallbackContextFromDiffSection(view diffSectionView) FallbackContext {
	if view.Fallback == "" {
		return FallbackContext{}
	}
	return FallbackContext{Mode: FallbackModeGeneric, Text: view.Fallback}
}

func templateContextFromSingleView(view singlePromptView) TemplateContext {
	return TemplateContext{
		Review: &ReviewTemplateContext{
			Kind:     ReviewKindSingle,
			Optional: reviewOptionalContextFromView(view.Optional),
			Subject: SubjectContext{Single: &SingleSubjectContext{
				Commit:  view.Current.Commit,
				Subject: view.Current.Subject,
				Author:  view.Current.Author,
				Message: view.Current.Message,
			}},
			Diff:     DiffContext{Heading: view.Diff.Heading, Body: view.Diff.Body},
			Fallback: fallbackContextFromDiffSection(view.Diff),
		},
	}
}

func templateContextFromRangeView(view rangePromptView) TemplateContext {
	entries := make([]RangeEntryContext, 0, len(view.Current.Entries))
	for _, entry := range view.Current.Entries {
		entries = append(entries, RangeEntryContext(entry))
	}
	return TemplateContext{
		Review: &ReviewTemplateContext{
			Kind:     ReviewKindRange,
			Optional: reviewOptionalContextFromView(view.Optional),
			Subject:  SubjectContext{Range: &RangeSubjectContext{Count: view.Current.Count, Entries: entries}},
			Diff:     DiffContext{Heading: view.Diff.Heading, Body: view.Diff.Body},
			Fallback: fallbackContextFromDiffSection(view.Diff),
		},
	}
}

func templateContextFromDirtyView(view dirtyPromptView) TemplateContext {
	fallback := fallbackContextFromDiffSection(view.Diff)
	if fallback.Text != "" {
		fallback.Mode = FallbackModeDirty
		fallback.Dirty = &DirtyFallbackContext{Body: fallback.Text}
		fallback.Text = ""
	}
	return TemplateContext{
		Review: &ReviewTemplateContext{
			Kind:     ReviewKindDirty,
			Optional: reviewOptionalContextFromView(view.Optional),
			Subject:  SubjectContext{Dirty: &DirtySubjectContext{Description: view.Current.Description}},
			Diff:     DiffContext{Heading: view.Diff.Heading, Body: view.Diff.Body},
			Fallback: fallback,
		},
	}
}

func templateContextFromAddressView(view addressPromptView) TemplateContext {
	toolAttempts := make([]AddressAttemptTemplateContext, 0, len(view.ToolAttempts))
	for _, attempt := range view.ToolAttempts {
		toolAttempts = append(toolAttempts, AddressAttemptTemplateContext(attempt))
	}
	userComments := make([]AddressAttemptTemplateContext, 0, len(view.UserComments))
	for _, comment := range view.UserComments {
		userComments = append(userComments, AddressAttemptTemplateContext(comment))
	}
	return TemplateContext{
		Address: &AddressTemplateContext{
			ProjectGuidelines: markdownSectionFromView(view.ProjectGuidelines),
			ToolAttempts:      toolAttempts,
			UserComments:      userComments,
			SeverityFilter:    view.SeverityFilter,
			ReviewFindings:    view.ReviewFindings,
			OriginalDiff:      view.OriginalDiff,
			JobID:             view.JobID,
		},
	}
}

func templateContextFromSystemView(view systemPromptView) TemplateContext {
	return TemplateContext{System: &SystemTemplateContext{NoSkillsInstruction: view.NoSkillsInstruction, ToolchainVerificationInstruction: view.ToolchainVerificationInstruction, CurrentDate: view.CurrentDate}}
}

func renderSinglePromptContext(ctx TemplateContext) (string, error) {
	return executePromptTemplate("assembled_single.md.gotmpl", ctx)
}

func renderRangePromptContext(ctx TemplateContext) (string, error) {
	return executePromptTemplate("assembled_range.md.gotmpl", ctx)
}

func renderDirtyPromptContext(ctx TemplateContext) (string, error) {
	return executePromptTemplate("assembled_dirty.md.gotmpl", ctx)
}

func renderAddressPromptContext(ctx TemplateContext) (string, error) {
	return executePromptTemplate("assembled_address.md.gotmpl", ctx)
}

func renderSinglePrompt(view singlePromptView) (string, error) {
	return renderSinglePromptContext(templateContextFromSingleView(view))
}

func renderRangePrompt(view rangePromptView) (string, error) {
	return renderRangePromptContext(templateContextFromRangeView(view))
}

func renderDirtyPrompt(view dirtyPromptView) (string, error) {
	return renderDirtyPromptContext(templateContextFromDirtyView(view))
}

func renderAddressPrompt(view addressPromptView) (string, error) {
	return renderAddressPromptContext(templateContextFromAddressView(view))
}

func fitSinglePromptContext(limit int, ctx TemplateContext) (string, error) {
	body, err := renderSinglePromptContext(ctx)
	if err != nil {
		return "", err
	}
	if len(body) <= limit {
		return body, nil
	}

	for ctx.Review != nil && ctx.Review.Optional.TrimNext() {
		body, err = renderSinglePromptContext(ctx)
		if err != nil {
			return "", err
		}
		if len(body) <= limit {
			return body, nil
		}
	}

	if ctx.Review != nil && ctx.Review.Subject.TrimSingleMessage() {
		body, err = renderSinglePromptContext(ctx)
		if err != nil {
			return "", err
		}
		if len(body) <= limit {
			return body, nil
		}
	}

	if ctx.Review != nil && ctx.Review.Subject.TrimSingleAuthor() {
		body, err = renderSinglePromptContext(ctx)
		if err != nil {
			return "", err
		}
		if len(body) <= limit {
			return body, nil
		}
	}

	for ctx.Review != nil && ctx.Review.Subject.Single != nil && len(body) > limit && ctx.Review.Subject.Single.Subject != "" {
		overflow := len(body) - limit
		ctx.Review.Subject.TrimSingleSubjectTo(max(0, len(ctx.Review.Subject.Single.Subject)-overflow))
		body, err = renderSinglePromptContext(ctx)
		if err != nil {
			return "", err
		}
	}

	return hardCapPrompt(body, limit), nil
}

func fitSinglePrompt(limit int, view singlePromptView) (string, error) {
	return fitSinglePromptContext(limit, templateContextFromSingleView(view))
}

func fitRangePromptContext(limit int, ctx TemplateContext) (string, error) {
	_, body, err := trimRangePromptContext(limit, ctx)
	if err != nil {
		return "", err
	}
	return hardCapPrompt(body, limit), nil
}

func fitRangePrompt(limit int, view rangePromptView) (string, error) {
	return fitRangePromptContext(limit, templateContextFromRangeView(view))
}

func trimRangePromptContext(limit int, ctx TemplateContext) (TemplateContext, string, error) {
	ctx = ctx.Clone()
	body, err := renderRangePromptContext(ctx)
	if err != nil {
		return TemplateContext{}, "", err
	}
	if len(body) <= limit {
		return ctx, body, nil
	}

	for ctx.Review != nil && ctx.Review.Optional.TrimNext() {
		body, err = renderRangePromptContext(ctx)
		if err != nil {
			return TemplateContext{}, "", err
		}
		if len(body) <= limit {
			return ctx, body, nil
		}
	}

	for ctx.Review != nil && len(body) > limit && ctx.Review.Subject.BlankNextRangeSubject() {
		body, err = renderRangePromptContext(ctx)
		if err != nil {
			return TemplateContext{}, "", err
		}
	}

	for ctx.Review != nil && len(body) > limit && ctx.Review.Subject.DropLastRangeEntry() {
		body, err = renderRangePromptContext(ctx)
		if err != nil {
			return TemplateContext{}, "", err
		}
	}

	return ctx, body, nil
}

func fitDirtyPromptContext(limit int, ctx TemplateContext) (string, error) {
	body, err := renderDirtyPromptContext(ctx)
	if err != nil {
		return "", err
	}
	if len(body) <= limit {
		return body, nil
	}

	for ctx.Review != nil && ctx.Review.Optional.TrimNext() {
		body, err = renderDirtyPromptContext(ctx)
		if err != nil {
			return "", err
		}
		if len(body) <= limit {
			return body, nil
		}
	}

	return hardCapPrompt(body, limit), nil
}

func fitDirtyPrompt(limit int, view dirtyPromptView) (string, error) {
	return fitDirtyPromptContext(limit, templateContextFromDirtyView(view))
}

func trimOptionalSections(view *optionalSectionsView) bool {
	if view == nil {
		return false
	}
	ctx := reviewOptionalContextFromView(*view)
	if !ctx.TrimNext() {
		return false
	}
	*view = ctx
	return true
}

func renderSystemPrompt(name string, view systemPromptView) (string, error) {
	return executePromptTemplate(name, templateContextFromSystemView(view))
}

func renderAddressPromptFromSections(view addressPromptView) (string, error) {
	return renderAddressPrompt(view)
}

func renderOptionalSectionsFromView(view optionalSectionsView) (string, error) {
	body, err := executePromptTemplate("optional_sections", view)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(body), nil
}

func renderOptionalSectionsPrefix(view optionalSectionsView) (string, error) {
	body, err := renderOptionalSectionsFromView(view)
	if err != nil || body == "" {
		return body, err
	}
	return body + "\n\n", nil
}

func renderCurrentCommitRequired(view currentCommitSectionView) (string, error) {
	return executePromptTemplate("current_commit_required", view)
}

func renderCurrentCommitOverflow(view currentCommitSectionView) (string, error) {
	return executePromptTemplate("current_commit_overflow", view)
}

func renderCommitRangeRequired(view commitRangeSectionView) (string, error) {
	return executePromptTemplate("commit_range_required", view)
}

func renderCommitRangeOverflow(view commitRangeSectionView) (string, error) {
	return executePromptTemplate("commit_range_overflow", view)
}

func renderDirtyChangesSection(view dirtyChangesSectionView) (string, error) {
	return executePromptTemplate("dirty_changes", view)
}

func renderDiffBlock(view diffSectionView) (string, error) {
	ctx := ReviewTemplateContext{
		Diff:     DiffContext{Heading: view.Heading, Body: view.Body},
		Fallback: fallbackContextFromDiffSection(view),
	}
	return executePromptTemplate("diff_block", ctx)
}

func renderInlineDiff(body string) (string, error) {
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return executePromptTemplate("inline_diff", inlineDiffView{Body: body})
}

func renderCommitInspectionFallback(name string, view commitInspectionFallbackView) (string, error) {
	return executePromptTemplate(name, view)
}

func renderRangeInspectionFallback(name string, view rangeInspectionFallbackView) (string, error) {
	return executePromptTemplate(name, view)
}

func renderGenericCommitFallback(viewCmd string) (string, error) {
	return executePromptTemplate("generic_commit_fallback", genericDiffFallbackView{ViewCmd: viewCmd})
}

func renderGenericRangeFallback(viewCmd string) (string, error) {
	return executePromptTemplate("generic_range_fallback", genericDiffFallbackView{ViewCmd: viewCmd})
}

func renderDirtyTruncatedDiffFallback(body string) (string, error) {
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return executePromptTemplate("dirty_truncated_diff_fallback", dirtyTruncatedDiffFallbackView{Body: body})
}

func previousReviewViews(contexts []HistoricalReviewContext) []previousReviewView {
	views := make([]previousReviewView, 0, len(contexts))
	for _, ctx := range contexts {
		view := previousReviewView{Commit: gitrepo.ShortSHA(ctx.SHA)}
		if ctx.Review != nil {
			view.Available = true
			view.Output = ctx.Review.Output
		}
		if len(ctx.Responses) > 0 {
			view.Comments = make([]reviewCommentView, 0, len(ctx.Responses))
			for _, resp := range ctx.Responses {
				view.Comments = append(view.Comments, reviewCommentView{Responder: resp.Responder, Response: resp.Response})
			}
		}
		views = append(views, view)
	}
	return views
}

func inRangeReviewViews(contexts []HistoricalReviewContext) []InRangeReviewTemplateContext {
	views := make([]InRangeReviewTemplateContext, 0, len(contexts))
	for _, ctx := range contexts {
		if ctx.Review == nil {
			continue
		}
		verdictLabel := "unknown"
		switch storage.ParseVerdict(ctx.Review.Output) {
		case "P":
			verdictLabel = "passed"
		case "F":
			verdictLabel = "failed"
		}
		view := InRangeReviewTemplateContext{
			Commit:  gitrepo.ShortSHA(ctx.SHA),
			Agent:   ctx.Review.Agent,
			Verdict: verdictLabel,
			Output:  ctx.Review.Output,
		}
		if len(ctx.Responses) > 0 {
			view.Comments = make([]reviewCommentView, 0, len(ctx.Responses))
			for _, resp := range ctx.Responses {
				view.Comments = append(view.Comments, reviewCommentView{Responder: resp.Responder, Response: resp.Response})
			}
		}
		views = append(views, view)
	}
	return views
}

func renderPreviousReviewsFromContexts(contexts []HistoricalReviewContext) (string, error) {
	return renderOptionalSectionsFromView(optionalSectionsView{PreviousReviews: previousReviewViews(contexts)})
}

func reviewAttemptViews(reviews []storage.Review) []reviewAttemptView {
	views := make([]reviewAttemptView, 0, len(reviews))
	for i, review := range reviews {
		when := ""
		if !review.CreatedAt.IsZero() {
			when = review.CreatedAt.Format("2006-01-02 15:04")
		}
		views = append(views, reviewAttemptView{
			Label:  "Review Attempt " + strconv.Itoa(i+1),
			Agent:  review.Agent,
			When:   when,
			Output: review.Output,
		})
	}
	return views
}

func renderPreviousAttemptsFromReviews(reviews []storage.Review) (string, error) {
	return renderOptionalSectionsFromView(optionalSectionsView{PreviousAttempts: reviewAttemptViews(reviews)})
}

func previousAttemptViewsFromContexts(attempts []reviewAttemptContext) []reviewAttemptView {
	views := make([]reviewAttemptView, 0, len(attempts))
	for i, attempt := range attempts {
		view := reviewAttemptView{
			Label:  "Review Attempt " + strconv.Itoa(i+1),
			Agent:  attempt.Review.Agent,
			Output: attempt.Review.Output,
		}
		if !attempt.Review.CreatedAt.IsZero() {
			view.When = attempt.Review.CreatedAt.Format("2006-01-02 15:04")
		}
		if len(attempt.Responses) > 0 {
			view.Comments = make([]reviewCommentView, 0, len(attempt.Responses))
			for _, resp := range attempt.Responses {
				view.Comments = append(view.Comments, reviewCommentView{Responder: resp.Responder, Response: resp.Response})
			}
		}
		views = append(views, view)
	}
	return views
}

func executePromptTemplate(name string, view any) (string, error) {
	var buf bytes.Buffer
	if err := promptTemplates.ExecuteTemplate(&buf, name, view); err != nil {
		return "", err
	}
	return buf.String(), nil
}
