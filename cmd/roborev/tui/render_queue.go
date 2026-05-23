package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
	"go.kenn.io/roborev/internal/version"
)

func (m model) getVisibleJobs() []storage.ReviewJob {
	if len(m.activeRepoFilter) == 0 && m.activeBranchFilter == "" && !m.hideClosed {
		return m.jobs
	}
	var visible []storage.ReviewJob
	for _, job := range m.jobs {
		if m.isJobVisible(job) {
			visible = append(visible, job)
		}
	}
	return visible
}
func (m model) queueHelpRows() [][]helpItem {
	row1 := []helpItem{
		{"x", "cancel"}, {"r", "rerun"}, {"l", "log"}, {"p", "prompt"},
		{"c", "comment"}, {"y", "copy"}, {"m", "commit"},
	}
	if m.tasksWorkflowEnabled() {
		row1 = append(row1, helpItem{"F", "fix"})
	}
	row1 = append(row1, helpItem{"o", "options"})
	row2 := []helpItem{
		{"↑/↓", "nav"}, {"↵", "review"}, {"a", "close"},
	}
	if !m.lockedRepoFilter || !m.lockedBranchFilter {
		row2 = append(row2, helpItem{"f", "filter"})
	}
	row2 = append(row2, helpItem{"h", "hide"})
	if m.shouldShowClassifyJobs() {
		row2 = append(row2, helpItem{"s", "hide classify"})
	} else {
		row2 = append(row2, helpItem{"s", "show classify"})
	}
	row2 = append(row2, helpItem{"D", "focus"})
	if m.tasksWorkflowEnabled() {
		row2 = append(row2, helpItem{"T", "tasks"})
	}
	row2 = append(row2, helpItem{"?", "help"})
	if !m.noQuit {
		row2 = append(row2, helpItem{"q", "quit"})
	}
	return [][]helpItem{row1, row2}
}
func (m model) queueHelpLines() int {
	return len(reflowHelpRows(m.queueHelpRows(), m.width))
}

// queueCompact returns true when chrome should be hidden
// (status line, table header, scroll indicator, flash, help footer).
// Triggered automatically for short terminals or manually via distraction-free mode.
func (m model) queueCompact() bool {
	return m.height < 15 || m.distractionFree
}

func (m model) queueVisibleRows() int {
	if m.queueCompact() {
		// compact: title(1) only
		return max(m.height-1, 1)
	}
	// title(1) + status(2) + header(1) + separator(1) + scroll(1) + flash(1) + help(dynamic)
	reserved := 7 + m.queueHelpLines()
	visibleRows := max(m.height-reserved, 3)
	return visibleRows
}
func (m model) canPaginate() bool {
	return m.hasMore && !m.loadingMore && !m.loadingJobs &&
		len(m.activeRepoFilter) <= 1 && m.activeBranchFilter != branchNone
}
func (m model) getVisibleSelectedIdx() int {
	if m.selectedIdx < 0 {
		return -1
	}
	if len(m.activeRepoFilter) == 0 && m.activeBranchFilter == "" && !m.hideClosed {
		return m.selectedIdx
	}
	count := 0
	for i, job := range m.jobs {
		if m.isJobVisible(job) {
			if i == m.selectedIdx {
				return count
			}
			count++
		}
	}
	return -1
}

// Queue table column indices.
const (
	colSel               = iota // "> " selection indicator
	colJobID                    // Job ID
	colRef                      // Git ref (short SHA or range)
	colBranch                   // Branch name
	colRepo                     // Repository display name
	colAgent                    // Agent name
	colQueued                   // Enqueue timestamp
	colElapsed                  // Elapsed time
	colStatus                   // Job status
	colPF                       // Pass/Fail verdict
	colHandled                  // Done status
	colSessionID                // Session ID
	colRequestedModel           // Explicitly requested model
	colRequestedProvider        // Explicitly requested provider
	colCost                     // Cost estimate (USD)
	colCount                    // total number of columns
)

