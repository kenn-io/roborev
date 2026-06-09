package tui

import (
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/mattn/go-runewidth"

	"go.kenn.io/roborev/internal/storage"
)

// Task view column constants (prefixed tcol to avoid collision with queue's col constants).
const (
	tcolSel        = iota // "> " selection indicator
	tcolStatus            // Job status
	tcolJobID             // Job ID
	tcolParent            // Parent job reference
	tcolQueued            // Enqueue timestamp
	tcolElapsed           // Elapsed time
	tcolBranch            // Branch name
	tcolRepo              // Repository display name
	tcolRefSubject        // Git ref + commit subject
	tcolCount             // total number of task columns
)

// taskToggleableColumns is the ordered list of task columns the user can show/hide/reorder.
// tcolSel is always visible and not included here.
var taskToggleableColumns = []int{tcolStatus, tcolJobID, tcolParent, tcolQueued, tcolElapsed, tcolBranch, tcolRepo, tcolRefSubject}

// taskColumnNames maps task column constants to display names.
var taskColumnNames = map[int]string{
	tcolStatus:     "Status",
	tcolJobID:      "Job",
	tcolParent:     "Parent",
	tcolQueued:     "Queued",
	tcolElapsed:    "Elapsed",
	tcolBranch:     "Branch",
	tcolRepo:       "Repo",
	tcolRefSubject: "Ref/Subject",
}

// taskColumnConfigNames maps task column constants to config file names.
var taskColumnConfigNames = map[int]string{
	tcolStatus:     "status",
	tcolJobID:      "job",
	tcolParent:     "parent",
	tcolQueued:     "queued",
	tcolElapsed:    "elapsed",
	tcolBranch:     "branch",
	tcolRepo:       "repo",
	tcolRefSubject: "ref_subject",
}

// taskColumnDisplayName returns the display name for a task column constant.
func taskColumnDisplayName(col int) string {
	return lookupDisplayName(col, taskColumnNames)
}

// parseTaskColumnOrder converts config names to ordered task column IDs.
func parseTaskColumnOrder(names []string) []int {
	return resolveColumnOrder(names, taskColumnConfigNames, taskToggleableColumns)
}

// taskColumnOrderToNames converts ordered task column IDs to config names.
func taskColumnOrderToNames(order []int) []string {
	return serializeColumnOrder(order, taskColumnConfigNames)
}

// visibleTaskColumns returns the ordered list of task column indices to display.
func (m model) visibleTaskColumns() []int {
	cols := make([]int, 0, 1+len(m.taskColumnOrder))
	cols = append(cols, tcolSel)
	cols = append(cols, m.taskColumnOrder...)
	return cols
}

// taskCells returns plain text cell values for a fix job row.
// Order: status, jobID, parent, queued, elapsed, branch, repo, refSubject
// (tcolStatus through tcolRefSubject, 8 values).
func (m model) taskCells(job storage.ReviewJob) []string {
	var statusLabel string
	switch job.Status {
	case storage.JobStatusQueued:
		statusLabel = "queued"
	case storage.JobStatusRunning:
		statusLabel = "running"
	case storage.JobStatusDone:
		statusLabel = "ready"
	case storage.JobStatusFailed:
		statusLabel = "error"
	case storage.JobStatusCanceled:
		statusLabel = "canceled"
	case storage.JobStatusApplied:
		statusLabel = "applied"
	case storage.JobStatusRebased:
		statusLabel = "rebased"
	}

	jobID := fmt.Sprintf("#%d", job.ID)

	parentRef := ""
	if job.ParentJobID != nil {
		parentRef = fmt.Sprintf("fixes #%d", *job.ParentJobID)
	}

	queued := ""
	if !job.EnqueuedAt.IsZero() {
		queued = job.EnqueuedAt.Local().Format("Jan 02 15:04")
	}

	elapsed := ""
	if job.StartedAt != nil {
		if job.FinishedAt != nil {
			elapsed = job.FinishedAt.Sub(*job.StartedAt).Round(time.Second).String()
		} else {
			elapsed = time.Since(*job.StartedAt).Round(time.Second).String()
		}
	}

	branch := job.Branch

	defaultRepoName := job.RepoName
	if defaultRepoName == "" && job.RepoPath != "" {
		defaultRepoName = filepath.Base(job.RepoPath)
	}
	repo := m.getDisplayName(job.RepoPath, defaultRepoName)
	if m.status.MachineID != "" && job.SourceMachineID != "" && job.SourceMachineID != m.status.MachineID {
		repo += " [R]"
	}

	refSubject := job.GitRef
	if job.CommitSubject != "" {
		if refSubject != "" {
			refSubject += " "
		}
		refSubject += job.CommitSubject
	}

	return []string{statusLabel, jobID, parentRef, queued, elapsed, branch, repo, refSubject}
}

