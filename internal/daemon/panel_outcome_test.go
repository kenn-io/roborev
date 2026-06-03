package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	reviewpkg "go.kenn.io/roborev/internal/review"
)

func TestClassifyPanelOutcome(t *testing.T) {
	assert := assert.New(t)
	ok := reviewpkg.ReviewResult{Status: reviewpkg.ResultDone, Output: "Findings"}
	transient := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: reviewpkg.OutageErrorPrefix + "429"}
	genuine := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: "bad model"}
	quota := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: reviewpkg.QuotaErrorPrefix + "quota"}

	assert.Equal(OutcomePost, classifyPanelOutcome([]reviewpkg.ReviewResult{ok, transient}, 0).Kind)
	assert.Equal(OutcomeDeferTransient, classifyPanelOutcome([]reviewpkg.ReviewResult{transient}, 0).Kind)
	assert.Equal(OutcomeDeferGenuine, classifyPanelOutcome([]reviewpkg.ReviewResult{genuine}, 1).Kind)
	assert.Equal(OutcomeGenuineGiveUp, classifyPanelOutcome([]reviewpkg.ReviewResult{genuine}, 3).Kind)
	assert.Equal(OutcomeAllSkip, classifyPanelOutcome([]reviewpkg.ReviewResult{quota}, 0).Kind)
}

// TestClassifyPanelOutcomeExcerpt verifies the representative error excerpt is
// the first transient error for transient outcomes and the first genuine error
// for genuine outcomes.
func TestClassifyPanelOutcomeExcerpt(t *testing.T) {
	assert := assert.New(t)
	transientA := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: reviewpkg.OutageErrorPrefix + "first outage"}
	transientB := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: reviewpkg.OutageErrorPrefix + "second outage"}
	genuineA := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: "first genuine"}
	genuineB := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: "second genuine"}

	assert.Equal(reviewpkg.OutageErrorPrefix+"first outage",
		classifyPanelOutcome([]reviewpkg.ReviewResult{transientA, transientB}, 0).LastErrorExcerpt)
	assert.Equal("first genuine",
		classifyPanelOutcome([]reviewpkg.ReviewResult{genuineA, genuineB}, 0).LastErrorExcerpt)
	assert.Equal("first genuine",
		classifyPanelOutcome([]reviewpkg.ReviewResult{genuineA, genuineB}, 3).LastErrorExcerpt)
}

// TestClassifyPanelOutcomeEmptyIsAllSkip verifies an empty member set classifies
// as OutcomeAllSkip (rule 4 fall-through), never a post or defer.
func TestClassifyPanelOutcomeEmptyIsAllSkip(t *testing.T) {
	assert.Equal(t, OutcomeAllSkip, classifyPanelOutcome(nil, 0).Kind)
}

// TestClassifyPanelOutcomeDoneEmptyOutputIsNotPost verifies a done member with no
// output does not satisfy rule 1: a transient sibling still defers, and an
// all-done-but-empty set falls through to AllSkip.
func TestClassifyPanelOutcomeDoneEmptyOutputIsNotPost(t *testing.T) {
	assert := assert.New(t)
	doneEmpty := reviewpkg.ReviewResult{Status: reviewpkg.ResultDone, Output: "   "}
	transient := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: reviewpkg.OutageErrorPrefix + "429"}

	assert.Equal(OutcomeDeferTransient, classifyPanelOutcome([]reviewpkg.ReviewResult{doneEmpty, transient}, 0).Kind)
	assert.Equal(OutcomeAllSkip, classifyPanelOutcome([]reviewpkg.ReviewResult{doneEmpty}, 0).Kind)
}
