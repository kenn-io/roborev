package main

import (
	"bufio"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// maxExpectedFindings caps the number {{expected_findings:R}} states. The
// most findings any single review made in the 25-PR test set was 40, on a
// 13,575-line change; a straight rate there would ask for hundreds.
const maxExpectedFindings = 40

var expectedFindingsRe = regexp.MustCompile(`\{\{expected_findings:([0-9]+(?:\.[0-9]+)?)\}\}`)

// expandInstructions fills the size placeholders in a panel member's
// instructions, so one experiment config can scale its wording with the
// change under review. {{changed_lines}} becomes the number of added and
// removed lines the reviewer sees. {{expected_findings:R}} becomes R
// findings per 100 of those lines, rounded, at least 1 and at most
// maxExpectedFindings.
func expandInstructions(text string, changedLines int) string {
	text = strings.ReplaceAll(text, "{{changed_lines}}", strconv.Itoa(changedLines))
	return expectedFindingsRe.ReplaceAllStringFunc(text, func(m string) string {
		rate, _ := strconv.ParseFloat(expectedFindingsRe.FindStringSubmatch(m)[1], 64)
		n := int(math.Round(float64(changedLines) * rate / 100))
		return strconv.Itoa(min(max(n, 1), maxExpectedFindings))
	})
}

// countChangedLines counts the added and removed lines in a git diff. Only
// lines inside hunks count, so a removed line that reads "-- x" is not
// mistaken for a "--- a/file" header.
func countChangedLines(diff string) int {
	n, inHunk := 0, false
	scanner := bufio.NewScanner(strings.NewReader(diff))
	scanner.Buffer(make([]byte, 0, 64*1024), math.MaxInt32)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")):
			n++
		}
	}
	return n
}