func (m model) renderTasksView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("roborev tasks (background fixes)"))
	b.WriteString("\x1b[K\n")

	// Help overlay
	if m.fixShowHelp {
		return m.renderTasksHelpOverlay(&b)
	}

	if len(m.fixJobs) == 0 {
		b.WriteString("\n  No fix tasks. Press F on a review to trigger a background fix.\n")
		b.WriteString("\n")
		emptyHelp := []helpItem{{"T", "back to queue"}, {"F", "fix review"}}
		if !m.noQuit {
			emptyHelp = append(emptyHelp, helpItem{"q", "quit"})
		}
		b.WriteString(renderHelpTable([][]helpItem{emptyHelp}, m.width))
		b.WriteString("\x1b[K\x1b[J")
		return b.String()
	}

	// Help row calculation for visible rows
	tasksHelpRows := [][]helpItem{
		{{"enter", "view"}, {"P", "parent"}, {"p", "patch"}, {"A", "apply"}, {"l", "log"}},
		{{"x", "cancel"}, {"o", "options"}, {"?", "help"}, {"T/esc", "back"}},
	}
	tasksHelpLines := len(reflowHelpRows(tasksHelpRows, m.width))
	visibleRows := m.height - (6 + tasksHelpLines) // title + header + separator + status + scroll + help(N)
	visibleRows = max(visibleRows, 1)

	// Columns in user-configured order.
	visCols := m.visibleTaskColumns()

	// Compute per-column max content widths, using cache when data hasn't changed.
	allHeaders := [tcolCount]string{tcolSel: "", tcolStatus: "Status", tcolJobID: "Job", tcolParent: "Parent", tcolQueued: "Queued", tcolElapsed: "Elapsed", tcolBranch: "Branch", tcolRepo: "Repo", tcolRefSubject: "Ref/Subject"}
	allFullRows := make([][]string, len(m.fixJobs))
	for i, job := range m.fixJobs {
		cells := m.taskCells(job)
		fullRow := make([]string, tcolCount)
		fullRow[tcolSel] = "  "
		copy(fullRow[tcolStatus:], cells)
		allFullRows[i] = fullRow
	}

	var contentWidth map[int]int
	if m.taskColCache.gen == m.taskColGen {
		contentWidth = m.taskColCache.contentWidths
	} else {
		contentWidth = make(map[int]int, tcolCount)
		for _, c := range visCols {
			w := lipgloss.Width(allHeaders[c])
			for _, fullRow := range allFullRows {
				if cw := lipgloss.Width(fullRow[c]); cw > w {
					w = cw
				}
			}
			contentWidth[c] = w
		}
		m.taskColCache.gen = m.taskColGen
		m.taskColCache.contentWidths = contentWidth
	}

	// Column widths: fixed columns get their natural size,
	// flexible columns (Branch, Repo, Ref/Subject) absorb excess space.
	bordersOn := m.colBordersOn
	borderColor := adaptiveColor("248", "242")

	spacing := func(tableCol int, logCol int) int {
		if logCol == tcolSel || tableCol == 0 {
			return 0
		}
		if bordersOn {
			return 2 // ▕ + PaddingLeft(1)
		}
		return 1 // PaddingRight(1)
	}

	fixedWidth := map[int]int{
		tcolSel:     2,
		tcolStatus:  8,
		tcolJobID:   5,
		tcolParent:  11,
		tcolQueued:  12,
		tcolElapsed: 8,
	}

	flexCols := []int{tcolBranch, tcolRepo, tcolRefSubject}

	// Compute total fixed consumption
	totalFixed := 0
	totalFlex := 0
	flexContentTotal := 0
	for ti, c := range visCols {
		sp := spacing(ti, c)
		if fw, ok := fixedWidth[c]; ok {
			totalFixed += fw + sp
		} else {
			totalFlex++
			flexContentTotal += contentWidth[c]
			totalFixed += sp
		}
	}

	remaining := m.width - totalFixed
	colWidths := make(map[int]int, tcolCount)
	maps.Copy(colWidths, fixedWidth)

	if totalFlex > 0 && remaining > 0 {
		distributed := 0
		for _, c := range flexCols {
			if flexContentTotal > 0 {
				colWidths[c] = max(remaining*contentWidth[c]/flexContentTotal, 1)
			} else {
				colWidths[c] = max(remaining/totalFlex, 1)
			}
			distributed += colWidths[c]
		}
		// Correct rounding: add leftover to first flex column on
		// undershoot, drain overflow across flex columns (widest
		// first) on overshoot from max(...,1) inflation.
		if distributed < remaining {
			colWidths[flexCols[0]] += remaining - distributed
		} else {
			drainFlexOverflow(flexCols, colWidths, distributed-remaining)
		}
	} else if totalFlex > 0 {
		for _, c := range flexCols {
			colWidths[c] = 1
		}
	}

	// Determine scroll window
	startIdx := 0
	if m.fixSelectedIdx >= visibleRows {
		startIdx = m.fixSelectedIdx - visibleRows + 1
	}
	startIdx = min(startIdx, max(len(m.fixJobs)-1, 0))
	endIdx := min(len(m.fixJobs), startIdx+visibleRows)

	// Build visible rows for the window
	windowJobs := m.fixJobs[startIdx:endIdx]
	rows := make([][]string, 0, endIdx-startIdx)
	for i := range windowJobs {
		sel := "  "
		if startIdx+i == m.fixSelectedIdx {
			sel = "> "
		}
		fullRow := allFullRows[startIdx+i]
		fullRow[tcolSel] = sel

		row := make([]string, len(visCols))
		for vi, c := range visCols {
			row[vi] = fullRow[c]
		}
		rows = append(rows, row)
	}

	selectedWindowIdx := m.fixSelectedIdx - startIdx
	lastVisCol := len(visCols) - 1

	t := table.New().
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderColumn(false).
		BorderRow(false).
		BorderHeader(true).
		Border(lipgloss.Border{
			Top:    "─",
			Bottom: "─",
			Middle: "─",
		}).
		Width(m.width).
		Wrap(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle()

			logicalCol := tcolSel
			if col >= 0 && col < len(visCols) {
				logicalCol = visCols[col]
			}

			// Inter-column spacing
			if logicalCol != tcolSel && col > 0 {
				if bordersOn {
					s = s.Border(lipgloss.Border{Left: "▕"}, false, false, false, true).
						BorderForeground(borderColor).PaddingLeft(1)
				} else if col < lastVisCol {
					s = s.PaddingRight(1)
				}
			}

			// Set explicit width
			w := colWidths[logicalCol]
			if w > 0 {
				s = s.Width(w + spacing(col, logicalCol))
			}

			// Right-align elapsed column
			if logicalCol == tcolElapsed {
				s = s.Align(lipgloss.Right)
			}

			// Header row styling
			if row == table.HeaderRow {
				return s.Foreground(adaptiveColor("242", "246"))
			}

			// Selection highlighting
			if row == selectedWindowIdx {
				bg := adaptiveColor("153", "24")
				s = s.Background(bg)
				if bordersOn {
					s = s.BorderBackground(bg)
				}
				return s
			}

			// Per-cell coloring for non-selected rows: status column
			if logicalCol == tcolStatus && row >= 0 && row < len(windowJobs) {
				job := windowJobs[row]
				switch job.Status {
				case storage.JobStatusQueued:
					s = s.Foreground(queuedStyle.GetForeground())
				case storage.JobStatusRunning:
					s = s.Foreground(runningStyle.GetForeground())
				case storage.JobStatusDone, storage.JobStatusApplied:
					s = s.Foreground(doneStyle.GetForeground())
				case storage.JobStatusFailed:
					s = s.Foreground(failedStyle.GetForeground())
				case storage.JobStatusCanceled, storage.JobStatusRebased:
					s = s.Foreground(canceledStyle.GetForeground())
				}
			}
			return s
		})

	headers := make([]string, len(visCols))
	for vi, c := range visCols {
		headers[vi] = allHeaders[c]
	}
	t = t.Headers(headers...).Rows(rows...)

	tableStr := t.Render()
	b.WriteString(tableStr)
	b.WriteString("\x1b[K\n")

	// Pad to fill visibleRows
	tableLines := strings.Count(tableStr, "\n") + 1
	headerLines := 2 // header + separator
	jobLinesWritten := tableLines - headerLines
	for jobLinesWritten < visibleRows {
		b.WriteString("\x1b[K\n")
		jobLinesWritten++
	}

	// Flash message
	if flash := m.renderFlash(viewTasks); flash != "" {
		b.WriteString(flash)
	}
	b.WriteString("\x1b[K\n")

	// Help
	b.WriteString(renderHelpTable(tasksHelpRows, m.width))
	b.WriteString("\x1b[K\x1b[J")

	return b.String()
}

