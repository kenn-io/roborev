package daemon

import (
	"fmt"
	"strings"
)

const ciDraftSkipReason = "Review skipped: draft"

func ciSkipReason(draft bool, labels []string, skipDrafts bool, skipLabels []string) (string, bool) {
	if skipDrafts && draft {
		return ciDraftSkipReason, true
	}
	if label, skip := matchingCISkipLabel(labels, skipLabels); skip {
		return fmt.Sprintf("Review skipped: label %s", label), true
	}
	return "", false
}

func matchingCISkipLabel(prLabels, skipLabels []string) (string, bool) {
	for _, label := range prLabels {
		label = strings.TrimSpace(label)
		for _, configured := range skipLabels {
			configured = strings.TrimSpace(configured)
			if configured != "" && strings.EqualFold(label, configured) {
				return label, true
			}
		}
	}
	return "", false
}
