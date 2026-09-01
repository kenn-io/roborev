package tui

// queuePaneColumns returns the visible queue columns that fit paneW,
// dropping from the end of the user's configured column order (rightmost-
// configured drops first). colSel, colJobID, and colRef always survive.
// Estimation: fixed columns use their minimum fixed widths; flex columns
// (ref/branch/repo) count a floor of min(content, 12); +1 spacing per
// non-first column. This mirrors renderQueueTable's sizing closely enough
// to decide fit; renderQueueTable does the exact layout afterward.
func (m model) queuePaneColumns(paneW int, contentWidth map[int]int) []int {
	core := map[int]bool{colSel: true, colJobID: true, colRef: true}
	estimate := func(c int) int {
		w := contentWidth[c]
		switch c {
		case colSel:
			return 2
		case colRef, colBranch, colRepo:
			return min(max(w, 4), 12)
		case colReviewType:
			return min(max(w, 11), 20)
		default:
			return w
		}
	}
	fits := func(cols []int) bool {
		total := 0
		for i, c := range cols {
			total += estimate(c)
			if i > 0 && c != colSel {
				total++
			}
		}
		return total <= paneW
	}

	cols := m.visibleColumns()
	// Drop candidates in reverse configured order until it fits.
	for i := len(m.columnOrder) - 1; i >= 0 && !fits(cols); i-- {
		drop := m.columnOrder[i]
		if core[drop] {
			continue
		}
		next := cols[:0:0]
		for _, c := range cols {
			if c != drop {
				next = append(next, c)
			}
		}
		cols = next
	}
	return cols
}

// renderQueuePaneBody renders the queue table body-only for the split list
// pane: exactly innerH clean lines (no escape codes), header + windowed
// rows, padded with blanks.
func (m model) renderQueuePaneBody(innerW, innerH int) []string {
	rows := m.visibleQueueRows()
	lines := make([]string, 0, innerH)
	if len(rows) == 0 {
		lines = append(lines, statusStyle.Render("No jobs"))
	} else {
		hasAnyPanel := anyPanelRow(rows)
		treeColor := queueColorEnabled()
		cw := m.queueContentWidths(rows, m.visibleColumns(), hasAnyPanel, treeColor)
		cols := m.queuePaneColumns(innerW, cw)
		// Table header+separator occupy the top of the pane; the rest is rows.
		lines = append(lines, m.renderQueueTable(rows, innerW, max(innerH-2, 1), cols, cw)...)
	}
	for len(lines) < innerH {
		lines = append(lines, "")
	}
	return lines[:innerH]
}