func (m model) renderTasksHelpOverlay(b *strings.Builder) string {
	help := []string{
		"",
		"  Task Status",
		"    queued     Waiting for a worker to pick up the job",
		"    running    Agent is working in an isolated worktree",
		"    ready      Patch captured and ready to apply to your working tree",
		"    failed     Agent failed (press enter or l to see error details)",
		"    applied    Patch was applied and committed to your working tree",
		"    canceled   Job was canceled by user",
		"",
		"  Keybindings",
		"    enter/l    View review output (ready) or error (failed) or log (running)",
		"    P          Open the parent review for this fix task",
		"    p          View the patch diff for a ready job",
		"    A          Apply patch from a ready job to your working tree",
		"    R          Re-run fix against current HEAD (when patch is stale)",
		"    F          Trigger a new fix from a review (from queue view)",
		"    x          Cancel a queued or running job",
		"    T/esc      Return to the main queue view",
		"    ?          Toggle this help",
		"",
		"  Workflow",
		"    1. Press F on a failing review to trigger a background fix",
		"    2. The agent runs in an isolated worktree (your files are untouched)",
		"    3. When status shows 'ready', press A to apply and commit the patch",
		"    4. If the patch is stale (code changed since), press R to re-run",
		"",
	}
	for _, line := range help {
		b.WriteString(line)
		b.WriteString("\x1b[K\n")
	}
	b.WriteString(helpStyle.Render("?: close help"))
	b.WriteString("\x1b[K\x1b[J")
	return b.String()
}

