package tui

import (
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/roborev/internal/storage"
)

// queueRow is one visible line in the flattened queue tree. The tree is exactly
// one level deep: synthesis (parent) rows at depth 0, their member rows at depth
// 1 when the panel is expanded. Mirrors kata's queue_rows.go:28 queueRow.
type queueRow struct {
	job         storage.ReviewJob
	depth       int  // 0 = parent/standalone, 1 = panel member
	hasChildren bool // true only for a synthesis parent with >=1 member
	expanded    bool // parent is expanded (members shown)
	lastChild   bool // last member of its run (└─ vs ├─)
}

// panelHasChildren reports whether a row is an expandable panel parent.
func panelHasChildren(j storage.ReviewJob) bool {
	return j.IsSynthesisJob() && j.PanelSummary != nil && j.PanelSummary.MembersTotal > 0
}

// flattenQueueRows derives the visible rows from the parents-only job list, the
// set of expanded panel run uuids, and the side-fetched members per run. Pure:
// no model/render dependency. A nil members map (fetch not yet returned) yields
// the parent with no member rows. Mirrors kata appendNode (queue_rows.go:182)
// simplified to one level.
func flattenQueueRows(
	parents []storage.ReviewJob,
	expanded map[string]bool,
	members map[string][]storage.ReviewJob,
) []queueRow {
	rows := make([]queueRow, 0, len(parents))
	for _, p := range parents {
		hasKids := panelHasChildren(p)
		isOpen := hasKids && expanded[p.PanelRunUUID]
		rows = append(rows, queueRow{job: p, depth: 0, hasChildren: hasKids, expanded: isOpen})
		if !isOpen {
			continue
		}
		kids := append([]storage.ReviewJob(nil), members[p.PanelRunUUID]...)
		sort.SliceStable(kids, func(i, j int) bool {
			return kids[i].PanelMemberIndex < kids[j].PanelMemberIndex
		})
		for i, k := range kids {
			rows = append(rows, queueRow{job: k, depth: 1, lastChild: i == len(kids)-1})
		}
	}
	return rows
}

// disclosureGlyph returns the leading expand/collapse indicator for a parent
// row: ▾ open / ▸ collapsed in color mode, - / + in ASCII (color=false), and a
// single space for a row without children. Mirrors kata list_render.go:1022.
func disclosureGlyph(hasChildren, expanded, color bool) string {
	if !hasChildren {
		return " "
	}
	if !color {
		if expanded {
			return "-"
		}
		return "+"
	}
	if expanded {
		return "▾"
	}
	return "▸"
}

// childConnector returns the tree connector for a member row: ├─ for a middle
// member and └─ for the last, with ASCII fallbacks (+- / \-) when color=false.
// Mirrors kata list_render.go:968 childGuide simplified to one level.
func childConnector(lastChild, color bool) string {
	if !color {
		if lastChild {
			return `\-`
		}
		return "+-"
	}
	if lastChild {
		return "└─"
	}
	return "├─"
}

// groupBanding flips the band flag at each depth-0 row so a parent and its
// members share one zebra band. Mirrors kata groupBanding.
func groupBanding(rows []queueRow) []bool {
	bands := make([]bool, len(rows))
	alt := true
	for i, r := range rows {
		if r.depth == 0 {
			alt = !alt
		}
		bands[i] = alt
	}
	return bands
}

// panelInProgress reports whether a panel parent is still completing (members
// running or synthesis not yet terminal).
func panelInProgress(j storage.ReviewJob) bool {
	return j.Status == storage.JobStatusQueued || j.Status == storage.JobStatusRunning
}

// panelOutcomeSplit renders a terminal panel's member outcomes compactly, e.g.
// "2 ok · 1 failed". Falls back to "N/M done" if no per-outcome count is set.
func panelOutcomeSplit(s *storage.PanelSummary) string {
	if s == nil {
		return ""
	}
	parts := make([]string, 0, 4)
	if s.MembersSucceeded > 0 {
		parts = append(parts, fmt.Sprintf("%d ok", s.MembersSucceeded))
	}
	if s.MembersFailed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", s.MembersFailed))
	}
	if s.MembersCanceled > 0 {
		parts = append(parts, fmt.Sprintf("%d canceled", s.MembersCanceled))
	}
	if s.MembersSkipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", s.MembersSkipped))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%d/%d done", s.MembersTerminal, s.MembersTotal)
	}
	return strings.Join(parts, " · ")
}

// panelStatusCell is the parent-row panel summary: live progress while running,
// or a compact outcome split once terminal ("2 ok · 1 failed").
func panelStatusCell(j storage.ReviewJob) string {
	s := j.PanelSummary
	if s == nil {
		return ""
	}
	if panelInProgress(j) {
		return fmt.Sprintf("synthesizing… %d/%d reviewers done", s.MembersTerminal, s.MembersTotal)
	}
	return panelOutcomeSplit(s)
}
