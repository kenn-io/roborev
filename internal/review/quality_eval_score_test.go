package review

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// These tests protect the evaluation's accounting, not model quality. Model
// findings are externally adjudicated under testdata/quality/rubric.json.
func TestQualityScoreOneFindingCannotEarnTwoBugs(t *testing.T) {
	got, err := qualityScore(qualityObservation{
		Expected: []string{"invoice", "cursor"}, Targets: []string{"invoice", "cursor"}, Findings: make([]StructuredFinding, 1),
	}, []qualityJudgment{{Finding: 1, Mechanisms: []string{"invoice", "cursor"}, Reason: "Combined allegation is ambiguous."}})
	require.NoError(t, err)
	assert.Equal(t, 0, got.Detected)
	assert.Equal(t, 2, got.UnknownExpected)
	assert.Equal(t, 1, got.UnjudgedFindings)
}

func TestQualityScoreDuplicatesCannotIncreaseRecall(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		labels := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{"invoice", "cursor"})))
		row := qualityObservation{Expected: []string{"invoice", "cursor"}, Targets: []string{"invoice", "cursor"}}
		var judgments []qualityJudgment
		for i, label := range labels {
			row.Findings = append(row.Findings, StructuredFinding{})
			judgments = append(judgments, qualityJudgment{Finding: i + 1, Mechanisms: []string{label}, Reason: "Source-grounded mechanism."})
		}
		got, err := qualityScore(row, judgments)
		require.NoError(ht, err)
		want := 0
		if slices.Contains(labels, "invoice") {
			want++
		}
		if slices.Contains(labels, "cursor") {
			want++
		}
		assert.Equal(ht, want, got.Detected)
		assert.Equal(ht, len(labels)-want, got.Duplicates)
	}, hegel.WithTestCases(2000))
}

func TestQualityScorePermutationDoesNotChangeCounts(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		labels := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{"invoice", "cursor", ""})))
		row := qualityObservation{Expected: []string{"invoice", "cursor"}, Targets: []string{"invoice", "cursor"}}
		var judgments []qualityJudgment
		for i, label := range labels {
			row.Findings = append(row.Findings, StructuredFinding{})
			if label != "" {
				judgments = append(judgments, qualityJudgment{Finding: i + 1, Mechanisms: []string{label}, Reason: "Mechanism."})
			}
		}
		before, err := qualityScore(row, judgments)
		require.NoError(ht, err)
		for i := range judgments {
			judgments[i].Finding = len(labels) + 1 - judgments[i].Finding
		}
		slices.Reverse(row.Findings)
		slices.Reverse(judgments)
		after, err := qualityScore(row, judgments)
		require.NoError(ht, err)
		assert.Equal(ht, before, after)
	}, hegel.WithTestCases(2000))
}

func TestQualityScoreFailuresAndUnjudgedRemainInDenominator(t *testing.T) {
	rows := []qualityObservation{
		{ID: "failed", Expected: []string{"invoice"}, Error: "provider failure"},
		{ID: "unjudged", Expected: []string{"invoice"}, Findings: make([]StructuredFinding, 1)},
		{ID: "missed", Expected: []string{"invoice"}},
	}
	got, err := qualitySummarize(rows, nil)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal(3, got.Planned)
	assert.Equal(2, got.Completed)
	assert.Equal(1, got.Failed)
	assert.Equal(1, got.UnjudgedCases)
	assert.Equal(qualityRate{Numerator: 0, Denominator: 3, Unknown: 2, Estimate: new(0.0), Upper: new(2.0 / 3), WilsonLow: new(0.0), WilsonHigh: got.Recall.WilsonHigh}, got.Recall)
	assert.Equal(1, got.Missed)
}

