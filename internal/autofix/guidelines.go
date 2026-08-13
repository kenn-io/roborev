// Package autofix provides shared formatting for trusted autofix policy.
package autofix

import "strings"

const GuidelinesHeading = "## Autofix Guidelines"

// AppendGuidelines appends non-empty user policy after all existing prompt
// content. Empty policy preserves the input byte for byte.
func AppendGuidelines(text, guidelines string) string {
	guidelines = strings.TrimSpace(guidelines)
	if guidelines == "" {
		return text
	}
	return text + "\n\n" + GuidelinesHeading + "\n\n" + guidelines
}
