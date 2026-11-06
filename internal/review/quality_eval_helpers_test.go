package review

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// qualityObservation is one planned invocation, including invocations that did
// not finish. Findings are the settled product output; raw streams are saved
// separately. External judgments refer to 1-based positions in this exact output.
type qualityObservation struct {
	ID, Case, Variant, Kind, State, Error string
	Repeat                                int
	Expected, Targets                     []string
	Findings                              []StructuredFinding
	PromptHash, FixtureHash, OutputHash   string
}

type qualityJudgment struct {
	Finding          int
	Mechanisms       []string
	Category, Reason string
	// Optional external clarity assessment; deliberately excluded from correctness.
	Clarity map[string]string `json:",omitempty"`
}

type qualityRate struct {
	Numerator, Denominator, Unknown        int
	Estimate, Upper, WilsonLow, WilsonHigh *float64
}

type qualityCounts struct {
	Detected, Duplicates, UnknownExpected, UnjudgedFindings             int
	MechanismFP, UnknownControls, LegitimateExtras, OtherFalsePositives int
	Matched, Missed, Reported                                           []string
}

type qualityStability struct {
	Planned, Completed, Failed, Unjudged int
	// Confirmed semantic target sets, including fixed/safe reflags. States
	// distinguishes incomplete/failed/unjudged output from a judged empty set.
	Matches map[int][]string
	States  map[int]string
}

const qualityScoringVersion = 2

type qualitySummary struct {
	ScoringVersion                                                       int
	Planned, Completed, Failed, Pending, UnjudgedCases, UnjudgedFindings int
	Missed, Duplicates, LegitimateExtras, OtherFalsePositives            int
	Recall, FixedFP, SafeFP, SynthesisLoss                               qualityRate
	Cases                                                                map[string]qualityCounts
	Stability                                                            map[string]qualityStability
}

// qualityScore never infers semantics from file/line numbers, confidence, or
// keywords. Only one unambiguous external assignment can credit a finding. Extra
// reports of that mechanism are duplicates, including on fixed/safe controls.
func qualityScore(row qualityObservation, judgments []qualityJudgment) (qualityCounts, error) {
	var got qualityCounts
	known := append(slices.Clone(row.Targets), row.Expected...)
	for i, id := range row.Expected {
		if id == "" || slices.Contains(row.Expected[:i], id) {
			return got, fmt.Errorf("invalid expected mechanism %q", id)
		}
	}
	seenFindings := make(map[int]bool)
	matched := make(map[string]bool)
	for _, judgment := range judgments {
		if judgment.Finding < 1 || judgment.Finding > len(row.Findings) || seenFindings[judgment.Finding] {
			return got, fmt.Errorf("invalid or repeated finding %d", judgment.Finding)
		}
		seenFindings[judgment.Finding] = true
		if strings.TrimSpace(judgment.Reason) == "" {
			return got, fmt.Errorf("finding %d needs a judgment reason", judgment.Finding)
		}
		if len(judgment.Mechanisms) > 0 {
			if judgment.Category != "" {
				return got, fmt.Errorf("conflicting judgment labels")
			}
			for i, id := range judgment.Mechanisms {
				if !slices.Contains(known, id) || slices.Contains(judgment.Mechanisms[:i], id) {
					return got, fmt.Errorf("invalid mechanism %q", id)
				}
			}
			if len(judgment.Mechanisms) != 1 {
				got.UnjudgedFindings++
				continue
			}
			id := judgment.Mechanisms[0]
			if matched[id] {
				got.Duplicates++
			} else {
				matched[id] = true
			}
		} else {
			switch judgment.Category {
			case "legitimate_extra":
				got.LegitimateExtras++
			case "other_false_positive":
				got.OtherFalsePositives++
			case "unjudged":
				got.UnjudgedFindings++
			default:
				return got, fmt.Errorf("unknown judgment category %q", judgment.Category)
			}
		}
	}
	got.UnjudgedFindings += len(row.Findings) - len(seenFindings)
	if row.Error != "" || row.State == "planned" {
		if len(judgments) > 0 {
			return got, fmt.Errorf("failed invocations cannot earn judgments")
		}
		got.UnknownExpected = len(row.Expected)
		got.UnknownControls = len(row.Targets)
		return got, nil
	}
	got.Reported = make([]string, 0, len(matched))
	for id := range matched {
		got.Reported = append(got.Reported, id)
	}
	slices.Sort(got.Reported)
	for _, id := range row.Expected {
		if matched[id] {
			got.Detected++
			got.Matched = append(got.Matched, id)
		} else if got.UnjudgedFindings > 0 {
			got.UnknownExpected++
		} else {
			got.Missed = append(got.Missed, id)
		}
	}
	for _, id := range row.Targets {
		if slices.Contains(row.Expected, id) {
			continue
		}
		if matched[id] {
			got.MechanismFP++
		} else if got.UnjudgedFindings > 0 {
			got.UnknownControls++
		}
	}
	slices.Sort(got.Matched)
	slices.Sort(got.Missed)
	return got, nil
}

