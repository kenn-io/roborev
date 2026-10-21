package main

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// The ids synthesis modes give every member finding an ID such as R2.3 (review
// 2, finding 3). The agent prefixes each finding it returns with the IDs it
// combines, and code puts back every ID the agent left out, so a member
// finding can be lost only by a wrong merge, never by omission.

type findingID struct{ review, finding int }

func (id findingID) String() string { return fmt.Sprintf("R%d.%d", id.review, id.finding) }

var idPrefix = regexp.MustCompile(`^\s*\[\s*(R\d+\.\d+(?:\s*,\s*R\d+\.\d+)*)\s*\]\s*`)
var idPattern = regexp.MustCompile(`R(\d+)\.(\d+)`)

// renderWithIDs lists every member finding under its ID.
func renderWithIDs(reviews []review.ReviewResult) string {
	var b strings.Builder
	for i, r := range reviews {
		fmt.Fprintf(&b, "---\n### Review %d\n", i+1)
		if r.Structured == nil || len(r.Structured.Findings) == 0 {
			b.WriteString("No findings.\n\n")
			continue
		}
		for j, f := range r.Structured.Findings {
			id := findingID{i + 1, j + 1}
			fmt.Fprintf(&b, "%s [%s", id, f.Severity)
			if f.Kind != "" {
				fmt.Fprintf(&b, ", %s", f.Kind)
			}
			b.WriteString("]")
			if loc := structuredreview.FormatLocation(f.File, f.StartLine, f.EndLine); loc != "" {
				fmt.Fprintf(&b, " %s", loc)
			}
			fmt.Fprintf(&b, "\nProblem: %s\nFix: %s\n\n", f.Problem, f.Fix)
		}
	}
	return b.String()
}

// accountForIDs maps the agent's findings back to member findings. With
// verbatim set, each group is replaced by its best member finding, so no
// wording is rewritten; otherwise the agent's wording is kept. Member findings
// that no group cites are appended unchanged. Agent findings that cite no
// valid ID are dropped, because they did not come from any member.
func accountForIDs(doc structuredreview.Document, reviews []review.ReviewResult, verbatim bool) (structuredreview.Document, int, int) {
	member := func(id findingID) (structuredreview.Finding, bool) {
		if id.review < 1 || id.review > len(reviews) || reviews[id.review-1].Structured == nil {
			return structuredreview.Finding{}, false
		}
		findings := reviews[id.review-1].Structured.Findings
		if id.finding < 1 || id.finding > len(findings) {
			return structuredreview.Finding{}, false
		}
		return findings[id.finding-1], true
	}

	cited := map[findingID]bool{}
	out := doc
	out.Findings = nil
	invented := 0
	for _, f := range doc.Findings {
		m := idPrefix.FindStringSubmatch(f.Problem)
		var ids []findingID
		if m != nil {
			for _, g := range idPattern.FindAllStringSubmatch(m[1], -1) {
				r, _ := strconv.Atoi(g[1])
				n, _ := strconv.Atoi(g[2])
				id := findingID{r, n}
				if _, ok := member(id); ok && !cited[id] {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) == 0 {
			invented++
			continue
		}
		var sources []int
		group := make([]structuredreview.Finding, 0, len(ids))
		for _, id := range ids {
			cited[id] = true
			mf, _ := member(id)
			group = append(group, mf)
			if !slices.Contains(sources, id.review) {
				sources = append(sources, id.review)
			}
		}
		slices.Sort(sources)
		if verbatim {
			f = best(group)
		} else {
			f.Problem = strings.TrimSpace(f.Problem[len(m[0]):])
		}
		f.Severity = highest(group)
		f.Sources = sources
		out.Findings = append(out.Findings, f)
	}

	restored := 0
	for i, r := range reviews {
		if r.Structured == nil {
			continue
		}
		for j, f := range r.Structured.Findings {
			if cited[findingID{i + 1, j + 1}] {
				continue
			}
			f.Sources = []int{i + 1}
			out.Findings = append(out.Findings, f)
			restored++
		}
	}
	return out, restored, invented
}

var severityOrder = []string{"low", "medium", "high", "critical"}

func highest(group []structuredreview.Finding) string {
	top := group[0].Severity
	for _, f := range group[1:] {
		if slices.Index(severityOrder, f.Severity) > slices.Index(severityOrder, top) {
			top = f.Severity
		}
	}
	return top
}

// best picks the finding a group keeps, the way RevEngine does: highest
// severity, then highest confidence, then the most detail.
func best(group []structuredreview.Finding) structuredreview.Finding {
	return slices.MaxFunc(group, func(a, b structuredreview.Finding) int {
		if c := slices.Index(severityOrder, a.Severity) - slices.Index(severityOrder, b.Severity); c != 0 {
			return c
		}
		if a.Confidence != b.Confidence {
			if a.Confidence < b.Confidence {
				return -1
			}
			return 1
		}
		return len(a.Problem) + len(a.Fix) - len(b.Problem) - len(b.Fix)
	})
}
