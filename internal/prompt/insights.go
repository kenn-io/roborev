package prompt

import (
	"fmt"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/storage"
)

// InsightsSystemPrompt is the instruction for analyzing review patterns
const InsightsSystemPrompt = `You are a code review insights analyst. Your task is to analyze a collection of failing code reviews and identify actionable patterns that can improve future reviews.

You will be given:
1. A set of failing review outputs with metadata (agent, date, git ref, addressed status)
2. The current project review guidelines (if any)

Your analysis should produce:

## 1. Recurring Finding Patterns

Identify clusters of similar findings that appear across multiple reviews. Apply evidence thresholds:
- 1-2 occurrences: note as a data point only, do not recommend action
- 3-5 occurrences: emerging pattern, suggest as a candidate guideline with moderate confidence
- 6+ occurrences: strong signal, recommend specific guideline text with high confidence

For each pattern:
- Name the pattern concisely
- Count how many reviews contain it
- Give 1-2 representative examples with file/line references if available
- Assess whether this pattern represents a real code quality issue or review noise

## 2. Hotspot Areas

Identify files, packages, or directories that concentrate failures. List them with:
- Path or package name
- Number of failing reviews touching this area
- Common finding types in this area

## 3. Noise Candidates

Identify finding types that are consistently present in reviews that were closed (marked "addressed/closed") and accompanied by developer comments suggesting the finding was intentional or a false positive. These suggest the review guideline should suppress them. For each:
- Describe the finding type
- Note how many times it appeared and was closed with dismissive comments
- Suggest whether to suppress it entirely or refine the criteria

## 4. Guideline Gaps

Identify patterns the reviews keep flagging that are NOT mentioned in the current guidelines. For each:
- Describe what the reviews are catching
- Explain why it should (or shouldn't) be codified as a guideline
- Suggest specific guideline text if appropriate

## 5. Suggested Guideline Additions

Only suggest guidelines backed by 3+ occurrences from section 1 or 3+ noise candidates from section 3. For each, provide:
- The concrete text snippet ready to add to review_guidelines configuration
- The occurrence count that justifies this addition
- Whether this suppresses noise or codifies a real quality standard

Be specific and actionable. Avoid vague recommendations. Base everything on evidence from the provided reviews.`

// InsightsData holds the data needed to build an insights prompt
type InsightsData struct {
	Reviews            []InsightsReview
	Guidelines         string
	RepoName           string
	Since              time.Time
	MaxReviews         int // Cap for number of reviews to include
	MaxOutputPerReview int // Cap for individual review output size
	MaxPromptSize      int // Overall prompt size budget (0 = use MaxPromptSize default)
}

// InsightsReview is a simplified review record for the insights prompt
type InsightsReview struct {
	JobID        int64
	Agent        string
	GitRef       string
	Branch       string
	FinishedAt   *time.Time
	Output       string
	Responses    []storage.Response
	Closed       bool
	Verdict      string // "P" or "F"
	PanelName    string
	PanelMembers int // Configured members, including advisory members
}

