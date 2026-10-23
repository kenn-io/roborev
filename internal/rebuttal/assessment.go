// Package rebuttal defines focused finding re-evaluation and its agent adapter.
package rebuttal

import "go.kenn.io/roborev/internal/rebuttal/result"

// Assessment is the agent's assertion about the selected original finding.
// The result package is independent of adapters so storage can persist it.
type Assessment = result.Assessment

// Parse accepts one JSON assessment with a nonempty explanation.
func Parse(output string) (Assessment, error) { return result.Parse(output) }
