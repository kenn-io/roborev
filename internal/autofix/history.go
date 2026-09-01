package autofix

import (
	"strconv"
	"strings"
)

// RestorationHistoryGuidance tells fix agents to recover established object
// identity before recreating behavior that a reviewed change removed.
const RestorationHistoryGuidance = `When a finding asks to restore, revert, or reinstate prior behavior, inspect the relevant repository history before editing. If a reviewed git ref is supplied, start with that ref and its parent commits, but apply the fix to the current checkout. Preserve established identifiers, schema object names, public API shapes, file paths, and migration semantics when they remain compatible. If history is unavailable or conflicts with current requirements, state the ambiguity instead of silently inventing a replacement identity.`

// FormatReviewedRef returns a prompt-safe line identifying the change that
// produced the findings. strconv.Quote keeps control characters and prompt
// delimiters in an untrusted stored ref from changing the prompt structure.
func FormatReviewedRef(gitRef string) string {
	gitRef = strings.TrimSpace(gitRef)
	if gitRef == "" {
		return ""
	}
	return "Reviewed git ref: " + strconv.Quote(gitRef) + ".\n\n"
}