func (m model) renderQueueView() string {
	var b strings.Builder
	compact := m.queueCompact()

	// Title with version, optional update notification, and filter indicators (in stack order)
	var title strings.Builder
	fmt.Fprintf(&title, "roborev queue (%s)", version.Version)
	for _, filterType := range m.filterStack {
		switch filterType {
		case filterTypeRepo:
			if len(m.activeRepoFilter) > 0 {
				filterName := m.repoFilterDisplayName()
				fmt.Fprintf(&title, " [f: %s]", filterName)
			}
		case filterTypeBranch:
			if m.activeBranchFilter != "" {
				fmt.Fprintf(&title, " [b: %s]", m.activeBranchFilter)
			}
		}
	}
	if m.hideClosed {
		title.WriteString(" [hiding closed]")
	}
	b.WriteString(titleStyle.Render(title.String()))
	// In compact mode, show version mismatch inline since the status area is hidden
	if compact && m.versionMismatch {
		b.WriteString(" ")
		b.WriteString(m.renderDaemonStatus())
	}
	b.WriteString("\x1b[K\n") // Clear to end of line

	if !compact {
		// Status line - use server-side aggregate counts for paginated views,
		// fall back to client-side counting for multi-repo filters (which load all jobs)
		var statusLine string
		var done, closed, open int
		if len(m.activeRepoFilter) > 1 || m.activeBranchFilter == branchNone {
			// Client-side filtered views load all jobs, so count locally
			for _, job := range m.jobs {
				if len(m.activeRepoFilter) > 0 && !m.repoMatchesFilter(job.RepoPath) {
					continue
				}
				if m.activeBranchFilter == branchNone && job.Branch != "" {
					continue
				}
				if job.Status == storage.JobStatusDone {
					done++
					if job.Closed != nil {
						if *job.Closed {
							closed++
						} else {
							open++
						}
					}
				}
			}
		} else {
			done = m.jobStats.Done
			closed = m.jobStats.Closed
			open = m.jobStats.Open
		}
		b.WriteString(m.renderDaemonStatus())
		if len(m.activeRepoFilter) > 0 || m.activeBranchFilter != "" {
			statusLine = fmt.Sprintf(" | Completed: %d | Closed: %d | Open: %d",
				done, closed, open)
		} else {
			statusLine = fmt.Sprintf(" | Workers: %d/%d | Completed: %d | Closed: %d | Open: %d",
				m.status.ActiveWorkers, m.status.MaxWorkers,
				done, closed, open)
		}
		b.WriteString(statusStyle.Render(statusLine))
		b.WriteString("\x1b[K\n") // Clear status line

		// Update notification on line 3 (above the table)
		if m.updateAvailable != "" {
			var updateMsg string
			if m.updateIsDevBuild {
				updateMsg = fmt.Sprintf("Dev build - latest release: %s - run 'roborev update --force'", m.updateAvailable)
			} else {
				updateMsg = fmt.Sprintf("Update available: %s - run 'roborev update'", m.updateAvailable)
			}
			b.WriteString(updateStyle.Render(updateMsg))
		}
		b.WriteString("\x1b[K\n") // Clear line 3
	}

	visibleJobList := m.getVisibleJobs()
	visibleSelectedIdx := m.getVisibleSelectedIdx()

	visibleRows := m.queueVisibleRows()

	// Track scroll indicator state for later
	var scrollInfo string
	start := 0
	end := 0

	if len(visibleJobList) == 0 {
		if m.loadingJobs || m.loadingMore {
			b.WriteString("Loading...")
			b.WriteString("\x1b[K\n")
		} else if len(m.activeRepoFilter) > 0 || m.hideClosed {
			b.WriteString("No jobs matching filters")
			b.WriteString("\x1b[K\n")
		} else {
			b.WriteString("No jobs in queue")
			b.WriteString("\x1b[K\n")
		}
		// Pad empty queue to fill visibleRows (minus 1 for the message we just wrote)
		// Also need header lines (2) to match non-empty case (skip in compact)
		linesWritten := 1
		padTarget := visibleRows
		if !compact {
			padTarget += 2 // +2 for header lines we skipped
		}
		for linesWritten < padTarget {
			b.WriteString("\x1b[K\n")
			linesWritten++
		}
	} else {
		// Calculate ID column width based on max ID
		idWidth := 5 // minimum width (fits "JobID" header)
		for _, job := range visibleJobList {
			w := len(fmt.Sprintf("%d", job.ID))
			if w > idWidth {
				idWidth = w
			}
		}

		// Determine which jobs to show, keeping selected item visible
		start = 0
		end = len(visibleJobList)

		if len(visibleJobList) > visibleRows {
			// Center the selected item when possible
			start = max(visibleSelectedIdx-visibleRows/2, 0)
			end = start + visibleRows
			if end > len(visibleJobList) {
				end = len(visibleJobList)
				start = end - visibleRows
			}
		}

		// Determine visible columns (respects hidden columns)
		visCols := m.visibleColumns()

		// Compute per-column max content widths, using cache when data hasn't changed.
		allHeaders := [colCount]string{"", "JobID", "Ref", "Branch", "Repo", "Agent", "Queued", "Elapsed", "Status", "P/F", "Closed", "Session", "Req Model", "Req Provider", "Cost"}
		allFullRows := make([][]string, len(visibleJobList))
		for i, job := range visibleJobList {
			cells := m.jobCells(job)
			fullRow := make([]string, colCount)
			fullRow[colSel] = "  "
			fullRow[colJobID] = fmt.Sprintf("%d", job.ID)
			copy(fullRow[colRef:], cells)
			allFullRows[i] = fullRow
		}

		var contentWidth map[int]int
		if m.queueColCache.gen == m.queueColGen {
			contentWidth = m.queueColCache.contentWidths
		} else {
			contentWidth = make(map[int]int, len(visCols))
			for _, c := range visCols {
				w := lipgloss.Width(allHeaders[c])
				for _, fullRow := range allFullRows {
					if cw := lipgloss.Width(fullRow[c]); cw > w {
						w = cw
					}
				}
				contentWidth[c] = w
			}
			m.queueColCache.gen = m.queueColGen
			m.queueColCache.contentWidths = contentWidth
		}

		// Compute column widths: fixed columns get their natural size,
		// flexible columns (Ref, Branch, Repo) absorb excess space.
		bordersOn := m.colBordersOn
		borderColor := lipgloss.AdaptiveColor{Light: "248", Dark: "242"}

		// Spacing per column: non-first, non-sel columns get 1 char of spacing
		// (either PaddingRight or border ▕ + PaddingLeft = 2 chars)
		spacing := func(tableCol int, logCol int) int {
			if logCol == colSel || tableCol == 0 {
				return 0
			}
			if bordersOn {
				return 2 // ▕ + PaddingLeft(1)
			}
			return 1 // PaddingRight(1)
		}

		// Fixed-width columns: exact sizes (content + padding, not counting inter-column spacing)
		fixedWidth := map[int]int{
			colSel:               2,
			colJobID:             idWidth,
			colStatus:            max(contentWidth[colStatus], 6), // "Status" header = 6, auto-sizes to content
			colQueued:            12,
			colElapsed:           8,
			colPF:                3,                                                    // "P/F" header = 3
			colHandled:           max(contentWidth[colHandled], 6),                     // "Closed" header = 6
			colAgent:             min(max(contentWidth[colAgent], 5), 12),              // "Agent" header = 5, cap at 12
			colSessionID:         min(max(contentWidth[colSessionID], 7), 12),          // "Session" header = 7, cap at 12
			colRequestedModel:    min(max(contentWidth[colRequestedModel], 9), 24),     // "Req Model" header = 9
			colRequestedProvider: min(max(contentWidth[colRequestedProvider], 12), 24), // "Req Provider" header = 12
			colCost:              max(contentWidth[colCost], 4),                        // "Cost" header = 4
		}

		// Flexible columns absorb excess space
		flexCols := []int{colRef, colBranch, colRepo}

		// Compute total fixed consumption
		totalFixed := 0
		for ti, c := range visCols {
			sp := spacing(ti, c)
			if fw, ok := fixedWidth[c]; ok {
				totalFixed += fw + sp
			} else {
				totalFixed += sp // spacing is always consumed
			}
		}

		remaining := m.width - totalFixed
		// Distribute remaining space among flex columns.
		// colWidths stores content-only width; StyleFunc adds spacing via
		// s.Width(w + spacing(col, logicalCol)) so the total column width
		// on screen = content width + inter-column spacing.
		colWidths := make(map[int]int, len(visCols))
		maps.Copy(colWidths, fixedWidth)

		// Build visible-only flex list once.
		var visFlex []int
		for _, c := range flexCols {
			if !m.hiddenColumns[c] {
				visFlex = append(visFlex, c)
			}
		}

		if len(visFlex) > 0 && remaining > 0 {
			// Two-phase distribution: first guarantee each flex
			// column at least min(contentWidth, equalShare), then
			// distribute surplus proportionally to remaining
			// content headroom. This prevents a single wide column
			// from starving narrower ones.
			equalShare := remaining / len(visFlex)

			// Phase 1: allocate floors.
			distributed := 0
			for _, c := range visFlex {
				floor := min(contentWidth[c], equalShare)
				colWidths[c] = max(floor, 1)
				distributed += colWidths[c]
			}

			// Drain overshoot from max(...,1) inflation when
			// remaining < len(visFlex).
			if distributed > remaining {
				drainFlexOverflow(visFlex, colWidths, distributed-remaining)
				distributed = remaining
			}

			// Compute headroom from actual allocated widths.
			totalHeadroom := 0
			headroom := make(map[int]int, len(visFlex))
			for _, c := range visFlex {
				h := contentWidth[c] - colWidths[c]
				if h > 0 {
					headroom[c] = h
					totalHeadroom += h
				}
			}

			// Phase 2: distribute surplus proportionally to
			// content headroom (columns already at content width
			// have zero headroom and get nothing extra).
			surplus := remaining - distributed
			if surplus > 0 && totalHeadroom > 0 {
				phase2 := 0
				for i, c := range visFlex {
					var extra int
					if i == len(visFlex)-1 {
						extra = surplus - phase2
					} else {
						extra = surplus * headroom[c] / totalHeadroom
					}
					colWidths[c] += extra
					phase2 += extra
				}
			} else if surplus > 0 {
				// All columns at content width — distribute
				// remaining space equally.
				for i, c := range visFlex {
					extra := surplus / (len(visFlex) - i)
					colWidths[c] += extra
					surplus -= extra
				}
			}
		} else if len(visFlex) > 0 {
			// No remaining space: give flex columns 1 char each to
			// avoid overflow at very narrow terminal widths.
			for _, c := range visFlex {
				colWidths[c] = 1
			}
		}

		// Build visible rows for the window
		windowJobs := visibleJobList[start:end]
		rows := make([][]string, 0, end-start)
		for i := range windowJobs {
			sel := "  "
			if start+i == visibleSelectedIdx {
				sel = "> "
			}
			fullRow := allFullRows[start+i]
			fullRow[colSel] = sel

			row := make([]string, len(visCols))
			for vi, c := range visCols {
				row[vi] = fullRow[c]
			}
			rows = append(rows, row)
		}

		// Compute the selected row index within the visible window
		selectedWindowIdx := visibleSelectedIdx - start

		// Find the last visible table column index (for padding logic)
		lastVisCol := len(visCols) - 1

		t := table.New().
			BorderTop(false).
			BorderBottom(false).
			BorderLeft(false).
			BorderRight(false).
			BorderColumn(false).
			BorderRow(false).
			BorderHeader(!compact).
			Border(lipgloss.Border{
				Top:    "─",
				Bottom: "─",
				Middle: "─",
			}).
			Width(m.width).
			Wrap(false).
			StyleFunc(func(row, col int) lipgloss.Style {
				s := lipgloss.NewStyle()

				// Map table col index to logical column
				logicalCol := colSel
				if col >= 0 && col < len(visCols) {
					logicalCol = visCols[col]
				}

				// Inter-column spacing: non-sel, non-first columns get border or padding
				if logicalCol != colSel && col > 0 {
					if bordersOn {
						s = s.Border(lipgloss.Border{Left: "▕"}, false, false, false, true).
							BorderForeground(borderColor).PaddingLeft(1)
					} else if col < lastVisCol {
						s = s.PaddingRight(1)
					}
				}

				// Set explicit width for all columns (includes spacing)
				w := colWidths[logicalCol]
				if w > 0 {
					s = s.Width(w + spacing(col, logicalCol))
				}

				// Right-align elapsed column
				if logicalCol == colElapsed {
					s = s.Align(lipgloss.Right)
				}

				// Header row styling
				if row == table.HeaderRow {
					return s.Foreground(lipgloss.AdaptiveColor{Light: "242", Dark: "246"})
				}

				// Selection highlighting — uniform background, no per-cell coloring
				if row == selectedWindowIdx {
					bg := lipgloss.AdaptiveColor{Light: "153", Dark: "24"}
					s = s.Background(bg)
					if bordersOn {
						s = s.BorderBackground(bg)
					}
					return s
				}

				// Per-cell coloring for non-selected rows
				if row >= 0 && row < len(windowJobs) {
					job := windowJobs[row]
					switch logicalCol {
					case colStatus:
						if c := statusColor(job.Status); c != nil {
							s = s.Foreground(c)
						}
					case colPF:
						if c := verdictColor(job.Verdict); c != nil {
							s = s.Foreground(c)
						}
					case colHandled:
						if job.Closed != nil {
							if *job.Closed {
								s = s.Foreground(closedStyle.GetForeground())
							} else {
								s = s.Foreground(queuedStyle.GetForeground())
							}
						}
					}
				}
				return s
			})

		// Always set headers — lipgloss table drops the last data row
		// when Headers() is not called.
		headers := make([]string, len(visCols))
		if !compact {
			for vi, c := range visCols {
				headers[vi] = allHeaders[c]
			}
		}
		t = t.Headers(headers...)
		t = t.Rows(rows...)

		tableStr := t.Render()

		// In compact mode, strip the empty header line we added as a
		// workaround (it renders as a row of spaces).
		if compact {
			if idx := strings.Index(tableStr, "\n"); idx >= 0 {
				tableStr = tableStr[idx+1:]
			}
		}
		b.WriteString(tableStr)
		b.WriteString("\x1b[K\n")

		// Pad with clear-to-end-of-line sequences to prevent ghost text
		tableLines := strings.Count(tableStr, "\n") + 1
		headerLines := 0
		if !compact {
			headerLines = 2 // header + separator
		}
		jobLinesWritten := tableLines - headerLines
		for jobLinesWritten < visibleRows {
			b.WriteString("\x1b[K\n")
			jobLinesWritten++
		}

		// Build scroll indicator if needed
		if len(visibleJobList) > visibleRows || m.hasMore || m.loadingMore {
			if m.loadingMore {
				scrollInfo = fmt.Sprintf("[showing %d-%d of %d] Loading more...", start+1, end, len(visibleJobList))
			} else if m.hasMore && len(m.activeRepoFilter) <= 1 {
				scrollInfo = fmt.Sprintf("[showing %d-%d of %d+] scroll down to load more", start+1, end, len(visibleJobList))
			} else if len(visibleJobList) > visibleRows {
				scrollInfo = fmt.Sprintf("[showing %d-%d of %d]", start+1, end, len(visibleJobList))
			}
		}
	}

	if !compact {
		// Always emit scroll indicator line (blank if no scroll info) to maintain consistent height
		if scrollInfo != "" {
			b.WriteString(statusStyle.Render(scrollInfo))
		}
		b.WriteString("\x1b[K\n") // Clear scroll indicator line

		// Status line: flash message (temporary)
		if flash := m.renderFlash(viewQueue); flash != "" {
			b.WriteString(flash)
		}
		b.WriteString("\x1b[K\n") // Clear to end of line

		// Help
		b.WriteString(renderHelpTable(m.queueHelpRows(), m.width))
	}

	output := b.String()
	if compact {
		// Trim trailing newline to avoid layout overflow (compact has no
		// help footer to consume the final line).
		output = strings.TrimSuffix(output, "\n")
	}
	output += "\x1b[K" // Clear to end of line (no newline at end)
	output += "\x1b[J" // Clear to end of screen to prevent artifacts

	return output
}

