package storage

import (
	"database/sql"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// Verdict is the canonical pass/fail result of a completed review. The empty
// value means no verdict is available, for example when a review produced no
// output.
type Verdict string

const (
	VerdictUnknown Verdict = ""
	VerdictPass    Verdict = "P"
	VerdictFail    Verdict = "F"
)

// VerdictFromPassed converts a structured pass/fail result into a Verdict.
func VerdictFromPassed(passed bool) Verdict {
	if passed {
		return VerdictPass
	}
	return VerdictFail
}

// Passed reports whether the verdict is an explicit pass.
func (v Verdict) Passed() bool {
	return v == VerdictPass
}

// verdictToBool converts a ParseVerdict result ("P"/"F") to an integer
// for storage in the verdict_bool column (1=pass, 0=fail).
func verdictToBool(verdict Verdict) int {
	if verdict == VerdictPass {
		return 1
	}
	return 0
}

// verdictFromBoolOrParse returns the verdict string from a stored verdict_bool
// value. If the value is NULL (legacy row), falls back to ParseVerdict(output).
func verdictFromBoolOrParse(vb sql.NullInt64, output string) Verdict {
	if vb.Valid {
		if vb.Int64 == 1 {
			return VerdictPass
		}
		return VerdictFail
	}
	return ParseVerdict(output)
}

func applyReviewVerdict(review *Review, verdictBool sql.NullInt64) {
	if verdictBool.Valid {
		v := int(verdictBool.Int64)
		review.VerdictBool = &v
	}
}

// Verdict returns the review's stored pass/fail result. Reviews created before
// verdict_bool was populated retain the existing Markdown fallback.
func (r Review) Verdict() Verdict {
	if r.VerdictBool != nil {
		if *r.VerdictBool == 1 {
			return VerdictPass
		}
		return VerdictFail
	}
	return ParseVerdict(r.Output)
}

// applyJobVerdict derives the job's verdict from the stored verdict_bool,
// falling back to parsing the review output. hasReview reports whether a
// non-empty review output exists; callers that skip hydrating the output for
// rows with a stored verdict pass the existence flag from SQL instead.
func applyJobVerdict(job *ReviewJob, verdictBool sql.NullInt64, output string, hasReview bool) {
	if !hasReview || job.Error != "" || job.IsTaskJob() {
		return
	}
	verdict := verdictFromBoolOrParse(verdictBool, output)
	if verdict == VerdictUnknown {
		return
	}
	value := string(verdict)
	job.Verdict = &value
}

// OutputKind classifies agent output before any verdict is parsed from it.
// Only OutputReviewed carries a verdict; the other kinds mean no review
// happened, whatever the text says afterwards.
type OutputKind int

const (
	// OutputReviewed is ordinary review text; parse it for a verdict.
	OutputReviewed OutputKind = iota
	// OutputEmpty means the agent produced nothing: blank output, or the fixed
	// placeholder every adapter returns when the process printed nothing.
	OutputEmpty
	// OutputUnreadableInput means the agent said it could not read its diff.
	OutputUnreadableInput
)

func (k OutputKind) String() string {
	switch k {
	case OutputEmpty:
		return "empty output"
	case OutputUnreadableInput:
		return "unreadable input"
	default:
		return "reviewed"
	}
}

// NoReviewOutputPlaceholder is the text agent adapters return when the agent
// process printed nothing at all.
const NoReviewOutputPlaceholder = "No review output generated"

// unreadableInputPhrases are deterministic signals that the agent never saw
// the diff it was asked to review. They win over any pass phrase that
// follows, such as a reflexive "No issues found."
var unreadableInputPhrases = []string{
	"unable to read the diff",
	"unable to access the diff",
	"cannot read the diff",
	"can't read the diff",
	"could not read the diff",
	"couldn't read the diff",
	"failed to read the diff",
	"no diff was provided",
	"ignored by configured ignore patterns",
}

// ClassifyOutput is the single place that decides whether agent output is a
// review at all. Every path that turns output into a verdict, whether fresh
// from an agent or already stored, asks this function.
func ClassifyOutput(output string) OutputKind {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" || trimmed == NoReviewOutputPlaceholder {
		return OutputEmpty
	}
	// A severity-labelled finding is a real review even if the agent also
	// says part of the input was unreadable.
	if HighestSeverityLabel(trimmed) != "" {
		return OutputReviewed
	}
	lower := strings.ReplaceAll(strings.ToLower(trimmed), "\u2019", "'")
	for _, phrase := range unreadableInputPhrases {
		if strings.Contains(lower, phrase) {
			return OutputUnreadableInput
		}
	}
	return OutputReviewed
}

