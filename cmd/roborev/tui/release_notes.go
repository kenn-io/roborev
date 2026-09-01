package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/muesli/termenv"
)

func (m model) handleReleaseNotesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m.handleQuitKey()
	case "esc", "q":
		m.currentView = m.releaseNotesFromView
		return m, nil
	case "u":
		m.releaseNotesLoading = true
		m.releaseNotesErr = nil
		return m, m.fetchReleaseNotes()
	case "home", "g":
		m.releaseNotesScroll = 0
	case "end", "G":
		m.releaseNotesScroll = m.releaseNotesMaxScroll()
	case "up", "k":
		m.releaseNotesScroll = max(0, m.releaseNotesScroll-1)
	case "down", "j":
		m.releaseNotesScroll = min(m.releaseNotesMaxScroll(), m.releaseNotesScroll+1)
	case "pgup":
		m.releaseNotesScroll = max(0, m.releaseNotesScroll-m.releaseNotesVisibleLines())
	case "pgdown":
		m.releaseNotesScroll = min(
			m.releaseNotesMaxScroll(),
			m.releaseNotesScroll+m.releaseNotesVisibleLines(),
		)
	}
	return m, nil
}

func (m model) releaseNotesVisibleLines() int {
	return max(m.height-3, 5)
}

func (m model) releaseNotesLines() []string {
	if m.releaseNotesLoading && len(m.releaseNotes) == 0 {
		return []string{"Loading release notes..."}
	}
	if m.releaseNotesErr != nil && len(m.releaseNotes) == 0 {
		return []string{
			errorStyle.Render("Could not load release notes"),
			m.releaseNotesErr.Error(),
			"",
			"Press u to retry.",
		}
	}
	if len(m.releaseNotes) == 0 {
		return []string{"No published releases found."}
	}

	var markdown strings.Builder
	if m.releaseNotesStale {
		markdown.WriteString("> GitHub could not be reached. Showing cached release notes.\n\n")
	}
	for i, release := range m.releaseNotes {
		if i > 0 {
			markdown.WriteString("\n---\n\n")
		}
		fmt.Fprintf(&markdown, "## %s\n\n", sanitizeForDisplay(release.Name))
		fmt.Fprintf(&markdown, "`%s` · %s", sanitizeForDisplay(release.TagName), release.PublishedAt.Format("January 2, 2006"))
		if release.Prerelease {
			markdown.WriteString(" · prerelease")
		}
		markdown.WriteString("\n\n")
		body := strings.TrimSpace(sanitizeForDisplay(release.Body))
		if body == "" {
			body = "No release notes were provided."
		}
		markdown.WriteString(body)
		if release.HTMLURL != "" {
			fmt.Fprintf(&markdown, "\n\n[View on GitHub](%s)\n", sanitizeForDisplay(release.HTMLURL))
		}
	}
	profile := termenv.Ascii
	if m.mdCache != nil {
		profile = m.mdCache.colorProfile
	}
	return renderMarkdownLines(
		markdown.String(), min(max(m.width-4, 20), 100), max(m.width, 20),
		m.glamourStyle, 2, profile,
	)
}

func (m model) releaseNotesMaxScroll() int {
	return max(len(m.releaseNotesLines())-m.releaseNotesVisibleLines(), 0)
}

func (m model) renderReleaseNotesView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Roborev release notes"))
	b.WriteString("\x1b[K\n\x1b[K\n")

	lines := m.releaseNotesLines()
	visible := m.releaseNotesVisibleLines()
	scroll := min(m.releaseNotesScroll, max(len(lines)-visible, 0))
	end := min(scroll+visible, len(lines))
	for _, line := range lines[scroll:end] {
		b.WriteString(line)
		b.WriteString("\x1b[K\n")
	}
	for i := end - scroll; i < visible; i++ {
		b.WriteString("\x1b[K\n")
	}
	b.WriteString(renderHelpTable([][]helpItem{
		{{"j/k", "scroll"}, {"pgup/pgdn", "page"}, {"u", "refresh"}, {"esc/q", "close"}},
	}, m.width))
	b.WriteString("\x1b[K\x1b[J")
	return b.String()
}
