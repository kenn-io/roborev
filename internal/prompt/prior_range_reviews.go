package prompt

import (
	"encoding/xml"
	"fmt"
	"slices"
	"strings"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
)

// PriorRangeReviewsFilePathPlaceholder defers file creation for stored prompts
// until the worker can place the document in the agent's checkout.
const PriorRangeReviewsFilePathPlaceholder = "/tmp/roborev prior range reviews placeholder"

type priorRangeReview struct {
	Range    string                    `xml:"range,attr"`
	Agent    string                    `xml:"agent,attr"`
	When     string                    `xml:"created-at,attr"`
	Output   string                    `xml:"output"`
	Comments []priorRangeReviewComment `xml:"comments>comment"`
}

type priorRangeReviewComment struct {
	Responder string `xml:"responder,attr"`
	Response  string `xml:",chardata"`
}

func (b *Builder) priorRangeReviewsForRef(rangeRef string, limit int) []priorRangeReview {
	if !git.IsRange(rangeRef) || b.db == nil || b.repoID <= 0 || limit <= 0 {
		return nil
	}
	start, err := git.GetRangeStartCtx(b.context(), b.repoPath, rangeRef)
	if err != nil {
		return nil
	}
	commits, err := git.GetRangeCommitsCtx(b.context(), b.repoPath, rangeRef)
	if err != nil {
		return nil
	}
	return b.priorRangeReviewViews(start, rangeRef, commits, limit)
}

func (b *Builder) writePriorRangeReviewsSnapshot(reviews []priorRangeReview, target SnapshotTarget) (string, func(), error) {
	document := struct {
		XMLName xml.Name           `xml:"prior-range-reviews"`
		Reviews []priorRangeReview `xml:"review"`
	}{Reviews: reviews}
	content, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("encode prior range reviews: %w", err)
	}
	repoPath, snapshotRoot, err := b.resolveSnapshotTarget(target)
	if err != nil {
		return "", nil, err
	}
	return writeExternalSnapshot(repoPath, snapshotRoot, "prior-range-reviews.xml", xml.Header+string(content)+"\n")
}

// PreparePriorRangeReviewsSnapshot materializes a stored prompt's optional
// historical context for this execution. The caller owns the returned cleanup.
func (b *Builder) PreparePriorRangeReviewsSnapshot(reviewPrompt, rangeRef string, limit int, target SnapshotTarget) (SnapshotResult, error) {
	if !strings.Contains(reviewPrompt, PriorRangeReviewsFilePathPlaceholder) {
		return SnapshotResult{Prompt: reviewPrompt}, nil
	}
	reviews := b.priorRangeReviewsForRef(rangeRef, limit)
	path, cleanup, err := b.writePriorRangeReviewsSnapshot(reviews, target)
	if err != nil {
		return SnapshotResult{}, err
	}
	prepared := strings.ReplaceAll(reviewPrompt, PriorRangeReviewsFilePathPlaceholder, escapeXML(path))
	if len(prepared) > b.resolveMaxPromptSize() {
		// The longer execution path may exhaust a prebuilt prompt's remaining
		// budget. Drop the optional reference as a whole, leaving the diff intact.
		reference, renderErr := renderOptionalSectionsFromView(optionalSectionsView{PriorRangeReviewsFile: path})
		if renderErr != nil {
			cleanup()
			return SnapshotResult{}, renderErr
		}
		trimmed := strings.ReplaceAll(prepared, reference, "")
		if trimmed != prepared {
			cleanup()
			return SnapshotResult{Prompt: trimmed}, nil
		}
	}
	return SnapshotResult{Prompt: prepared, Cleanup: cleanup}, nil
}

const priorRangeReviewPageSize = 40

func (b *Builder) priorRangeReviewViews(
	rangeStart, rangeRef string, commits []string, limit int,
) []priorRangeReview {
	if b.db == nil || b.repoID <= 0 || rangeStart == "" || limit <= 0 || len(commits) == 0 {
		return nil
	}
	commitIndex := make(map[string]int, len(commits))
	for i, commit := range commits {
		commitIndex[commit] = i
	}
	currentEnd := commits[len(commits)-1]
	type selected struct {
		view  priorRangeReview
		index int
	}
	selectedByEnd := make(map[string]selected)
	resolveCache := make(map[string]string)
	resolve := func(ref string) (string, bool) {
		if resolved, ok := resolveCache[ref]; ok {
			return resolved, resolved != ""
		}
		resolved, err := git.ResolveSHACtx(b.context(), b.repoPath, ref)
		if err != nil {
			resolveCache[ref] = ""
			return "", false
		}
		resolveCache[ref] = resolved
		return resolved, true
	}
	for offset := 0; len(selectedByEnd) < limit; offset += priorRangeReviewPageSize {
		candidates, err := b.db.GetRecentRangeReviewCandidates(b.repoID, priorRangeReviewPageSize, offset)
		if err != nil {
			return nil
		}
		for _, candidate := range candidates {
			if candidate.GitRef == rangeRef {
				continue
			}
			start, end, ok := git.ParseRange(candidate.GitRef)
			if !ok {
				continue
			}
			resolvedStart, ok := resolve(start)
			if !ok || resolvedStart != rangeStart {
				continue
			}
			resolvedEnd, ok := resolve(end)
			index, contained := commitIndex[resolvedEnd]
			if !ok || !contained || resolvedEnd == currentEnd {
				continue
			}
			if _, exists := selectedByEnd[resolvedEnd]; exists {
				continue
			}
			review, err := b.db.GetReviewByJobID(candidate.JobID)
			if err != nil || review == nil {
				continue
			}
			view := priorRangeReview{
				Range:  gitrepo.ShortSHA(resolvedStart) + ".." + gitrepo.ShortSHA(resolvedEnd),
				Agent:  review.Agent,
				When:   review.CreatedAt.Format("2006-01-02 15:04"),
				Output: review.Output,
			}
			if responses, err := b.db.GetCommentsForJob(review.JobID); err == nil {
				for _, response := range storage.PromptTrustedResponses(responses) {
					view.Comments = append(view.Comments, priorRangeReviewComment{Responder: response.Responder, Response: response.Response})
				}
			}
			selectedByEnd[resolvedEnd] = selected{view: view, index: index}
		}
		if len(candidates) < priorRangeReviewPageSize {
			break
		}
	}
	selectedViews := make([]selected, 0, len(selectedByEnd))
	for _, candidate := range selectedByEnd {
		selectedViews = append(selectedViews, candidate)
	}
	slices.SortFunc(selectedViews, func(a, b selected) int { return a.index - b.index })
	if len(selectedViews) > limit {
		selectedViews = selectedViews[len(selectedViews)-limit:]
	}
	views := make([]priorRangeReview, 0, len(selectedViews))
	for _, candidate := range selectedViews {
		views = append(views, candidate.view)
	}
	return views
}