func TestQualityScoreSeparatesControlFPAndExtras(t *testing.T) {
	for _, variant := range []string{"fixed", "safe"} {
		t.Run(variant, func(t *testing.T) {
			row := qualityObservation{ID: variant, Variant: variant, Targets: []string{"invoice"}, Findings: make([]StructuredFinding, 3)}
			got, err := qualitySummarize([]qualityObservation{row}, map[string][]qualityJudgment{variant: {
				{Finding: 1, Mechanisms: []string{"invoice"}, Reason: "Reflags the guarded read."},
				{Finding: 2, Category: "legitimate_extra", Reason: "Unrelated grounded improvement."},
				{Finding: 3, Category: "other_false_positive", Reason: "Other claim is disproved."},
			}})
			require.NoError(t, err)
			assert.Equal(t, 1, got.LegitimateExtras)
			assert.Equal(t, 1, got.OtherFalsePositives)
			rates := map[string]qualityRate{"fixed": got.FixedFP, "safe": got.SafeFP}
			assert.Equal(t, 1, rates[variant].Numerator)
			assert.Equal(t, 1, rates[variant].Denominator)
		})
	}
}

func TestQualityScoreRejectsInvalidAdjudication(t *testing.T) {
	for _, judgments := range [][]qualityJudgment{
		{{Finding: 0, Category: "legitimate_extra", Reason: "Invalid index."}},
		{{Finding: 2, Category: "legitimate_extra", Reason: "Invalid index."}},
		{{Finding: 1, Category: "legitimate_extra", Reason: ""}},
		{{Finding: 1, Mechanisms: []string{"unknown"}, Reason: "Unknown mechanism."}},
		{{Finding: 1, Category: "clean", Reason: "Unknown category."}},
		{{Finding: 1, Category: "legitimate_extra", Mechanisms: []string{"invoice"}, Reason: "Conflicting labels."}},
		{{Finding: 1, Category: "legitimate_extra", Reason: "One."}, {Finding: 1, Category: "unjudged", Reason: "Two."}},
	} {
		_, err := qualityScore(qualityObservation{Targets: []string{"invoice"}, Findings: make([]StructuredFinding, 1)}, judgments)
		require.Error(t, err)
	}
}

func TestQualityRateEmptyUnavailable(t *testing.T) {
	assert.Equal(t, qualityRate{}, qualityMeasure(0, 0, 0))
}

func TestQualityRateWilsonKnownEndpoints(t *testing.T) {
	zero := qualityMeasure(0, 10, 0)
	all := qualityMeasure(10, 10, 0)
	require.NotNil(t, zero.WilsonHigh)
	require.NotNil(t, all.WilsonLow)
	assert.InDelta(t, 0.2775328, *zero.WilsonHigh, 0.0000001)
	assert.InDelta(t, 0.7224672, *all.WilsonLow, 0.0000001)
}

func TestQualityRateBoundsIncludeUnknown(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		// Values are counts materialized from a full-domain list of trial outcomes.
		trials := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 2)))
		successes, unknown := 0, 0
		for _, outcome := range trials {
			if outcome == 1 {
				successes++
			}
			if outcome == 2 {
				unknown++
			}
		}
		got := qualityMeasure(successes, len(trials), unknown)
		if len(trials) == 0 {
			assert.Nil(ht, got.Estimate)
			return
		}
		require.NotNil(ht, got.Estimate)
		require.NotNil(ht, got.Upper)
		assert.InDelta(ht, float64(successes)/float64(len(trials)), *got.Estimate, 0)
		assert.InDelta(ht, float64(successes+unknown)/float64(len(trials)), *got.Upper, 0)
		assert.False(ht, math.IsNaN(*got.WilsonLow))
	}, hegel.WithTestCases(2000))
}

