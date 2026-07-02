package review

import (
	"fmt"
	"strings"

	gitrepo "go.kenn.io/kit/git/repo"
)

// severityAbove maps a minimum severity to the instruction
// describing which levels to include in synthesis output.
var severityAbove = map[string]string{
	"critical": "Only include Critical findings.",
	"high":     "Only include High and Critical findings.",
	"medium":   "Only include Medium, High, and Critical findings.",
}

// VerifyDedupePreamble returns the instruction block used by the compact
// command's consolidation prompt: verify each finding against the current
// codebase, consolidate duplicates, and re-emit every still-valid finding
// using the regular review output format so downstream verdict parsing and
// fix flows keep working.
func VerifyDedupePreamble() string {
	return "## Instructions\n\n" +
		"1. **Verify each finding against the current codebase:**\n" +
		"   - Search the codebase to check if the issue still exists\n" +
		"   - Use wide code search patterns (grep, find files, read context)\n" +
		"   - Mark findings as VERIFIED or FALSE_POSITIVE\n\n" +
		"2. **Consolidate related findings:**\n" +
		"   - Group findings that address the same underlying issue\n" +
		"   - Merge duplicate findings from different reviews\n" +
		"   - Provide a single comprehensive description for each group\n\n" +
		"3. **Output format:**\n" +
		"   - Use the review output format above, including verdict-compatible finding structure\n" +
		"   - Every verified finding that still applies must be repeated in the compact output\n" +
		"   - Separate repeated findings with the same `---` delimiter used by regular reviews\n" +
		"   - Counts, totals, and summaries may accompany repeated findings, but must not replace them\n" +
		"   - The summary may mention how many prior findings were dropped as fixed, duplicates, or false positives\n\n"
}

// BuildSynthesisPrompt creates the prompt for the synthesis agent.
// When minSeverity is non-empty (and not "low"), a filtering
// instruction is appended.
func BuildSynthesisPrompt(
	reviews []ReviewResult,
	minSeverity string,
) string {
	var b strings.Builder
	b.WriteString(
		"You are combining multiple code review outputs " +
			"into a single GitHub PR comment.\nRules:\n" +
			"- Do not call tools or run commands\n" +
			"- Only combine the input review results according to these rules\n" +
			"- Deduplicate findings reported by multiple reviewers\n" +
			"- Organize by severity (Critical > High > Medium > Low)\n" +
			"- Preserve file/line references\n" +
			"- If all reviewers agree code is clean, say so concisely\n" +
			"- Start with a one-line summary verdict\n" +
			"- Use markdown formatting\n" +
			"- No preamble about yourself\n")

	if instruction, ok := severityAbove[minSeverity]; ok {
		b.WriteString(
			"- Omit findings below " + minSeverity +
				" severity. " + instruction + "\n")
	}

	b.WriteString("\n")

	// Truncate per-review output to avoid blowing the synthesis
	// agent's context window.
	const maxPerReview = 15000

	for i, r := range reviews {
		fmt.Fprintf(&b, "---\n### Review %d", i+1)
		if r.Skipped || r.Status == ResultSkipped {
			b.WriteString(" [SKIPPED]")
		} else if IsQuotaFailure(r) {
			b.WriteString(" [SKIPPED]")
		} else if IsTransientFailure(r) {
			b.WriteString(" [SKIPPED]")
		} else if r.Status == ResultFailed {
			b.WriteString(" [FAILED]")
		}
		b.WriteString("\n")
		if r.Skipped || r.Status == ResultSkipped {
			reason := r.SkipReason
			if reason == "" {
				reason = "no reason recorded"
			}
			b.WriteString("Review skipped: " + reason)
		} else if IsQuotaFailure(r) {
			b.WriteString(
				"(review skipped — quota exhausted)")
		} else if IsTransientFailure(r) {
			b.WriteString(
				"(review skipped — provider unavailable)")
		} else if r.Output != "" {
			output := r.Output
			if len(output) > maxPerReview {
				output = output[:maxPerReview] +
					"\n\n...(truncated)"
			}
			b.WriteString(output)
		} else if r.Status == ResultFailed {
			b.WriteString("(no output — review failed)")
		}
		b.WriteString("\n\n")
	}

	return b.String()
}

