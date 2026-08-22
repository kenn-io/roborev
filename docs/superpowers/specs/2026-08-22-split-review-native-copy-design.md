# Restore Native Selection in Split Review Detail

## Goal

Restore native terminal text selection for review findings shown in the TUI's
split detail pane, while preserving mouse navigation in the queue pane.

## Context

Issue #1079 reports that the split view introduced a sharp edge: users can no
longer drag-select individual findings for copying. The existing TUI behavior
deliberately releases mouse capture in read-only content views so the terminal
can own text selection. Split view currently bypasses that rule and always
requests cell-motion mouse reporting while either split-rooted view is active.

## Design

Use the existing `mouseCaptureEnabled` policy for split views as well as
stacked views. While the queue is focused, `currentView` is `viewQueue`, so
cell-motion capture remains enabled for queue clicks and wheel navigation.
When the user enters the detail pane, `currentView` becomes `viewReview`, so
the rendered view requests no mouse mode and the terminal receives drag events
for native selection. Escaping back to the queue restores cell-motion capture.

No new selection model or clipboard protocol is needed. Detail-pane scrolling
remains available through the existing keyboard navigation, matching the
full-screen review view. Existing split mouse handlers remain unchanged and
continue to cover the list-focused interaction path.

## Testing

Add focused TUI view tests that verify split list focus reports
`tea.MouseModeCellMotion` and split detail focus reports
`tea.MouseModeNone`. Keep the existing stacked content-view tests as coverage
for the shared policy, and run the full TUI package plus repository checks
before publishing the change.

## Scope

The implementation is limited to the split view's mouse-mode selection and
the regression tests that define its list/detail behavior. It does not change
the `y` clipboard action, review rendering, queue navigation, or the global
mouse preference.
