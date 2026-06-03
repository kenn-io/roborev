package tui

import (
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"

	"go.kenn.io/roborev/internal/storage"
)

// commandHeaderLines renders the "Command: <cmd>" header for a job, honoring
// the cmdExpanded toggle. Collapsed (default) returns a single line truncated
// to the terminal width; expanded wraps the full command across as many lines
// as needed. Returns nil when the job has no representative command line.
// The length of the result is the header height, so the renderers and the
// *VisibleLines helpers stay in agreement on layout.
func (m model) commandHeaderLines(job *storage.ReviewJob) []string {
	cmdLine := commandLineForJob(job)
	if cmdLine == "" {
		return nil
	}
	cmdText := "Command: " + cmdLine
	if m.width <= 0 {
		return []string{statusStyle.Render(cmdText)}
	}
	if m.cmdExpanded {
		wrapped := wrapLine(cmdText, m.width)
		lines := make([]string, len(wrapped))
		for i, ln := range wrapped {
			lines[i] = statusStyle.Render(ln)
		}
		return lines
	}
	if runewidth.StringWidth(cmdText) > m.width {
		cmdText = runewidth.Truncate(cmdText, m.width, "…")
	}
	return []string{statusStyle.Render(cmdText)}
}

func (m model) renderLogView() string {
	var b strings.Builder

	// Title with job info (matches Prompt view format)
	var title string
	job := m.logViewLookupJob()
	if job != nil {
		ref := shortJobRef(*job)
		agentStr := formatAgentLabel(job.Agent, job.Model)
		title = fmt.Sprintf(
			"Log #%d %s (%s)", job.ID, ref, agentStr,
		)
	} else {
		title = fmt.Sprintf("Log #%d", m.logJobID)
	}
	if m.logStreaming {
		title += " " + runningStyle.Render("● live")
	} else {
		title += " " + doneStyle.Render("● complete")
	}
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\x1b[K\n")

	// Show command line below title (dimmed, like Prompt view). Collapsed by
	// default; press i to expand the full command (wrapped) via cmdExpanded.
	headerLines := 1
	for _, line := range m.commandHeaderLines(job) {
		b.WriteString(line)
		b.WriteString("\x1b[K\n")
		headerLines++
	}

	// For auto-design-router rows (classify-typed, or terminal skipped
	// design reviews), surface the classifier verdict and reasoning above
	// any raw log lines. The header remains useful when the log file was
	// cleaned up or the classifier failed before producing output.
	// Truncated to m.width so each returned string occupies exactly one
	// terminal row, keeping the log content area aligned with logVisibleLines.
	for _, line := range classifyReasoningLines(job, m.width) {
		b.WriteString(line)
		b.WriteString("\x1b[K\n")
		headerLines++
	}

	// Calculate visible area (must match logVisibleLines())
	logHelp := m.logHelpRows()
	logHelpLines := len(reflowHelpRows(logHelp, m.width))
	reservedLines := (2 + logHelpLines) + headerLines // title + cmd(N, in headerLines) + sep + status + help(N)
	visibleLines := max(m.height-reservedLines, 1)

	// Clamp scroll
	maxScroll := max(len(m.logLines)-visibleLines, 0)
	scroll := max(min(m.logScroll, maxScroll), 0)

	// Separator
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\x1b[K\n")

	// Render lines (pre-styled by streamFormatter)
	linesWritten := 0
	if len(m.logLines) == 0 {
		if m.logLines == nil {
			b.WriteString(statusStyle.Render("Waiting for output..."))
		} else {
			b.WriteString(statusStyle.Render("(no output)"))
		}
		b.WriteString("\x1b[K\n")
		linesWritten++
	} else {
		end := min(scroll+visibleLines, len(m.logLines))
		for i := scroll; i < end; i++ {
			b.WriteString(m.logLines[i].text)
			b.WriteString("\x1b[K\n")
			linesWritten++
		}
	}

	// Pad remaining lines
	for linesWritten < visibleLines {
		b.WriteString("\x1b[K\n")
		linesWritten++
	}

	// Status line with position and follow mode
	var status string
	if len(m.logLines) > visibleLines {
		// Calculate actual displayed range (not including padding)
		displayEnd := min(scroll+visibleLines, len(m.logLines))
		status = fmt.Sprintf("[%d-%d of %d lines]", scroll+1, displayEnd, len(m.logLines))
	} else {
		status = fmt.Sprintf("[%d lines]", len(m.logLines))
	}
	if m.logFollow {
		status += " " + runningStyle.Render("[following]")
	} else {
		status += " " + statusStyle.Render("[paused - G to follow]")
	}
	b.WriteString(statusStyle.Render(status))
	b.WriteString("\x1b[K\n")

	b.WriteString(renderHelpTable(logHelp, m.width))
	b.WriteString("\x1b[K")
	b.WriteString("\x1b[J") // Clear to end of screen

	return b.String()
}

// classifyReasoningLines returns pre-styled header lines describing the
// auto-design-router classifier's verdict for a classify-typed row or a
// terminal skipped design row. Returns nil for jobs that aren't part of
// the auto-design pipeline so the log view leaves their layout alone.
//
// Both classify and skipped triggers require source='auto_design'; a
// future pipeline that adopts the classify job_type or the skipped
// status for unrelated reasons should not be mislabeled as
// auto-design output.
//
// Each returned string is truncated to fit width so it occupies exactly
// one terminal row. width <= 0 disables truncation (useful for tests or
// when the caller doesn't know the terminal width yet); the styled
// output reflects the raw text in that case.
func classifyReasoningLines(job *storage.ReviewJob, width int) []string {
	if job == nil || job.Source != "auto_design" {
		return nil
	}
	isClassify := job.JobType == storage.JobTypeClassify
	isAutoDesignSkipped := job.Status == storage.JobStatusSkipped
	if !isClassify && !isAutoDesignSkipped {
		return nil
	}

	var verdict string
	switch {
	case isAutoDesignSkipped:
		// completeClassifyAsSkip converts a classifier execution
		// failure into a skipped row with job.Error populated. A
		// clean "no design review needed" verdict comes from
		// applyClassifyVerdict and leaves job.Error empty. Don't
		// label a degraded skip as a clean verdict — operators
		// reading the log header need to see that the classifier
		// errored.
		if job.Error != "" {
			verdict = "Auto-design classifier failed (skipped)"
		} else {
			verdict = "Auto-design verdict: no design review needed"
		}
	case isClassify:
		// classify rows can sit in queued/running, or end in
		// failed/canceled when something went wrong before the
		// classifier could MarkClassifyAsSkippedDesign or
		// PromoteClassifyToDesignReview the row. Distinguish these so
		// 'l' on a failed classifier doesn't claim it's still running.
		switch job.Status {
		case storage.JobStatusQueued, storage.JobStatusRunning:
			verdict = "Auto-design classifier in progress"
		case storage.JobStatusFailed:
			verdict = "Auto-design classifier failed"
		case storage.JobStatusCanceled:
			verdict = "Auto-design classifier canceled"
		default:
			verdict = "Auto-design classifier: " + string(job.Status)
		}
	default:
		verdict = "Auto-design classifier"
	}

	render := func(text string) string {
		// Fold embedded newlines/tabs to single spaces so each
		// rendered line occupies one terminal row. job.Error stores
		// raw classifier stderr/error text and routinely contains
		// '\n'; without this, multiline blobs would wrap across
		// terminal rows while logVisibleLines reserves only one row
		// per returned line, pushing status/help off the screen.
		text = foldWhitespace(text)
		if width > 0 && runewidth.StringWidth(text) > width {
			text = runewidth.Truncate(text, width, "…")
		}
		return statusStyle.Render(text)
	}

	var lines []string
	lines = append(lines, render(verdict))
	if job.SkipReason != "" {
		lines = append(lines, render("Reason: "+job.SkipReason))
	}
	if job.Error != "" {
		lines = append(lines, render("Detail: "+job.Error))
	}
	return lines
}

// foldWhitespace replaces embedded newlines, carriage returns, and
// tabs with a single space and collapses runs of resulting whitespace.
// Used to keep one-row-per-line invariants when rendering raw error or
// reason text into the log view header.
func foldWhitespace(s string) string {
	if !strings.ContainsAny(s, "\n\r\t") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch r {
		case '\n', '\r', '\t':
			r = ' '
		}
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func formatHelpLine(key, desc string) string {
	return fmt.Sprintf("  %-14s %s", key, desc)
}

func disabledHelpLine(key, desc string) string {
	return statusStyle.Render(formatHelpLine(key, desc+" (disabled)"))
}

func helpLines(tasksEnabled, noQuit bool) []string {
	shortcuts := []struct {
		group string
		keys  []struct{ key, desc string }
	}{
		{
			group: "Queue View",
			keys: []struct{ key, desc string }{
				{"↑/k, ↓/j", "Navigate jobs"},
				{"g/Home", "Jump to top"},
				{"PgUp/PgDn", "Page through list"},
				{"enter", "View review"},
				{"p", "View prompt"},
				{"l", "View agent log"},
				{"m", "View commit message"},
			},
		},
		{
			group: "Actions",
			keys: []struct{ key, desc string }{
				{"a", "Toggle closed"},
				{"c", "Add comment"},
				{"y", "Copy review to clipboard"},
				{"x", "Cancel running/queued job"},
				{"r", "Re-run completed/failed job"},
				{"o", "Column options (visibility, order, borders)"},
				{"D", "Toggle distraction-free mode"},
			},
		},
		{
			group: "Filtering",
			keys: []struct{ key, desc string }{
				{"f", "Filter by repository/branch"},
				{"h", "Toggle hide closed/failed"},
				{"s", "Toggle classify rows (auto-design router)"},
				{"esc", "Clear filters (one at a time)"},
			},
		},
		{
			group: "Review View",
			keys: []struct{ key, desc string }{
				{"↑/↓", "Scroll content"},
				{"←/→", "Previous / next review"},
				{"PgUp/PgDn", "Page through content"},
				{"p", "Switch to prompt view"},
				{"a", "Toggle closed"},
				{"c", "Add comment"},
				{"y", "Copy review to clipboard"},
				{"m", "View commit message"},
				{"F", "Trigger fix (opens inline panel)"},
				{"esc/q", "Back to queue"},
			},
		},
		{
			group: "Prompt View",
			keys: []struct{ key, desc string }{
				{"↑/↓", "Scroll content"},
				{"←/→", "Previous / next prompt"},
				{"PgUp/PgDn", "Page through content"},
				{"i", "Expand/collapse command line"},
				{"p", "Switch to review / back to queue"},
				{"esc/q", "Back to queue"},
			},
		},
		{
			group: "Log View",
			keys: []struct{ key, desc string }{
				{"↑/↓", "Scroll output"},
				{"←/→", "Previous / next log"},
				{"PgUp/PgDn", "Page through output"},
				{"g", "Toggle follow mode / jump to top"},
				{"i", "Expand/collapse command line"},
				{"x", "Cancel job"},
				{"esc/q", "Back to queue"},
			},
		},
		{
			group: "Tasks View",
			keys: []struct{ key, desc string }{
				{"↑/↓", "Navigate fix jobs"},
				{"A", "Apply patch from completed fix"},
				{"R", "Re-trigger fix (rebase)"},
				{"l", "View agent log"},
				{"x", "Cancel running/queued fix job"},
				{"o", "Column options (order, borders)"},
				{"esc/T", "Back to queue"},
			},
		},
		{
			group: "General",
			keys: func() []struct{ key, desc string } {
				keys := []struct{ key, desc string }{
					{"?", "Toggle this help"},
				}
				if !noQuit {
					keys = append(keys, struct{ key, desc string }{
						"q", "Quit (from queue view)",
					})
				}
				return keys
			}(),
		},
	}

	var lines []string
	for i, g := range shortcuts {
		lines = append(lines, "\x00group:"+g.group)
		if g.group == "Tasks View" && !tasksEnabled {
			lines = append(lines, statusStyle.Render("  advanced.tasks_enabled = false"))
			for _, k := range g.keys {
				lines = append(lines, disabledHelpLine(k.key, k.desc))
			}
			if i < len(shortcuts)-1 {
				lines = append(lines, "")
			}
			continue
		}
		for _, k := range g.keys {
			if g.group == "Review View" && k.key == "F" && !tasksEnabled {
				lines = append(lines, disabledHelpLine(k.key, k.desc))
				continue
			}
			lines = append(lines, formatHelpLine(k.key, k.desc))
		}
		if g.group == "Actions" {
			if tasksEnabled {
				lines = append(lines, formatHelpLine("F", "Trigger fix for selected review"))
				lines = append(lines, formatHelpLine("T", "Open Tasks view"))
			} else {
				lines = append(lines, disabledHelpLine("F", "Trigger fix for selected review"))
				lines = append(lines, disabledHelpLine("T", "Open Tasks view"))
			}
		}
		if i < len(shortcuts)-1 {
			lines = append(lines, "")
		}
	}
	return lines
}

func (m model) helpMaxScroll() int {
	reservedLines := 3 // title + blank + help hint
	visibleLines := max(m.height-reservedLines, 5)
	maxScroll := len(helpLines(m.tasksWorkflowEnabled(), m.noQuit)) - visibleLines
	if maxScroll < 0 {
		return 0
	}
	return maxScroll
}

func (m model) renderHelpView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Keyboard Shortcuts"))
	b.WriteString("\x1b[K\n\x1b[K\n")

	allLines := helpLines(m.tasksWorkflowEnabled(), m.noQuit)

	// Calculate visible area: title(1) + blank(1) + help(1)
	reservedLines := 3
	visibleLines := max(m.height-reservedLines, 5)

	// Clamp scroll
	maxScroll := max(len(allLines)-visibleLines, 0)
	scroll := min(m.helpScroll, maxScroll)

	// Render visible window
	end := min(scroll+visibleLines, len(allLines))
	linesWritten := 0
	for _, line := range allLines[scroll:end] {
		if after, ok := strings.CutPrefix(line, "\x00group:"); ok {
			b.WriteString(selectedStyle.Render(after))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\x1b[K\n")
		linesWritten++
	}

	// Pad remaining space
	for linesWritten < visibleLines {
		b.WriteString("\x1b[K\n")
		linesWritten++
	}

	b.WriteString(renderHelpTable([][]helpItem{
		{{"↑/↓", "scroll"}, {"esc/q/?", "close"}},
	}, m.width))
	b.WriteString("\x1b[K")
	b.WriteString("\x1b[J") // Clear to end of screen

	return b.String()
}
