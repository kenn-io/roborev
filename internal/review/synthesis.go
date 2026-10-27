package review

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"strings"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// SynthesisSchema is the structured review schema with one addition: every
// finding cites the input review numbers that reported it.
var SynthesisSchema = structuredreview.SourcedSchema

// SynthesisDocument is the structured result returned by a synthesis agent.
// It has the same shape as a single structured review, so the verdict derives
// from the findings instead of being asserted separately, and each finding
// keeps the reviews it came from.
type SynthesisDocument = structuredreview.Document

// DecodeSynthesisDocument validates one complete synthesis JSON document
// against the input reviews it combined and attaches reviewer labels so the
// rendered Markdown can say where each finding came from.
func DecodeSynthesisDocument(raw jsontext.Value, reviews []ReviewResult) (SynthesisDocument, error) {
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return SynthesisDocument{}, fmt.Errorf("decode synthesis output: %w", err)
	}
	if err := doc.RequireSources(len(reviews)); err != nil {
		return SynthesisDocument{}, fmt.Errorf("decode synthesis output: %w", err)
	}
	doc.SourceLabels = SynthesisSourceLabels(reviews)
	return doc, nil
}

// SynthesisVerdict is the canonical verdict of a synthesis document: pass when
// no finding reaches the severity threshold, otherwise fail. Findings below
// the threshold stay in the document.
func SynthesisVerdict(doc SynthesisDocument, minSeverity string) storage.Verdict {
	return storage.VerdictFromPassed(doc.Passed(minSeverity))
}

// SynthesisSourceLabels names each input review for readers: the agent, plus
// the review type when it is not the default review. The prompt itself keeps
// reviewers anonymous as "Review N" so the synthesizer weighs findings on
// their merits; the labels are applied only when rendering.
func SynthesisSourceLabels(reviews []ReviewResult) []string {
	labels := make([]string, len(reviews))
	for i, r := range reviews {
		label := strings.TrimSpace(r.Agent)
		if label == "" {
			label = fmt.Sprintf("review %d", i+1)
		}
		if rt := strings.TrimSpace(r.ReviewType); rt != "" && !config.IsDefaultReviewType(rt) {
			label += " (" + rt + ")"
		}
		labels[i] = label
	}
	return labels
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
		"   - Return the verified findings in the review JSON findings array\n" +
		"   - Counts, totals, and summaries may accompany repeated findings, but must not replace them\n" +
		"   - The summary may mention how many prior findings were dropped as fixed, duplicates, or false positives\n\n"
}

// BuildSynthesisPrompt creates the prompt for the synthesis agent. The
// severity threshold is applied to the synthesized output afterwards and is
// never shown to the agent, so every finding survives synthesis. When neutral
// is true (see NeedsNeutralSynthesis), the prompt avoids calling the work code
// and labels each input with its rubric. Empty instructions keep synthesis a
// pure merge of the inputs. Non-empty instructions come from the panel's
// synthesis_instructions: the agent may then read the repository with its
// read-only tools to follow them, and the instructions follow the rules.
func BuildSynthesisPrompt(
	reviews []ReviewResult,
	_ string,
	neutral bool,
	instructions string,
) string {
	intro := "You are combining multiple code review outputs into a single GitHub PR comment.\n"
	material := "the code"
	inspect := "source code"
	rubricRule := ""
	if neutral {
		intro = "You are combining multiple review outputs into a combined review of the reviewed material.\n"
		material = "the reviewed material"
		inspect = "the reviewed material"
		rubricRule = "- Silence from a different rubric is not disagreement; merge only findings describing the same problem\n"
	}
	scopeRules := "- Only combine the input review results according to these rules\n" +
		"- Do not independently review " + inspect + " or inspect the repository\n" +
		"- Do not use tools except to read referenced prompt or review-input files in full\n"
	if instructions != "" {
		scopeRules = "- Combine the input review results according to these rules, then apply the additional synthesis instructions below\n" +
			"- You may read the repository with your read-only tools when the additional instructions call for it; never modify files\n" +
			"- Every finding must come from at least one input review; do not add findings of your own\n"
	}
	var b strings.Builder
	b.WriteString(
		intro +
			"Return exactly one JSON object with this shape and no code fence: " +
			`{"schema_version":3,"summary":"...","verdict":"pass|fail","findings":[{"severity":"critical|high|medium|low","kind":"defect|improvement","topic":"copied from the input finding, or null","confidence":0.8,"problem":"...","fix":"...","file":"path or null","start_line":12,"end_line":14,"sources":[1]}]}` + "\n" +
			"Put a one-line overall summary in `summary` and your overall judgment in `verdict`. " +
			"List every finding that requires changes in `findings`; " +
			"return an empty `findings` array if the reviewers agree " + material + " is clean.\n" +
			"In `sources`, list the numbers of the reviews below (### Review N) " +
			"that reported the finding.\nRules:\n" +
			scopeRules +
			"- Deduplicate findings reported by multiple reviewers and cite every source\n" +
			rubricRule +
			"- Order findings by severity (Critical > High > Medium > Low)\n" +
			"- Keep each finding's kind and topic; when reviewers disagree, use the value most of them gave\n" +
			"- Set confidence to the highest confidence any source gave, or 0.5 when the sources give none\n" +
			"- Keep the file, start_line, and end_line of the findings you combine; use null when there is none\n" +
			"- When an input finding has a location but no file, name that location in the problem\n" +
			"- No preamble about yourself\n")
	if instructions != "" {
		b.WriteString("\n## Additional synthesis instructions\n" + instructions + "\n")
	}

	b.WriteString("\n")

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
		if neutral {
			rubric := r.ReviewType
			if config.IsDefaultReviewType(rubric) {
				rubric = config.ReviewTypeDefault
			}
			fmt.Fprintf(&b, "Rubric: %s\n", rubric)
		}
		b.WriteString(synthesisReviewContent(r))
		b.WriteString("\n\n")
	}

	return b.String()
}

