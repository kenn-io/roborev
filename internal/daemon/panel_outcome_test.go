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

	assert.Equal(OutcomePost, classifyPanelOutcome([]reviewpkg.ReviewResult{ok, transient}, nil, 0).Kind)
	assert.Equal(OutcomeDeferTransient, classifyPanelOutcome([]reviewpkg.ReviewResult{transient}, nil, 0).Kind)
	assert.Equal(OutcomeDeferGenuine, classifyPanelOutcome([]reviewpkg.ReviewResult{genuine}, nil, 1).Kind)
	assert.Equal(OutcomeGenuineGiveUp, classifyPanelOutcome([]reviewpkg.ReviewResult{genuine}, nil, 3).Kind)
	assert.Equal(OutcomeAllSkip, classifyPanelOutcome([]reviewpkg.ReviewResult{quota}, nil, 0).Kind)
}

// TestClassifyPanelOutcomeSynthesisFailure verifies the synthesis-failure
// precedence: a synthesis that failed on quota or a transient outage defers the
// whole run even when members produced output (rule 0), so the degraded raw
// fallback is never posted; a genuine synthesis failure falls through to the
// member rules and still posts; a nil synthesis leaves member classification
// unchanged.
func TestClassifyPanelOutcomeSynthesisFailure(t *testing.T) {
	assert := assert.New(t)
	ok := reviewpkg.ReviewResult{Status: reviewpkg.ResultDone, Output: "Member findings"}
	quota := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: reviewpkg.QuotaErrorPrefix + "quota exhausted"}
	transient := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: reviewpkg.OutageErrorPrefix + "429"}
	genuine := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: "synthesis crashed"}
	members := []reviewpkg.ReviewResult{ok}

	assert.Equal(OutcomeDeferTransient, classifyPanelOutcome(members, &quota, 0).Kind,
		"synthesis quota failure defers instead of posting the raw fallback")
	assert.Equal(reviewpkg.QuotaErrorPrefix+"quota exhausted",
		classifyPanelOutcome(members, &quota, 0).LastErrorExcerpt,
		"defer carries the synthesis error excerpt")
	assert.Equal(OutcomeDeferTransient, classifyPanelOutcome(members, &transient, 0).Kind,
		"synthesis transient outage defers")
	assert.Equal(OutcomePost, classifyPanelOutcome(members, &genuine, 0).Kind,
		"genuine synthesis failure still posts (raw fallback); retrying would not help")
	assert.Equal(OutcomePost, classifyPanelOutcome(members, nil, 0).Kind,
		"a healthy synthesis leaves member classification unchanged")
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
		classifyPanelOutcome([]reviewpkg.ReviewResult{transientA, transientB}, nil, 0).LastErrorExcerpt)
	assert.Equal("first genuine",
		classifyPanelOutcome([]reviewpkg.ReviewResult{genuineA, genuineB}, nil, 0).LastErrorExcerpt)
	assert.Equal("first genuine",
		classifyPanelOutcome([]reviewpkg.ReviewResult{genuineA, genuineB}, nil, 3).LastErrorExcerpt)
}

// TestClassifyPanelOutcomeEmptyIsAllSkip verifies an empty member set classifies
// as OutcomeAllSkip (rule 4 fall-through), never a post or defer.
func TestClassifyPanelOutcomeEmptyIsAllSkip(t *testing.T) {
	assert.Equal(t, OutcomeAllSkip, classifyPanelOutcome(nil, nil, 0).Kind)
}

// TestClassifyPanelOutcomeDoneEmptyOutputIsNotPost verifies a done member with no
// output does not satisfy rule 1: a transient sibling still defers, and an
// all-done-but-empty set falls through to AllSkip.
func TestClassifyPanelOutcomeDoneEmptyOutputIsNotPost(t *testing.T) {
	assert := assert.New(t)
	doneEmpty := reviewpkg.ReviewResult{Status: reviewpkg.ResultDone, Output: "   "}
	transient := reviewpkg.ReviewResult{Status: reviewpkg.ResultFailed, Error: reviewpkg.OutageErrorPrefix + "429"}

	assert.Equal(OutcomeDeferTransient, classifyPanelOutcome([]reviewpkg.ReviewResult{doneEmpty, transient}, nil, 0).Kind)
	assert.Equal(OutcomeAllSkip, classifyPanelOutcome([]reviewpkg.ReviewResult{doneEmpty}, nil, 0).Kind)
}