// NoReviewLikeClauses returns SQL predicates that prefilter review rows whose
// output may classify as anything other than OutputReviewed. The caller ORs
// them and binds args in order; ClassifyOutput makes the final call.
func NoReviewLikeClauses(column string) (string, []any) {
	phrases := append([]string{strings.ToLower(NoReviewOutputPlaceholder)}, unreadableInputPhrases...)
	clauses := make([]string, 0, len(phrases))
	args := make([]any, 0, len(phrases))
	for _, phrase := range phrases {
		clauses = append(clauses, "lower("+column+") LIKE '%' || ? || '%'")
		args = append(args, phrase)
	}
	return strings.Join(clauses, " OR "), args
}

// isFreeFormJobType reports whether a job's output is free-form prose that
// never carries a verdict (task and insights jobs).
func isFreeFormJobType(jobType string) bool {
	return jobType == JobTypeTask || jobType == JobTypeInsights
}

// verdictBoolFromOutput returns the verdict_bool column value for review
// output: 1 or 0 for a parsed verdict, nil (SQL NULL) when the output carries
// no verdict.
func verdictBoolFromOutput(output string) any {
	verdict := ParseVerdict(output)
	if verdict == VerdictUnknown {
		return nil
	}
	return verdictToBool(verdict)
}

// ParseVerdict extracts P (pass) or F (fail) from review output. Output that
// ClassifyOutput does not consider a review returns VerdictUnknown so the
// caller can treat the job as never reviewed instead of clean or failed.
// It intentionally uses a small set of deterministic signals:
// clear severity/findings markers mean fail, and clear pass phrases mean pass.
// Anything else defaults to fail so prose findings without labels are kept.
// We do not try to interpret narrative caveats after "No issues found." because
// that quickly turns into a brittle natural-language parser. If agent output is
// too chatty or mixes process narration with findings, that should be fixed in
// the review prompt rather than by adding more verdict heuristics here.
func ParseVerdict(output string) Verdict {
	return ParseVerdictAtSeverity(output, "")
}

// ParseVerdictAtSeverity is ParseVerdict with a minimum severity. Labeled
// findings at or above minSeverity fail the review. When every labeled
// finding is below the threshold the review passes: the findings stay in the
// output as information, they just do not count against it. Output without
// severity labels falls back to the agent's own pass/fail statement. Empty,
// "low", and unknown thresholds count every labeled finding.
func ParseVerdictAtSeverity(output, minSeverity string) Verdict {
	if ClassifyOutput(output) != OutputReviewed {
		return VerdictUnknown
	}

	// Severity labels indicate actual findings. They appear as
	// "- Medium —", "* Low:", "Critical -", etc.
	if highest := HighestSeverityLabel(output); highest != "" {
		threshold := max(config.SeverityRank(minSeverity), config.SeverityRank("low"))
		if config.SeverityRank(highest) >= threshold {
			return VerdictFail
		}
		return VerdictPass
	}

	// Marker signals pass ONLY when it stands alone. A loose
	// substring check would let prose findings without severity
	// labels (e.g. "the auth module leaks tokens") flip to pass
	// just because the agent echoed the marker in narration.
	if config.IsMarkerOnlyOutput(output) {
		return VerdictPass
	}

	for line := range strings.SplitSeq(output, "\n") {
		normalized := normalizeVerdictLine(line)
		// Historical reviews sometimes include a stale "Verdict: Fail" header
		// before a later "No issues found." summary. Preserve the existing rule
		// that a later clear pass phrase wins over contradictory narration.
		if isExplicitVerdictValue(normalized, "pass") {
			return VerdictPass
		}
		if isNoFindingVerdictLine(normalized) {
			return VerdictPass
		}
		if hasPassPrefix(normalized) {
			return VerdictPass
		}
	}
	return VerdictFail
}