// FormatSynthesizedComment wraps synthesized output with header
// and metadata.
func FormatSynthesizedComment(
	output string,
	reviews []ReviewResult,
	headSHA string,
) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"## roborev: Combined Review (`%s`)\n\n",
		gitrepo.ShortSHA(headSHA))
	b.WriteString(output)

	fmt.Fprintf(&b,
		"\n\n---\n*Synthesized from %d reviews*\n",
		len(reviews))

	if note := SkippedAgentNote(reviews); note != "" {
		b.WriteString(note)
	}

	return b.String()
}

// FormatRawBatchComment formats all review outputs as expanded
// inline sections. Used as a fallback when synthesis fails.
func FormatRawBatchComment(
	reviews []ReviewResult,
	headSHA string,
) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"## roborev: Combined Review (`%s`)\n\n",
		gitrepo.ShortSHA(headSHA))
	b.WriteString(
		"> Synthesis unavailable. " +
			"Showing individual review outputs.\n\n")

	for i, r := range reviews {
		if i > 0 {
			b.WriteString("---\n\n")
		}
		status := r.Status
		if IsQuotaFailure(r) {
			status = "skipped (quota)"
		} else if IsTransientFailure(r) {
			status = "skipped (provider unavailable)"
		} else if r.Skipped || r.Status == ResultSkipped {
			status = "skipped"
		}
		fmt.Fprintf(&b, "### Review %d (%s)\n\n",
			i+1, status)

		if r.Skipped || r.Status == ResultSkipped {
			reason := r.SkipReason
			if reason == "" {
				reason = "no reason recorded"
			}
			b.WriteString("Review skipped: " + reason + "\n\n")
		} else if IsQuotaFailure(r) {
			b.WriteString(
				"Review skipped: quota exhausted.\n\n")
		} else if IsTransientFailure(r) {
			b.WriteString(
				"Review skipped — provider temporarily unavailable.\n\n")
		} else if r.Status == ResultFailed {
			b.WriteString(
				"**Error:** Review failed. " +
					"Check CI logs for details.\n\n")
		} else if r.Output != "" {
			output := r.Output
			const maxLen = 15000
			if len(output) > maxLen {
				output = output[:maxLen] +
					"\n\n...(truncated)"
			}
			b.WriteString(output)
			b.WriteString("\n\n")
		} else {
			b.WriteString("(no output)\n\n")
		}
	}

	if note := SkippedAgentNote(reviews); note != "" {
		b.WriteString(note)
	}

	return b.String()
}

// FormatAllFailedComment formats a comment when every job in a
// batch failed.
func FormatAllFailedComment(
	reviews []ReviewResult,
	headSHA string,
) string {
	quotaSkips := CountQuotaFailures(reviews)
	timeoutSkips := CountTimeoutCancellations(reviews)
	transientSkips := CountTransientFailures(reviews)
	allSkipped := len(reviews) > 0 &&
		quotaSkips+timeoutSkips+transientSkips == len(reviews)

	var b strings.Builder
	if allSkipped {
		fmt.Fprintf(&b,
			"## roborev: Review Skipped (`%s`)\n\n",
			gitrepo.ShortSHA(headSHA))
		b.WriteString(
			"All review agents were skipped " +
				"due to quota exhaustion, timeout, or provider " +
				"unavailability.\n\n")
	} else {
		fmt.Fprintf(&b,
			"## roborev: Review Failed (`%s`)\n\n",
			gitrepo.ShortSHA(headSHA))
		b.WriteString(
			"All review jobs in this batch failed.\n\n")
	}

	for i, r := range reviews {
		if IsQuotaFailure(r) {
			fmt.Fprintf(&b,
				"- Review %d: skipped (quota)\n",
				i+1)
		} else if IsTimeoutCancellation(r) {
			fmt.Fprintf(&b,
				"- Review %d: skipped (timeout)\n",
				i+1)
		} else if IsTransientFailure(r) {
			fmt.Fprintf(&b,
				"- Review %d: skipped (provider unavailable)\n",
				i+1)
		} else {
			fmt.Fprintf(&b,
				"- Review %d: failed\n",
				i+1)
		}
	}

	if !allSkipped {
		b.WriteString("\nCheck CI logs for error details.")
	}

	if note := SkippedAgentNote(reviews); note != "" {
		b.WriteString(note)
	}

	return b.String()
}

