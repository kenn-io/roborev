package tui

import (
	"fmt"
	"strings"

	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/helprender"
)

func (m model) renderFilterView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Filter"))
	b.WriteString("\x1b[K\n\x1b[K\n") // Clear title and blank line

	// Show loading state if tree hasn't been built yet
	if m.filterTree == nil {
		b.WriteString(statusStyle.Render("Loading repos..."))
		b.WriteString("\x1b[K\n")
		// Pad to fill terminal height: title(1) + blank(1) + loading(1) + padding + help(1)
		linesWritten := 3
		for linesWritten < m.height-1 {
			b.WriteString("\x1b[K\n")
			linesWritten++
		}
		b.WriteString(helpStyle.Render("esc: cancel"))
		b.WriteString("\x1b[K")
		b.WriteString("\x1b[J")
		return b.String()
	}

	// Search box
	searchDisplay := m.filterSearch
	if searchDisplay == "" {
		searchDisplay = statusStyle.Render("Type to search...")
	}
	fmt.Fprintf(&b, "Search: %s", searchDisplay)
	b.WriteString("\x1b[K\n\x1b[K\n")

	flatList := m.filterFlatList

	// Calculate visible rows
	filterHelpRows := [][]helplayout.HelpItem{
		{{Key: "↑/↓", Description: "nav"}, {Key: "→/←", Description: "expand/collapse"}, {Key: "↵", Description: "select"}, {Key: "esc", Description: "cancel"}, {Key: "type to search", Description: ""}},
	}
	filterHelpLines := len(convertAndReflowHelpRows(filterHelpRows, m.width))
	// Reserve: title(1) + blank(1) + search(1) + blank(1) + scroll-info(1) + blank(1) + help(N)
	reservedLines := 6 + filterHelpLines
	visibleRows := max(m.height-reservedLines, 0)

	// Determine which rows to show, keeping selected item visible
	start := 0
	end := len(flatList)
	needsScroll := len(flatList) > visibleRows && visibleRows > 0
	if needsScroll {
		start = max(m.filterSelectedIdx-visibleRows/2, 0)
		end = start + visibleRows
		if end > len(flatList) {
			end = len(flatList)
			start = max(end-visibleRows, 0)
		}
	} else if visibleRows > 0 {
		if end > visibleRows {
			end = visibleRows
		}
	} else {
		end = 0
	}

	linesWritten := 0
	for i := start; i < end; i++ {
		entry := flatList[i]
		var line string

		if entry.repoIdx == -1 {
			// "All" row
			line = fmt.Sprintf("  All (%d)", m.filterTreeTotalCount())
		} else if entry.branchIdx == -1 {
			// Repo node
			node := m.filterTree[entry.repoIdx]
			chevron := ">"
			if node.expanded {
				chevron = "v"
			}
			suffix := ""
			if node.loading {
				suffix = " ..."
			}
			line = fmt.Sprintf("  %s %s (%d)%s", chevron, sanitizeForDisplay(node.name), node.count, suffix)
		} else {
			// Branch node (indented)
			node := m.filterTree[entry.repoIdx]
			branch := node.children[entry.branchIdx]
			line = fmt.Sprintf("      %s (%d)", sanitizeForDisplay(branch.name), branch.count)
		}

		if i == m.filterSelectedIdx {
			b.WriteString(selectedStyle.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\x1b[K\n")
		linesWritten++
	}

	if len(flatList) == 0 {
		b.WriteString(statusStyle.Render("  No matching items"))
		b.WriteString("\x1b[K\n")
		linesWritten++
	} else if visibleRows == 0 {
		b.WriteString(statusStyle.Render("  (terminal too small)"))
		b.WriteString("\x1b[K\n")
		linesWritten++
	}

	// Pad with clear-to-end-of-line sequences to prevent ghost text
	for linesWritten < visibleRows {
		b.WriteString("\x1b[K\n")
		linesWritten++
	}

	if needsScroll {
		scrollInfo := fmt.Sprintf("[showing %d-%d of %d]", start+1, end, len(flatList))
		b.WriteString(statusStyle.Render(scrollInfo))
	}
	b.WriteString("\x1b[K\n")

	b.WriteString(helprender.RenderHelpTable(convertAndReflowHelpRows(filterHelpRows, m.width), helpTableStyles))
	b.WriteString("\x1b[K")
	b.WriteString("\x1b[J")

	return b.String()
}
