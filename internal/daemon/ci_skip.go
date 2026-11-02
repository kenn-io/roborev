package daemon

import "strings"

const ciDraftSkipReason = "Review skipped: draft"

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