func normalizeVerdictLine(line string) string {
	normalized := strings.TrimSpace(strings.ToLower(line))
	// Normalize curly apostrophes to straight apostrophes (LLMs sometimes use these)
	normalized = strings.ReplaceAll(normalized, "\u2018", "'") // left single quote
	normalized = strings.ReplaceAll(normalized, "\u2019", "'") // right single quote
	normalized = stripMarkdown(normalized)
	normalized = stripListMarker(normalized)
	return stripFieldLabel(normalized)
}

func hasPassPrefix(line string) bool {
	passPrefixes := []string{
		"code review passed:",
		"no issues",
		"no findings",
		"i didn't find any issues",
		"i did not find any issues",
		"i found no issues",
	}
	for _, prefix := range passPrefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func isNoFindingVerdictLine(line string) bool {
	line = strings.TrimRight(line, ".!?")
	line = strings.Join(strings.Fields(line), " ")
	switch line {
	case "all previous findings have been addressed",
		"all findings have been resolved",
		"no verified findings remain",
		"no findings remain",
		"no remaining findings",
		"0 findings",
		"0 findings remain",
		"0 verified findings",
		"0 verified findings remain",
		"zero findings",
		"zero findings remain",
		"zero verified findings",
		"zero verified findings remain":
		return true
	default:
		return false
	}
}

func isExplicitVerdictValue(line, value string) bool {
	return line == value
}

// stripMarkdown removes common markdown formatting from a line
func stripMarkdown(s string) string {
	// Strip leading markdown headers (##, ###, etc.)
	for strings.HasPrefix(s, "#") {
		s = strings.TrimPrefix(s, "#")
	}
	s = strings.TrimSpace(s)

	// Strip bold/italic markers (**, __, *, _)
	// Handle ** and __ first (bold), then * and _ (italic)
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	// Don't strip single * or _ as they might be intentional (e.g., bullet points handled separately)

	return strings.TrimSpace(s)
}

// stripListMarker removes leading bullet/number markers from a line
func stripListMarker(s string) string {
	// Handle: "- ", "* ", "1. ", "99) ", "100. ", etc.
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return s
	}
	// Check for bullet markers
	if s[0] == '-' || s[0] == '*' {
		return strings.TrimSpace(s[1:])
	}
	// Check for numbered lists - scan all leading digits
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			continue
		}
		if i > 0 && (s[i] == '.' || s[i] == ')' || s[i] == ':') {
			return strings.TrimSpace(s[i+1:])
		}
		break
	}
	return s
}

// stripFieldLabel removes a known leading field label from structured review output.
// Handles "Review Findings: No issues found." and similar patterns.
func stripFieldLabel(s string) string {
	labels := []string{
		"review findings",
		"findings",
		"review result",
		"result",
		"verdict",
		"review",
	}
	for _, label := range labels {
		if strings.HasPrefix(s, label) {
			rest := s[len(label):]
			if len(rest) > 0 && rest[0] == ':' {
				return strings.TrimSpace(rest[1:])
			}
		}
	}
	return s
}

// HighestSeverityLabel returns the highest severity label found in prose
// review output, or "" when the output has no severity-labeled findings.
// Matches patterns like "- Medium —", "* Low:", "Critical — issue", etc.
// Checks lines that start with bullets/numbers OR directly with severity words.
// Requires separators to be followed by space to avoid "High-level overview".
// Skips lines that appear to be part of a severity legend/rubric.
func HighestSeverityLabel(output string) string {
	lines := strings.Split(output, "\n")
	highest := ""
	for i := range lines {
		severity, legend := SeverityLabelAt(lines, i)
		if !legend && config.SeverityRank(severity) > config.SeverityRank(highest) {
			highest = severity
		}
	}
	return highest
}