func synthesisReviewContent(r ReviewResult) string {
	var b strings.Builder
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
	} else if r.Structured != nil {
		// The JSON document keeps each finding's kind, confidence, and line
		// range, which the Markdown rendering drops or flattens. It carries
		// no severity threshold, so the synthesis agent never learns which
		// severities are informational.
		member := anchorLegacyLocations(*r.Structured)
		if document, err := json.Marshal(member, jsontext.WithIndentPrefix(""), jsontext.WithIndent("  ")); err == nil {
			b.WriteString("```json\n" + string(document) + "\n```")
		} else {
			b.WriteString(member.Markdown(""))
		}
	} else if r.Output != "" {
		b.WriteString(r.Output)
	} else if r.Status == ResultFailed {
		b.WriteString("(no output — review failed)")
	}
	return b.String()
}

// anchorLegacyLocations gives findings from a version 1 or 2 review a file
// and line range parsed from their free-text location. A panel member that
// finished before a daemon update may still be stored in that form, and the
// synthesis agent copies only file, start_line, and end_line. A location in
// any other form stays as free text. The caller's findings are not changed.
func anchorLegacyLocations(doc StructuredReview) StructuredReview {
	doc.Findings = slices.Clone(doc.Findings)
	for i, finding := range doc.Findings {
		if finding.File != "" || finding.Location == "" {
			continue
		}
		if file, start, end, ok := parseLocation(finding.Location); ok {
			doc.Findings[i].SetLocation(file, start, end)
		}
	}
	return doc
}

// parseLocation reads the "path:12" and "path:12-20" forms that
// [structuredreview.FormatLocation] writes. A bare path is not accepted,
// because free text such as "the retry loop" would read as one.
func parseLocation(location string) (file string, start, end int, ok bool) {
	location = strings.TrimSpace(location)
	colon := strings.LastIndex(location, ":")
	if colon <= 0 || strings.ContainsAny(location[:colon], " \t") {
		return "", 0, 0, false
	}
	file, lines := location[:colon], location[colon+1:]
	first, last, isRange := strings.Cut(lines, "-")
	start, err := strconv.Atoi(first)
	if err != nil || start <= 0 {
		return "", 0, 0, false
	}
	end = start
	if isRange {
		if end, err = strconv.Atoi(last); err != nil || end < start {
			return "", 0, 0, false
		}
	}
	return file, start, end, true
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
	cfg CommentConfig,
	reviews []ReviewResult,
	headSHA string,
) string {
	return formatRawBatchOutput(reviews, headSHA, &cfg)
}

func formatRawBatchOutput(reviews []ReviewResult, headSHA string, cfg *CommentConfig) string {
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
			if cfg != nil {
				b.WriteString(FormatComment(PrepareComment(*cfg, r)))
			} else {
				b.WriteString(r.Output)
			}
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
	emptyOutputSkips := 0
	for _, r := range reviews {
		if r.Status == ResultDone && !IsSubstantiveOutput(r) {
			emptyOutputSkips++
		}
	}
	allSkipped := len(reviews) > 0 &&
		quotaSkips+timeoutSkips+transientSkips+emptyOutputSkips == len(reviews)

	var b strings.Builder
	if allSkipped {
		fmt.Fprintf(&b,
			"## roborev: Review Skipped (`%s`)\n\n",
			gitrepo.ShortSHA(headSHA))
		if emptyOutputSkips == 0 {
			b.WriteString(
				"All review agents were skipped " +
					"due to quota exhaustion, timeout, or provider " +
					"unavailability.\n\n")
		} else {
			b.WriteString(
				"No review output was produced; every review was skipped " +
					"or completed without output.\n\n")
		}
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
		} else if r.Status == ResultDone && !IsSubstantiveOutput(r) {
			fmt.Fprintf(&b,
				"- Review %d: skipped (no output)\n",
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

// NeedsNeutralSynthesis reports whether any voting input uses a custom
// rubric. Pass the complete voting set before failed or skipped outputs are
// filtered, so a failed custom voter still selects neutral wording. Non-voting
// observers must not be included.
func NeedsNeutralSynthesis(reviews []ReviewResult) bool {
	for _, r := range reviews {
		if !config.IsBuiltInReviewType(r.ReviewType) {
			return true
		}
	}
	return false
}