// jobCells returns plain text cell values for a job row.
// Order: ref, branch, repo, agent, queued, elapsed, status, pf, handled,
// session, requested model, requested provider, cost.
func (m model) jobCells(job storage.ReviewJob) []string {
	ref := shortJobRef(job)
	if !config.IsDefaultReviewType(job.ReviewType) {
		ref = ref + " [" + job.ReviewType + "]"
	}

	branch := m.getBranchForJob(job)

	repo := m.getDisplayName(job.RepoPath, job.RepoName)
	if m.status.MachineID != "" && job.SourceMachineID != "" && job.SourceMachineID != m.status.MachineID {
		repo += " [R]"
	}

	agentName := job.Agent
	if agentName == "claude-code" {
		agentName = "claude"
	}

	enqueued := job.EnqueuedAt.Local().Format("Jan 02 15:04")

	elapsed := ""
	if job.StartedAt != nil {
		if job.FinishedAt != nil {
			elapsed = job.FinishedAt.Sub(*job.StartedAt).Round(time.Second).String()
		} else {
			elapsed = time.Since(*job.StartedAt).Round(time.Second).String()
		}
	}

	status := statusLabel(job)

	verdict := "-"
	if job.Verdict != nil {
		verdict = *job.Verdict
	}

	handled := ""
	if job.Closed != nil {
		if *job.Closed {
			handled = "yes"
		} else {
			handled = "no"
		}
	}

	sessionID := stripControlChars(job.SessionID)
	if runes := []rune(sessionID); len(runes) > 12 {
		sessionID = string(runes[:12])
	}

	requestedModel := stripControlChars(job.RequestedModel)
	requestedProvider := stripControlChars(job.RequestedProvider)

	// Cost estimate from the stored agentsview usage blob; blank when
	// no priced estimate is available (old agentsview, unpriced model,
	// or usage not yet fetched for a running/queued job).
	cost := ""
	if tu := tokens.ParseJSON(job.TokenUsage); tu != nil {
		cost = tu.FormatCost()
	}

	return []string{ref, branch, repo, agentName, enqueued, elapsed, status, verdict, handled, sessionID, requestedModel, requestedProvider, cost}
}