// FormatTransientGiveUpComment is posted after the 3-day transient retry cap.
// It explains that the AI provider was repeatedly unavailable and includes a
// one-line excerpt of the last error encountered.
func FormatTransientGiveUpComment(headSHA, lastErrExcerpt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## roborev: Review Unavailable (`%s`)\n\n", gitrepo.ShortSHA(headSHA))
	b.WriteString("roborev tried to review this PR for 3 days but the AI provider " +
		"was repeatedly unavailable, so no review was produced.\n\n")
	if strings.TrimSpace(lastErrExcerpt) != "" {
		fmt.Fprintf(&b, "Last error: `%s`\n", oneLineExcerpt(lastErrExcerpt))
	}
	return b.String()
}

// FormatGenuineSoftNoteComment is posted after bounded genuine failures. It
// notes the agent repeatedly failed to run and that roborev will retry on the
// next commit, with a one-line excerpt of the last error.
func FormatGenuineSoftNoteComment(headSHA, lastErrExcerpt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## roborev: Review Unavailable (`%s`)\n\n", gitrepo.ShortSHA(headSHA))
	b.WriteString("The review agent repeatedly failed to run (likely an agent or " +
		"configuration error). roborev will try again on the next commit.\n\n")
	if strings.TrimSpace(lastErrExcerpt) != "" {
		fmt.Fprintf(&b, "Last error: `%s`\n", oneLineExcerpt(lastErrExcerpt))
	}
	return b.String()
}

// oneLineExcerpt flattens a message to a single line (newlines to spaces,
// carriage returns dropped) and truncates to 200 bytes for inline display.
func oneLineExcerpt(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	s = strings.TrimSpace(s)
	const max = 200
	if len(s) > max {
		s = strings.TrimRight(TrimPartialRune(s[:max]), " ") + "..."
	}
	return s
}

// IsQuotaFailure returns true if a review's error indicates a
// quota skip rather than a real failure.
func IsQuotaFailure(r ReviewResult) bool {
	return r.Status == ResultFailed &&
		strings.HasPrefix(r.Error, QuotaErrorPrefix)
}

// CountQuotaFailures returns the number of reviews that failed
// due to agent quota exhaustion rather than a real error.
func CountQuotaFailures(reviews []ReviewResult) int {
	n := 0
	for _, r := range reviews {
		if IsQuotaFailure(r) {
			n++
		}
	}
	return n
}

// IsTimeoutCancellation returns true if a review was canceled
// because the batch timed out and posted early.
func IsTimeoutCancellation(r ReviewResult) bool {
	return r.Status == "canceled" &&
		strings.HasPrefix(r.Error, TimeoutErrorPrefix)
}

// CountTimeoutCancellations returns the number of reviews that
// were canceled due to batch timeout.
func CountTimeoutCancellations(reviews []ReviewResult) int {
	n := 0
	for _, r := range reviews {
		if IsTimeoutCancellation(r) {
			n++
		}
	}
	return n
}

// SkippedAgentNote returns a neutral markdown note counting reviews skipped due
// to quota exhaustion. Returns "" if none.
func SkippedAgentNote(reviews []ReviewResult) string {
	skips := 0
	for _, r := range reviews {
		if IsQuotaFailure(r) {
			skips++
		}
	}
	if skips == 0 {
		return ""
	}
	if skips == 1 {
		return fmt.Sprintf(
			"\n*Note: 1 review skipped " +
				"(quota exhausted)*\n")
	}
	return fmt.Sprintf(
		"\n*Note: %d reviews skipped "+
			"(quota exhausted)*\n",
		skips)
}