// qualityMeasure reports a conservative rate and its possible upper value.
// Wilson's 95% interval describes the observed binary trials conditional on
// this tiny fixture set. Unknowns widen the upper interval as well as the point
// bound; failures never shrink the planned denominator.
func qualityMeasure(n, d, u int) qualityRate {
	if d == 0 {
		return qualityRate{}
	}
	p := float64(n) / float64(d)
	hi := float64(n+u) / float64(d)
	wilson := func(p float64) (float64, float64) {
		const z = 1.959963984540054
		nf := float64(d)
		center := (p + z*z/(2*nf)) / (1 + z*z/nf)
		radius := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / (1 + z*z/nf)
		return math.Max(0, center-radius), math.Min(1, center+radius)
	}
	low, _ := wilson(p)
	_, high := wilson(hi)
	if n == 0 {
		low = 0
	}
	if n+u == d {
		high = 1
	}
	return qualityRate{n, d, u, new(p), new(hi), new(low), new(high)}
}

func qualitySummarize(rows []qualityObservation, judgments map[string][]qualityJudgment) (qualitySummary, error) {
	got := qualitySummary{ScoringVersion: qualityScoringVersion, Planned: len(rows), Cases: map[string]qualityCounts{}, Stability: map[string]qualityStability{}}
	var recallN, recallD, recallU, fixedN, fixedD, fixedU, safeN, safeD, safeU, lossN, lossD, lossU int
	for _, row := range rows {
		if row.ID == "" {
			return got, fmt.Errorf("missing invocation ID")
		}
		if _, exists := got.Cases[row.ID]; exists {
			return got, fmt.Errorf("duplicate invocation %q", row.ID)
		}
		score, err := qualityScore(row, judgments[row.ID])
		if err != nil {
			return got, fmt.Errorf("%s: %w", row.ID, err)
		}
		got.Cases[row.ID] = score
		if row.State == "planned" {
			got.Pending++
		} else if row.Error != "" {
			got.Failed++
		} else {
			got.Completed++
		}
		if score.UnjudgedFindings > 0 {
			got.UnjudgedCases++
		}
		got.UnjudgedFindings += score.UnjudgedFindings
		got.Missed += len(score.Missed)
		got.Duplicates += score.Duplicates
		got.LegitimateExtras += score.LegitimateExtras
		got.OtherFalsePositives += score.OtherFalsePositives
		if row.Kind == "synthesis" {
			lossN += len(score.Missed)
			lossD += len(row.Expected)
			lossU += score.UnknownExpected
		} else {
			recallN += score.Detected
			recallD += len(row.Expected)
			recallU += score.UnknownExpected
		}
		if row.Variant == "fixed" {
			fixedN += score.MechanismFP
			fixedD += len(row.Targets)
			fixedU += score.UnknownControls
		}
		if row.Variant == "safe" {
			safeN += score.MechanismFP
			safeD += len(row.Targets)
			safeU += score.UnknownControls
		}
		stable := got.Stability[row.Case]
		stable.Planned++
		if stable.Matches == nil {
			stable.Matches = make(map[int][]string)
			stable.States = make(map[int]string)
		}
		if row.State == "planned" {
			// Pending is visible in Planned - Completed - Failed.
		} else if row.Error != "" {
			stable.Failed++
		} else {
			stable.Completed++
		}
		if score.UnjudgedFindings > 0 {
			stable.Unjudged++
		}
		state := "judged"
		switch {
		case row.State == "planned":
			state = "planned"
		case row.Error != "":
			state = "failed"
		case score.UnjudgedFindings > 0:
			state = "unjudged"
		}
		stable.States[row.Repeat] = state
		stable.Matches[row.Repeat] = score.Reported
		got.Stability[row.Case] = stable
	}
	for id := range judgments {
		if _, exists := got.Cases[id]; !exists {
			return got, fmt.Errorf("judgment names unknown invocation %q", id)
		}
	}
	got.Recall = qualityMeasure(recallN, recallD, recallU)
	got.FixedFP = qualityMeasure(fixedN, fixedD, fixedU)
	got.SafeFP = qualityMeasure(safeN, safeD, safeU)
	got.SynthesisLoss = qualityMeasure(lossN, lossD, lossU)
	return got, nil
}