// statusLabel returns a capitalized display label for the job status.
func statusLabel(job storage.ReviewJob) string {
	switch job.Status {
	case storage.JobStatusQueued:
		return "Queued"
	case storage.JobStatusRunning:
		return "Running"
	case storage.JobStatusFailed:
		return "Error"
	case storage.JobStatusCanceled:
		return "Canceled"
	case storage.JobStatusDone, storage.JobStatusApplied,
		storage.JobStatusRebased:
		return "Done"
	case storage.JobStatusSkipped:
		if job.SkipReason != "" {
			return "skipped: " + truncateReason(job.SkipReason, 40)
		}
		return "skipped"
	default:
		return string(job.Status)
	}
}

// truncateReason returns reason truncated to maxLen runes, with an ellipsis
// appended when it had to be cut.
func truncateReason(reason string, maxLen int) string {
	if maxLen <= 0 {
		return reason
	}
	runes := []rune(reason)
	if len(runes) <= maxLen {
		return reason
	}
	if maxLen <= 1 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-1]) + "…"
}

// statusColor returns the foreground color for the Status column.
func statusColor(
	status storage.JobStatus,
) lipgloss.TerminalColor {
	switch status {
	case storage.JobStatusQueued:
		return queuedStyle.GetForeground()
	case storage.JobStatusRunning:
		return runningStyle.GetForeground()
	case storage.JobStatusDone, storage.JobStatusApplied,
		storage.JobStatusRebased:
		return doneStyle.GetForeground()
	case storage.JobStatusFailed:
		return failedStyle.GetForeground()
	case storage.JobStatusCanceled:
		return canceledStyle.GetForeground()
	case storage.JobStatusSkipped:
		return canceledStyle.GetForeground()
	default:
		return nil
	}
}