func TestQualityScoreSynthesisCannotHideDistinctBug(t *testing.T) {
	row := qualityObservation{ID: "merge", Kind: "synthesis", Expected: []string{"invoice", "cursor"}, Targets: []string{"invoice", "cursor"}, Findings: make([]StructuredFinding, 1)}
	summary, err := qualitySummarize([]qualityObservation{row}, map[string][]qualityJudgment{"merge": {{Finding: 1, Mechanisms: []string{"invoice"}, Reason: "Only the ownership mechanism remains."}}})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.SynthesisLoss.Numerator)
	assert.Equal(t, 2, summary.SynthesisLoss.Denominator)
	assert.Equal(t, []string{"cursor"}, summary.Cases["merge"].Missed)
}

func TestQualityScoreAllTrialStatesConservePlannedDenominator(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		states := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{"complete", "failed", "unjudged", "planned"})))
		var rows []qualityObservation
		unknown, completed, failed, pending := 0, 0, 0, 0
		for i, state := range states {
			row := qualityObservation{ID: fmt.Sprint(i), Expected: []string{"invoice"}, Case: fmt.Sprint(i)}
			switch state {
			case "failed":
				row.Error = "failure"
				failed++
				unknown++
			case "unjudged":
				row.Findings = make([]StructuredFinding, 1)
				completed++
				unknown++
			case "planned":
				row.State = "planned"
				pending++
				unknown++
			default:
				completed++
			}
			rows = append(rows, row)
		}
		got, err := qualitySummarize(rows, nil)
		require.NoError(ht, err)
		assert.Equal(ht, len(states), got.Planned)
		assert.Equal(ht, len(states), got.Recall.Denominator)
		assert.Equal(ht, unknown, got.Recall.Unknown)
		assert.Equal(ht, completed, got.Completed)
		assert.Equal(ht, failed, got.Failed)
		assert.Equal(ht, pending, got.Pending)
	}, hegel.WithTestCases(2000))
}

func TestQualityControlStabilityReportsReflags(t *testing.T) {
	for _, variant := range []string{"fixed", "safe"} {
		rows := []qualityObservation{
			{ID: "reflag", Case: "invoice/" + variant, Variant: variant, Repeat: 1, Targets: []string{"invoice"}, Findings: make([]StructuredFinding, 1)},
			{ID: "clean", Case: "invoice/" + variant, Variant: variant, Repeat: 2, Targets: []string{"invoice"}},
		}
		summary, err := qualitySummarize(rows, map[string][]qualityJudgment{"reflag": {{Finding: 1, Mechanisms: []string{"invoice"}, Reason: "The control reflags the ownership mechanism."}}})
		require.NoError(t, err)
		assert.Equal(t, map[int][]string{1: {"invoice"}, 2: {}}, summary.Stability["invoice/"+variant].Matches)
	}
}

func TestQualityStabilityDistinguishesUnknownFromJudgedEmpty(t *testing.T) {
	rows := []qualityObservation{
		{ID: "clean", Case: "control", Repeat: 1, Targets: []string{"invoice"}},
		{ID: "unjudged", Case: "control", Repeat: 2, Targets: []string{"invoice"}, Findings: make([]StructuredFinding, 1)},
		{ID: "failed", Case: "control", Repeat: 3, Targets: []string{"invoice"}, Error: "provider failure"},
		{ID: "pending", Case: "control", Repeat: 4, Targets: []string{"invoice"}, State: "planned"},
	}
	summary, err := qualitySummarize(rows, nil)
	require.NoError(t, err)
	assert.Equal(t, map[int]string{1: "judged", 2: "unjudged", 3: "failed", 4: "planned"}, summary.Stability["control"].States)
}