// SeverityLabelAt classifies one line with its surrounding prose context.
// A legend entry has a severity label but is not a finding. Callers provide
// the whole section and a valid line index so rubric context is preserved.
func SeverityLabelAt(lines []string, i int) (severity string, legend bool) {
	trimmed := strings.TrimSpace(strings.ToLower(lines[i]))
	if len(trimmed) == 0 {
		return "", false
	}
	severities := []string{"critical", "high", "medium", "low"}
	first := trimmed[0]
	hasBullet := first == '-' || first == '*' || (first >= '0' && first <= '9') ||
		strings.HasPrefix(trimmed, "•")
	checkText := trimmed
	if hasBullet {
		checkText = strings.TrimSpace(strings.TrimLeft(trimmed, "-*•0123456789.) "))
	}
	checkText = stripMarkdown(checkText)

	// Structured prose headings contain only a numbered severity label.
	if first == '#' {
		heading := stripListMarker(checkText)
		if slices.Contains(severities, heading) {
			return heading, isLegendEntry(lines, i)
		}
	}
	for _, sev := range severities {
		if !strings.HasPrefix(checkText, sev) {
			continue
		}
		rest := strings.TrimSpace(checkText[len(sev):])
		if len(rest) == 0 {
			continue
		}
		// A hyphen requires a following space to exclude "High-level".
		if strings.HasPrefix(rest, "—") || strings.HasPrefix(rest, "–") ||
			rest[0] == ':' || rest[0] == '|' || strings.HasPrefix(rest, "- ") {
			return sev, isLegendEntry(lines, i)
		}
	}
	if strings.HasPrefix(checkText, "severity") {
		rest := strings.TrimSpace(checkText[len("severity"):])
		hasSep := len(rest) > 0 && (rest[0] == ':' || rest[0] == '|' ||
			strings.HasPrefix(rest, "—") || strings.HasPrefix(rest, "–") || strings.HasPrefix(rest, "- "))
		if hasSep {
			rest = strings.TrimSpace(strings.TrimLeft(rest, ":-–—| "))
			for _, sev := range severities {
				if strings.HasPrefix(rest, sev) {
					return sev, isLegendEntry(lines, i)
				}
			}
		}
	}
	return "", false
}

// ProseSection identifies explicit review section boundaries. The return
// value is "summary", "findings", "separator", or empty for ordinary prose.
// Verdict parsing and comment preparation use the same boundaries.
func ProseSection(line string) string {
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "---" {
		return "separator"
	}
	line = stripListMarker(stripMarkdown(line))
	if line == "summary" || strings.HasPrefix(line, "summary:") {
		return "summary"
	}
	switch line {
	case "findings", "findings:", "review findings", "review findings:":
		return "findings"
	default:
		return ""
	}
}

// isLegendEntry checks if a line at index i appears to be part of a severity legend/rubric
// by looking at preceding lines for legend indicators. Scans up to 10 lines back,
// skipping empty lines, severity lines, and description lines that may appear
// between legend entries.
func isLegendEntry(lines []string, i int) bool {
	for j := i - 1; j >= 0 && j >= i-10; j-- {
		if ProseSection(lines[j]) != "" {
			return false
		}
		prev := strings.TrimSpace(strings.ToLower(lines[j]))
		if len(prev) == 0 {
			continue
		}

		// Strip markdown and list markers so bolded headers like
		// "**Severity levels:**" are recognized the same as plain text.
		prev = stripMarkdown(stripListMarker(prev))

		// Check for legend header patterns (ends with ":" and contains indicator word)
		if strings.HasSuffix(prev, ":") || strings.HasSuffix(prev, "：") {
			if strings.Contains(prev, "severity") ||
				strings.Contains(prev, "level") ||
				strings.Contains(prev, "legend") ||
				strings.Contains(prev, "priority") ||
				strings.Contains(prev, "rubric") ||
				strings.Contains(prev, "rating") ||
				strings.Contains(prev, "scale") {
				return true
			}
		}
	}
	return false
}