// verdictColor returns the foreground color for the P/F column.
// Returns nil when no color should be applied (nil verdict).
func verdictColor(
	verdict *string,
) lipgloss.TerminalColor {
	if verdict == nil {
		return nil
	}
	if *verdict == "P" {
		return passStyle.GetForeground()
	}
	return failStyle.GetForeground()
}

// commandLineForJob computes the representative agent command line from job parameters.
// Returns empty string if the agent is not available.
func commandLineForJob(job *storage.ReviewJob) string {
	if job == nil {
		return ""
	}
	// Prefer the actual command line saved by the daemon worker.
	if job.CommandLine != "" {
		return stripControlChars(job.CommandLine)
	}
	// Fallback: reconstruct locally (may not reflect daemon config).
	a, err := agent.Get(job.Agent)
	if err != nil {
		return ""
	}
	reasoning := strings.ToLower(strings.TrimSpace(job.Reasoning))
	if reasoning == "" {
		reasoning = "thorough"
	}
	cmd := a.WithReasoning(agent.ParseReasoningLevel(reasoning)).WithAgentic(job.Agentic).WithModel(job.Model).CommandLine()
	return stripControlChars(cmd)
}

// stripControlChars removes all control characters including C0 (\x00-\x1f),
// DEL (\x7f), and C1 (\x80-\x9f) from a string to prevent terminal escape
// injection and line/tab spoofing in single-line display contexts.
func stripControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// migrateColumnConfig resets stale column_order and hidden_columns
// entries so users pick up the current default layout. Returns true
// if the config was modified and should be saved.
func migrateColumnConfig(cfg *config.Config) bool {
	dirty := false
	// Pre-rename config: "addressed" → reset
	if slices.Contains(cfg.ColumnOrder, "addressed") {
		cfg.ColumnOrder = nil
		dirty = true
	}
	if slices.Contains(cfg.HiddenColumns, "addressed") {
		cfg.HiddenColumns = nil
		dirty = true
	}
	// Old default orders → reset
	oldDefaults := [][]string{
		// status before queued (pre-rename)
		{"ref", "branch", "repo", "agent",
			"status", "queued", "elapsed", "closed"},
		// combined status without pf column
		{"ref", "branch", "repo", "agent",
			"queued", "elapsed", "status", "closed"},
	}
	for _, old := range oldDefaults {
		if slices.Equal(cfg.ColumnOrder, old) {
			cfg.ColumnOrder = nil
			dirty = true
			break
		}
	}

	// Version 1: backfill only the columns introduced in d2d671f6.
	// Do not touch session_id — it was already a default-hidden
	// column, so its absence from the list is a deliberate choice.
	if cfg.ColumnConfigVersion < 1 &&
		len(cfg.HiddenColumns) > 0 &&
		cfg.HiddenColumns[0] != config.HiddenColumnsNoneSentinel {
		v1NewColumns := []int{colRequestedModel, colRequestedProvider}
		for _, col := range v1NewColumns {
			name := columnConfigNames[col]
			if !slices.Contains(cfg.HiddenColumns, name) {
				cfg.HiddenColumns = append(
					cfg.HiddenColumns, name,
				)
			}
		}
		cfg.ColumnConfigVersion = 1
		dirty = true
	}

	return dirty
}