func TestQualityScoreConservesEveryFinding(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		labels := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{"invoice", "cursor", "ambiguous", "legitimate_extra", "other_false_positive", "unjudged", "unassigned"})))
		row := qualityObservation{Expected: []string{"invoice"}, Targets: []string{"invoice", "cursor"}, Findings: make([]StructuredFinding, len(labels))}
		var judgments []qualityJudgment
		for i, label := range labels {
			judgment := qualityJudgment{Finding: i + 1, Reason: "External source adjudication."}
			switch label {
			case "unassigned":
				continue
			case "invoice", "cursor":
				judgment.Mechanisms = []string{label}
			case "ambiguous":
				judgment.Mechanisms = []string{"invoice", "cursor"}
			default:
				judgment.Category = label
			}
			judgments = append(judgments, judgment)
		}
		got, err := qualityScore(row, judgments)
		require.NoError(ht, err)
		assert.Equal(ht, len(labels), got.Detected+got.MechanismFP+got.Duplicates+got.LegitimateExtras+got.OtherFalsePositives+got.UnjudgedFindings)
	}, hegel.WithTestCases(2000))
}

func TestQualityPlanConservesRepeatDenominators(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		// Unbounded collection length materializes every positive repeat count;
		// the generator does not impose a new application count limit.
		repeat := len(hegel.Draw(ht, hegel.Lists(hegel.Booleans()))) + 1
		rows, err := qualityPlan(repeat)
		require.NoError(ht, err)
		ids := map[string]bool{}
		for i := range rows {
			ids[rows[i].ID] = true
			rows[i].State = "planned"
		}
		summary, err := qualitySummarize(rows, nil)
		require.NoError(ht, err)
		assert.Len(ht, ids, 8*repeat)
		assert.Equal(ht, 2*repeat, summary.Recall.Denominator)
		assert.Equal(ht, 2*repeat, summary.FixedFP.Denominator)
		assert.Equal(ht, 2*repeat, summary.SafeFP.Denominator)
		assert.Equal(ht, 3*repeat, summary.SynthesisLoss.Denominator)
	}, hegel.WithTestCases(2000))
}

func TestQualityFailedControlRemainsUnknown(t *testing.T) {
	for _, variant := range []string{"fixed", "safe"} {
		summary, err := qualitySummarize([]qualityObservation{{ID: variant, Variant: variant, Targets: []string{"invoice"}, Error: "provider failure"}}, nil)
		require.NoError(t, err)
		rate := summary.FixedFP
		if variant == "safe" {
			rate = summary.SafeFP
		}
		assert.Equal(t, 0, rate.Numerator)
		assert.Equal(t, 1, rate.Denominator)
		assert.Equal(t, 1, rate.Unknown)
		require.NotNil(t, rate.Estimate)
		require.NotNil(t, rate.Upper)
		assert.Zero(t, *rate.Estimate)
		assert.InDelta(t, 1, *rate.Upper, 0)
	}
}

func TestQualitySummaryRowPermutation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		labels := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{"invoice", "cursor", "unjudged", "failed", "planned", "clean"})))
		var rows []qualityObservation
		judgments := map[string][]qualityJudgment{}
		for i, label := range labels {
			row := qualityObservation{ID: fmt.Sprint(i), Case: "repeats", Repeat: i + 1, Expected: []string{"invoice"}, Targets: []string{"invoice", "cursor"}}
			switch label {
			case "failed":
				row.Error = "provider failure"
			case "planned":
				row.State = "planned"
			case "unjudged":
				row.Findings = make([]StructuredFinding, 1)
			case "invoice", "cursor":
				row.Findings = make([]StructuredFinding, 1)
				judgments[row.ID] = []qualityJudgment{{Finding: 1, Mechanisms: []string{label}, Reason: "External source attribution."}}
			}
			rows = append(rows, row)
		}
		before, err := qualitySummarize(rows, judgments)
		require.NoError(ht, err)
		slices.Reverse(rows)
		after, err := qualitySummarize(rows, judgments)
		require.NoError(ht, err)
		assert.Equal(ht, before, after)
	}, hegel.WithTestCases(2000))
}

func TestQualityPlanRejectsNonpositiveRepeats(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(math.MinInt, 0))
		_, err := qualityPlan(count)
		require.Error(ht, err)
	}, hegel.WithTestCases(2000))
}
