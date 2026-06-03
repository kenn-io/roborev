package review

import (
	"fmt"
	"sort"
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

// VerifyDedupePreamble returns the static instruction block shared by the CLI
// compact command and the panel synthesis worker: verify each finding against
// the current codebase, consolidate duplicates, and emit only verified
// findings. The text is byte-preserved so both call sites emit identical
// instructions.
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
		"   - List only VERIFIED findings in your output\n" +
		"   - Use the same severity levels (Critical, High, Medium, Low)\n" +
		"   - Include file and line references where possible\n" +
		"   - Explain what the issue is and why it matters\n\n"
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
			"- Deduplicate findings reported by multiple agents\n" +
			"- Organize by severity (Critical > High > Medium > Low)\n" +
			"- Preserve file/line references\n" +
			"- If all agents agree code is clean, say so concisely\n" +
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
		fmt.Fprintf(&b,
			"---\n### Review %d: Agent=%s, Type=%s",
			i+1, r.Agent, r.ReviewType)
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
			b.WriteString("Auto-design-review skipped: " + reason)
		} else if IsQuotaFailure(r) {
			b.WriteString(
				"(review skipped — agent quota exhausted)")
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

	agentSet := make(map[string]struct{})
	typeSet := make(map[string]struct{})
	for _, r := range reviews {
		if r.Agent != "" {
			agentSet[r.Agent] = struct{}{}
		}
		if r.ReviewType != "" {
			typeSet[r.ReviewType] = struct{}{}
		}
	}
	agents := sortedKeys(agentSet)
	types := sortedKeys(typeSet)

	fmt.Fprintf(&b,
		"\n\n---\n*Synthesized from %d reviews "+
			"(agents: %s | types: %s)*\n",
		len(reviews),
		strings.Join(agents, ", "),
		strings.Join(types, ", "))

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
			status = "skipped (auto-design)"
		}
		fmt.Fprintf(&b, "### %s — %s (%s)\n\n",
			r.Agent, r.ReviewType, status)

		if r.Skipped || r.Status == ResultSkipped {
			reason := r.SkipReason
			if reason == "" {
				reason = "no reason recorded"
			}
			b.WriteString("Auto-design-review skipped: " + reason + "\n\n")
		} else if IsQuotaFailure(r) {
			b.WriteString(
				"Review skipped — agent quota exhausted.\n\n")
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

	for _, r := range reviews {
		if IsQuotaFailure(r) {
			fmt.Fprintf(&b,
				"- **%s** (%s): skipped (quota)\n",
				r.Agent, r.ReviewType)
		} else if IsTimeoutCancellation(r) {
			fmt.Fprintf(&b,
				"- **%s** (%s): skipped (timeout)\n",
				r.Agent, r.ReviewType)
		} else if IsTransientFailure(r) {
			fmt.Fprintf(&b,
				"- **%s** (%s): skipped (provider unavailable)\n",
				r.Agent, r.ReviewType)
		} else {
			fmt.Fprintf(&b,
				"- **%s** (%s): failed\n",
				r.Agent, r.ReviewType)
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

// SkippedAgentNote returns a markdown note listing agents that
// were skipped due to quota exhaustion. Returns "" if none.
func SkippedAgentNote(reviews []ReviewResult) string {
	agents := make(map[string]struct{})
	for _, r := range reviews {
		if IsQuotaFailure(r) {
			agents[r.Agent] = struct{}{}
		}
	}
	if len(agents) == 0 {
		return ""
	}
	names := sortedKeys(agents)
	if len(names) == 1 {
		return fmt.Sprintf(
			"\n*Note: %s review skipped "+
				"(agent quota exhausted)*\n",
			names[0])
	}
	return fmt.Sprintf(
		"\n*Note: %s reviews skipped "+
			"(agent quota exhausted)*\n",
		strings.Join(names, ", "))
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