// toggleableColumns is the ordered list of columns the user can show/hide.
// colSel and colJobID are always visible and not included here.
var toggleableColumns = []int{colRef, colBranch, colRepo, colAgent, colQueued, colElapsed, colStatus, colPF, colHandled, colCost, colSessionID, colRequestedModel, colRequestedProvider}

// columnNames maps column constants to display names.
var columnNames = map[int]string{
	colRef:               "Ref",
	colBranch:            "Branch",
	colRepo:              "Repo",
	colAgent:             "Agent",
	colStatus:            "Status",
	colQueued:            "Queued",
	colElapsed:           "Elapsed",
	colPF:                "P/F",
	colHandled:           "Closed",
	colSessionID:         "Session",
	colRequestedModel:    "Req Model",
	colRequestedProvider: "Req Provider",
	colCost:              "Cost",
}

// columnConfigNames maps column constants to config file names (lowercase).
var columnConfigNames = map[int]string{
	colRef:               "ref",
	colBranch:            "branch",
	colRepo:              "repo",
	colAgent:             "agent",
	colStatus:            "status",
	colQueued:            "queued",
	colElapsed:           "elapsed",
	colPF:                "pf",
	colHandled:           "closed",
	colSessionID:         "session_id",
	colRequestedModel:    "requested_model",
	colRequestedProvider: "requested_provider",
	colCost:              "cost",
}

