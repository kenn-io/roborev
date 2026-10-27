package prompt

import (
	"bufio"
	"fmt"
	"math"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// exhaustiveFindingsPer100Lines sets how many findings an exhaustive review is
// told to expect per 100 changed lines. On ReviewBench's 25-PR test set, six
// and twelve per 100 found about the same share of known issues (33.0% and
// 33.2%), and six kept precision higher (98.1% and 96.9%).
const exhaustiveFindingsPer100Lines = 6

// maxExpectedFindings caps the finding count an exhaustive review is told to
// expect. The most findings any single review made on ReviewBench's 25-PR
// test set was 40, on a 13,575-line change; a straight rate there would ask
// for hundreds.
const maxExpectedFindings = 40

// exhaustiveInstruction returns the section that turns a default review into
// an exhaustive one, or "" for any other review type. It goes after the diff
// and sizes its expectation by the diff the reviewer sees.
func exhaustiveInstruction(reviewType, diff string) string {
	if reviewType != config.ReviewTypeExhaustive {
		return ""
	}
	lines := countChangedLines(diff)
	return fmt.Sprintf(`

## Exhaustive review

Be exhaustive: report every problem you can support, not only the most serious
few. Low-severity findings are wanted when they are concrete and a maintainer
would act on them. This change has %d changed lines. A thorough review
of a change this size usually has %d or more findings spread across
several kinds of problem; a clean change may have fewer.
`, lines, expectedFindings(lines))
}

// expectedFindings is exhaustiveFindingsPer100Lines findings per 100 changed
// lines, rounded, at least 1 and at most maxExpectedFindings.
func expectedFindings(changedLines int) int {
	n := int(math.Round(float64(changedLines) * exhaustiveFindingsPer100Lines / 100))
	return min(max(n, 1), maxExpectedFindings)
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
