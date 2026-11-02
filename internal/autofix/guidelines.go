// Package autofix provides shared formatting for trusted autofix policy.
package autofix

import "strings"

const GuidelinesHeading = "## Autofix Guidelines"

// GuidelinesEvaluationInstruction tells fix agents how to apply trusted policy.
const GuidelinesEvaluationInstruction = "Evaluate each finding against the autofix guidelines. " +
	"Apply changes for findings that warrant a fix, and record any finding intentionally not applied with the reason it was skipped."

// AppendGuidelines appends non-empty user policy after all existing prompt
// content. Empty policy preserves the input byte for byte.
func AppendGuidelines(text, guidelines string) string {
	guidelines = strings.TrimSpace(guidelines)
	if guidelines == "" {
		return text
	}
	return text + "\n\n" + GuidelinesHeading + "\n\n" + guidelines
}