// drainFlexOverflow reduces flex column widths to absorb overflow,
// shrinking the widest column first, repeating until overflow is zero
// or all columns are at minimum width 1.
func drainFlexOverflow(
	cols []int, widths map[int]int, overflow int,
) {
	for overflow > 0 {
		widest := -1
		for _, c := range cols {
			if widths[c] > 1 && (widest < 0 || widths[c] > widths[widest]) {
				widest = c
			}
		}
		if widest < 0 {
			break
		}
		reduce := min(overflow, widths[widest]-1)
		widths[widest] -= reduce
		overflow -= reduce
	}
}

// lookupDisplayName returns the display name for a column constant from the given map.
func lookupDisplayName(col int, displayNames map[int]string) string {
	if name, ok := displayNames[col]; ok {
		return name
	}
	return "?"
}

// columnDisplayName returns the display name for a queue column constant.
func columnDisplayName(col int) string {
	return lookupDisplayName(col, columnNames)
}

// defaultHiddenColumns lists columns that are hidden by default.
// Users can enable them via the column options modal.
var defaultHiddenColumns = map[int]bool{
	colSessionID:         true,
	colRequestedModel:    true,
	colRequestedProvider: true,
}

// parseHiddenColumns converts config hidden_columns strings to column ID set.
// When names is empty (no user config), defaultHiddenColumns are applied.
// When names contains only the sentinel "_", the user explicitly has nothing hidden.
// Otherwise, only the listed columns are hidden.
func parseHiddenColumns(names []string) map[int]bool {
	result := map[int]bool{}
	if len(names) == 0 {
		maps.Copy(result, defaultHiddenColumns)
		return result
	}
	if len(names) == 1 && names[0] == config.HiddenColumnsNoneSentinel {
		return result
	}
	lookup := map[string]int{}
	for id, name := range columnConfigNames {
		lookup[name] = id
	}
	for _, n := range names {
		if id, ok := lookup[strings.ToLower(n)]; ok {
			result[id] = true
		}
	}
	return result
}