// BuildInsightsPrompt constructs the full prompt for insights analysis.
// It prioritizes unaddressed (open) findings over addressed (closed) ones
// when truncating to fit within size limits.
func BuildInsightsPrompt(data InsightsData) string {
	var sb strings.Builder

	promptBudget := data.MaxPromptSize
	if promptBudget <= 0 {
		promptBudget = MaxPromptSize
	}

	sb.WriteString(InsightsSystemPrompt)
	sb.WriteString("\n\n")

	// Current guidelines section — cap at 10% of prompt budget so
	// oversized guidelines don't crowd out review data.
	sb.WriteString("## Current Review Guidelines\n\n")
	if data.Guidelines != "" {
		guidelines := data.Guidelines
		guidelineCap := min(promptBudget/10, 1024)
		if len(guidelines) > guidelineCap {
			guidelines = guidelines[:guidelineCap] + "\n... (guidelines truncated)"
		}
		sb.WriteString(guidelines)
	} else {
		sb.WriteString("(No review guidelines configured for this project)")
	}
	sb.WriteString("\n\n")

	// Separate into open (unaddressed) and closed (addressed) reviews
	var open, closed []InsightsReview
	for _, r := range data.Reviews {
		if r.Closed {
			closed = append(closed, r)
		} else {
			open = append(open, r)
		}
	}

	maxReviews := data.MaxReviews
	if maxReviews <= 0 {
		maxReviews = 50
	}
	maxOutput := data.MaxOutputPerReview
	if maxOutput <= 0 {
		maxOutput = 8192
	}
	// Prioritize open reviews, fill remaining slots with closed
	var selected []InsightsReview
	for _, r := range open {
		if len(selected) >= maxReviews {
			break
		}
		selected = append(selected, r)
	}
	for _, r := range closed {
		if len(selected) >= maxReviews {
			break
		}
		selected = append(selected, r)
	}

	// Reviews section
	sb.WriteString("## Failing Reviews\n\n")
	fmt.Fprintf(&sb, "Showing %d failing review(s) since %s.\n\n",
		len(selected), data.Since.Format("2006-01-02"))

	for i, r := range selected {
		// Build the review entry in a temporary buffer so we can
		// check size before committing it to the prompt.
		var entry strings.Builder
		fmt.Fprintf(&entry, "### Review %d (Job #%d)\n\n", i+1, r.JobID)
		fmt.Fprintf(&entry, "- **Agent:** %s\n", r.Agent)
		if r.PanelName != "" {
			fmt.Fprintf(&entry, "- **Panel:** %s", r.PanelName)
			if r.PanelMembers > 0 {
				fmt.Fprintf(&entry, " (%d configured members)", r.PanelMembers)
			}
			entry.WriteString("\n")
		}
		fmt.Fprintf(&entry, "- **Git Ref:** %s\n", r.GitRef)
		if r.Branch != "" {
			fmt.Fprintf(&entry, "- **Branch:** %s\n", r.Branch)
		}
		if r.FinishedAt != nil {
			fmt.Fprintf(&entry, "- **Date:** %s\n", r.FinishedAt.Format("2006-01-02 15:04"))
		}
		status := "unaddressed"
		if r.Closed {
			status = "addressed/closed"
		}
		fmt.Fprintf(&entry, "- **Status:** %s\n", status)
		entry.WriteString("\n")

		output := r.Output
		if len(output) > maxOutput {
			output = output[:maxOutput] + "\n... (truncated)"
		}
		entry.WriteString(output)
		if !strings.HasSuffix(output, "\n") {
			entry.WriteString("\n")
		}
		if len(r.Responses) > 0 {
			entry.WriteString("\nComments on this review:\n")
			commentCap := 2048
			commentBytes := 0
			for _, resp := range r.Responses {
				line := fmt.Sprintf("- %s: %q\n", resp.Responder, resp.Response)
				if commentBytes+len(line) > commentCap {
					entry.WriteString("... (remaining comments truncated)\n")
					break
				}
				entry.WriteString(line)
				commentBytes += len(line)
			}
		}
		entry.WriteString("\n")

		// Pre-check: would adding this entry exceed the budget?
		if sb.Len()+entry.Len() > promptBudget-256 {
			remaining := len(selected) - i
			fmt.Fprintf(&sb, "\n(Remaining %d reviews omitted due to prompt size limits)\n", remaining)
			break
		}

		sb.WriteString(entry.String())
	}

	if len(data.Reviews) > len(selected) {
		fmt.Fprintf(&sb, "\nNote: %d additional failing reviews were omitted (showing most recent %d, prioritizing unaddressed findings).\n",
			len(data.Reviews)-len(selected), len(selected))
	}

	return sb.String()
}

// InsightsReviewFromJob converts a ReviewJob (with verdict) to an InsightsReview.
// The review output must be fetched separately.
func InsightsReviewFromJob(
	job storage.ReviewJob, output string, responses []storage.Response, closed bool,
) InsightsReview {
	verdict := ""
	if job.Verdict != nil {
		verdict = *job.Verdict
	}
	result := InsightsReview{
		JobID:      job.ID,
		Agent:      job.Agent,
		GitRef:     job.GitRef,
		Branch:     job.Branch,
		FinishedAt: job.FinishedAt,
		Output:     output,
		Responses:  responses,
		Closed:     closed,
		Verdict:    verdict,
	}
	if job.IsSynthesisJob() {
		result.PanelName = job.PanelName
		if job.PanelSummary != nil {
			result.PanelMembers = job.PanelSummary.MembersTotal
		}
	}
	return result
}