func (m model) renderPatchView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render(fmt.Sprintf("patch for fix job #%d", m.patchJobID)))
	b.WriteString("\x1b[K\n")

	if m.patchText == "" {
		b.WriteString("\n  No patch available.\n")
	} else {
		lines := strings.Split(m.patchText, "\n")
		visibleRows := max(m.height-4, 1)
		maxScroll := max(len(lines)-visibleRows, 0)
		start := max(min(m.patchScroll, maxScroll), 0)
		end := min(start+visibleRows, len(lines))

		addStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("34"))   // green
		delStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("160"))  // red
		hdrStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("33"))   // blue
		metaStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245")) // gray

		for _, line := range lines[start:end] {
			display := line
			switch {
			case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
				display = addStyle.Render(line)
			case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
				display = delStyle.Render(line)
			case strings.HasPrefix(line, "@@"):
				display = hdrStyle.Render(line)
			case strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ") ||
				strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++"):
				display = metaStyle.Render(line)
			}
			b.WriteString("  ")
			b.WriteString(display)
			b.WriteString("\x1b[K\n")
		}

		if len(lines) > visibleRows {
			pct := 0
			if maxScroll > 0 {
				pct = start * 100 / maxScroll
			}
			b.WriteString(helpStyle.Render(fmt.Sprintf("  [%d%%]", pct)))
			b.WriteString("\x1b[K\n")
		}
	}

	if m.savePatchInputActive {
		label := "Save to: "
		inputWidth := max(m.width-len(label)-2, 10)
		display := m.savePatchInput
		if w := runewidth.StringWidth(display); w > inputWidth {
			// Truncate from the left so the cursor end is visible
			rs := []rune(display)
			for runewidth.StringWidth(string(rs)) > inputWidth {
				rs = rs[1:]
			}
			display = string(rs)
		}
		display = display + strings.Repeat(" ", max(inputWidth-runewidth.StringWidth(display), 0))
		b.WriteString(helpStyle.Render(label))
		b.WriteString(display)
		b.WriteString("\x1b[K\n")
		b.WriteString(renderHelpTable([][]helpItem{
			{{"enter", "save"}, {"esc", "cancel"}},
		}, m.width))
	} else {
		b.WriteString(renderHelpTable([][]helpItem{
			{{"j/k/↑/↓", "scroll"}, {"s", "save"}, {"esc", "back to tasks"}},
		}, m.width))
	}
	b.WriteString("\x1b[K\x1b[J")
	return b.String()
}

func (m model) renderWorktreeConfirmView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Create Worktree"))
	b.WriteString("\x1b[K\n\n")

	fmt.Fprintf(&b, "  Branch %q is not checked out anywhere.\n", m.worktreeConfirmBranch)
	b.WriteString("  Create a temporary worktree to apply and commit the patch?\n\n")
	b.WriteString("  The worktree will be removed after the commit.\n")
	b.WriteString("  The commit will persist on the branch.\n\n")

	b.WriteString(helpStyle.Render("y/enter: create worktree and apply | esc/n: cancel"))
	b.WriteString("\x1b[K\x1b[J")

	return b.String()
}
