package storage

import (
	"database/sql"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

const (
	verdictPass = "P"
	verdictFail = "F"
)

// verdictToBool converts a ParseVerdict result ("P"/"F") to an integer
// for storage in the verdict_bool column (1=pass, 0=fail).
func verdictToBool(verdict string) int {
	if verdict == verdictPass {
		return 1
	}
	return 0
}

// verdictFromBoolOrParse returns the verdict string from a stored verdict_bool
// value. If the value is NULL (legacy row), falls back to ParseVerdict(output).
func verdictFromBoolOrParse(vb sql.NullInt64, output string) string {
	if vb.Valid {
		if vb.Int64 == 1 {
			return verdictPass
		}
		return verdictFail
	}
	return ParseVerdict(output)
}

func applyReviewVerdict(review *Review, verdictBool sql.NullInt64) {
	if verdictBool.Valid {
		v := int(verdictBool.Int64)
		review.VerdictBool = &v
	}
}

func applyJobVerdict(job *ReviewJob, verdictBool sql.NullInt64, output string) {
	if output == "" || job.Error != "" || job.IsTaskJob() {
		return
	}
	verdict := verdictFromBoolOrParse(verdictBool, output)
	job.Verdict = &verdict
}

// ParseVerdict extracts P (pass) or F (fail) from review output.
// It intentionally uses a small set of deterministic signals:
// clear severity/findings markers mean fail, and clear pass phrases mean pass.
// We do not try to interpret narrative caveats after "No issues found." because
// that quickly turns into a brittle natural-language parser. If agent output is
// too chatty or mixes process narration with findings, that should be fixed in
// the review prompt rather than by adding more verdict heuristics here.
func ParseVerdict(output string) string {
	// First check for severity labels which indicate actual findings
	// These appear as "- Medium —", "* Low:", "Critical -", etc.
	if hasSeverityLabel(output) {
		return verdictFail
	}

	// Marker signals pass ONLY when it stands alone. A loose
	// substring check would let prose findings without severity
	// labels (e.g. "the auth module leaks tokens") flip to pass
	// just because the agent echoed the marker in narration.
	if config.IsMarkerOnlyOutput(output) {
		return verdictPass
	}

	for line := range strings.SplitSeq(output, "\n") {
		normalized := normalizeVerdictLine(line)
		// Historical reviews sometimes include a stale "Verdict: Fail" header
		// before a later "No issues found." summary. Preserve the existing rule
		// that a later clear pass phrase wins over contradictory narration.
		if isExplicitVerdictValue(normalized, "pass") {
			return verdictPass
		}
		if isNoFindingVerdictLine(normalized) {
			return verdictPass
		}
		if !hasPassPrefix(normalized) {
			continue
		}
		return verdictPass
	}
	return verdictFail
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

// hasSeverityLabel checks if the output contains severity labels indicating findings.
// Matches patterns like "- Medium —", "* Low:", "Critical — issue", etc.
// Checks lines that start with bullets/numbers OR directly with severity words.
// Requires separators to be followed by space to avoid "High-level overview".
// Skips lines that appear to be part of a severity legend/rubric.
func hasSeverityLabel(output string) bool {
	lc := strings.ToLower(output)
	severities := []string{"critical", "high", "medium", "low"}
	lines := strings.Split(lc, "\n")

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}

		// Check if line starts with bullet/number - if so, strip it
		first := trimmed[0]
		hasBullet := first == '-' || first == '*' || (first >= '0' && first <= '9') ||
			strings.HasPrefix(trimmed, "•")

		checkText := trimmed
		if hasBullet {
			// Strip leading bullets/asterisks/numbers
			checkText = strings.TrimLeft(trimmed, "-*•0123456789.) ")
			checkText = strings.TrimSpace(checkText)
		}

		// Strip markdown formatting (bold, headers) before checking
		checkText = stripMarkdown(checkText)

		// Check if text starts with a severity word
		for _, sev := range severities {
			if !strings.HasPrefix(checkText, sev) {
				continue
			}

			// Check if followed by separator (dash, em-dash, colon, pipe)
			rest := checkText[len(sev):]
			rest = strings.TrimSpace(rest)
			if len(rest) == 0 {
				continue
			}

			// Check for valid separator
			hasValidSep := false
			// Check for em-dash or en-dash (these are unambiguous)
			if strings.HasPrefix(rest, "—") || strings.HasPrefix(rest, "–") {
				hasValidSep = true
			}
			// Check for colon or pipe (unambiguous separators)
			if rest[0] == ':' || rest[0] == '|' {
				hasValidSep = true
			}
			// For hyphen, require space after to avoid "High-level"
			if rest[0] == '-' && len(rest) > 1 && rest[1] == ' ' {
				hasValidSep = true
			}

			if !hasValidSep {
				continue
			}

			// Skip if this looks like a legend/rubric entry
			// Check if previous non-empty line is a legend header
			if isLegendEntry(lines, i) {
				continue
			}

			return true
		}

		// Check for "severity: <level>" pattern (e.g., "**Severity**: High")
		if strings.HasPrefix(checkText, "severity") {
			rest := checkText[len("severity"):]
			rest = strings.TrimSpace(rest)
			hasSep := len(rest) > 0 && (rest[0] == ':' || rest[0] == '|' ||
				strings.HasPrefix(rest, "—") || strings.HasPrefix(rest, "–"))
			// Accept hyphen-minus when followed by space (mirrors the severity-word branch)
			if !hasSep && len(rest) > 1 && rest[0] == '-' && rest[1] == ' ' {
				hasSep = true
			}
			if hasSep {
				// Skip separator and whitespace
				rest = strings.TrimLeft(rest, ":-–—| ")
				rest = strings.TrimSpace(rest)
				for _, sev := range severities {
					if strings.HasPrefix(rest, sev) {
						if !isLegendEntry(lines, i) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// isLegendEntry checks if a line at index i appears to be part of a severity legend/rubric
// by looking at preceding lines for legend indicators. Scans up to 10 lines back,
// skipping empty lines, severity lines, and description lines that may appear
// between legend entries.
func isLegendEntry(lines []string, i int) bool {
	for j := i - 1; j >= 0 && j >= i-10; j-- {
		prev := strings.TrimSpace(lines[j])
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