// hiddenColumnsToNames converts a hidden column ID set to config names.
// When nothing is hidden, returns the sentinel ["_"] to distinguish
// from an unconfigured (nil) slice.
func hiddenColumnsToNames(hidden map[int]bool) []string {
	var names []string
	// Maintain stable order
	for _, col := range toggleableColumns {
		if hidden[col] {
			names = append(names, columnConfigNames[col])
		}
	}
	if len(names) == 0 {
		return []string{config.HiddenColumnsNoneSentinel}
	}
	return names
}

// resolveColumnOrder converts config names to ordered column IDs using the given
// configNames map. Any columns from defaults not in names are appended at the end.
func resolveColumnOrder(names []string, configNames map[int]string, defaults []int) []int {
	if len(names) == 0 {
		result := make([]int, len(defaults))
		copy(result, defaults)
		return result
	}
	lookup := map[string]int{}
	for id, name := range configNames {
		lookup[name] = id
	}
	seen := map[int]bool{}
	var order []int
	for _, n := range names {
		if id, ok := lookup[strings.ToLower(n)]; ok && !seen[id] {
			order = append(order, id)
			seen[id] = true
		}
	}
	for _, col := range defaults {
		if !seen[col] {
			order = append(order, col)
		}
	}
	return order
}

// serializeColumnOrder converts ordered column IDs to config names.
func serializeColumnOrder(order []int, configNames map[int]string) []string {
	names := make([]string, 0, len(order))
	for _, col := range order {
		if name, ok := configNames[col]; ok {
			names = append(names, name)
		}
	}
	return names
}

// parseColumnOrder converts config names to ordered queue column IDs.
func parseColumnOrder(names []string) []int {
	return resolveColumnOrder(names, columnConfigNames, toggleableColumns)
}

// columnOrderToNames converts ordered queue column IDs to config names.
func columnOrderToNames(order []int) []string {
	return serializeColumnOrder(order, columnConfigNames)
}

// visibleColumns returns the ordered list of column indices to display,
// always including colSel and colJobID, plus any non-hidden toggleable columns.
func (m model) visibleColumns() []int {
	cols := []int{colSel, colJobID}
	for _, c := range m.columnOrder {
		if !m.hiddenColumns[c] {
			cols = append(cols, c)
		}
	}
	return cols
}

// saveColumnOptions persists table preferences and advanced task workflow state.
// Column order is only saved when it differs from the built-in default,
// so future default changes take effect for users who haven't customized.
func (m model) saveColumnOptions() tea.Cmd {
	hidden := hiddenColumnsToNames(m.hiddenColumns)
	borders := m.colBordersOn
	mouseEnabled := m.mouseEnabled
	tasksEnabled := m.tasksWorkflowEnabled()
	var colOrd []string
	if !slices.Equal(m.columnOrder, toggleableColumns) {
		colOrd = columnOrderToNames(m.columnOrder)
	}
	var taskColOrd []string
	if !slices.Equal(m.taskColumnOrder, taskToggleableColumns) {
		taskColOrd = taskColumnOrderToNames(m.taskColumnOrder)
	}
	return func() tea.Msg {
		cfg, err := config.LoadGlobal()
		if err != nil {
			return configSaveErrMsg{err: fmt.Errorf("load config: %w", err)}
		}
		cfg.HiddenColumns = hidden
		cfg.ColumnBorders = borders
		cfg.MouseEnabled = mouseEnabled
		cfg.ColumnOrder = colOrd
		cfg.TaskColumnOrder = taskColOrd
		cfg.Advanced.TasksEnabled = tasksEnabled
		cfg.ColumnConfigVersion = 1
		if err := config.SaveGlobal(cfg); err != nil {
			return configSaveErrMsg{err: fmt.Errorf("save config: %w", err)}
		}
		return nil
	}
}

// renderColumnOptionsView renders the column toggle modal.
func (m model) renderColumnOptionsView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Table Options"))
	b.WriteString("\n\n")

	for i, opt := range m.colOptionsList {
		check := "[ ]"
		if opt.enabled {
			check = "[x]"
		}
		prefix := "  "
		line := fmt.Sprintf("%s %s", check, opt.name)
		if i == m.colOptionsIdx {
			prefix = "> "
			line = selectedStyle.Render(line)
		}
		// Separator before settings/toggles
		if opt.id == colOptionBorders && i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\x1b[K\n")
	}

	b.WriteString("\n")
	helpRows := [][]helpItem{
		{{"↑/↓", "navigate"}, {"j/k", "reorder"}, {"space", "toggle"}, {"esc", "close"}},
	}
	b.WriteString(renderHelpTable(helpRows, m.width))

	return b.String()
}
