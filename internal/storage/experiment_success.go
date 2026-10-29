package storage

import (
	"encoding/json/v2"
	"fmt"
	"math"

	"go.kenn.io/roborev/internal/config"
)

// ExperimentSuccessResult evaluates the enrolled definition's objective. The
// free metric comparisons remain exploratory and are reported independently.
type ExperimentSuccessResult struct {
	Metric    string                  `json:"metric"`
	Direction string                  `json:"direction"`
	MinEffect string                  `json:"min_effect"`
	State     string                  `json:"state"`
	Reason    string                  `json:"reason"`
	Guards    []ExperimentGuardResult `json:"guards"`
}

type ExperimentGuardResult struct {
	Guard  string `json:"guard"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

func evaluateExperimentSuccess(r *ExperimentReport) *ExperimentSuccessResult {
	var definition struct {
		Success *config.ExperimentSuccess `json:"success"`
	}
	err := json.Unmarshal([]byte(r.definitionJSON), &definition)
	if err == nil && definition.Success == nil {
		return nil
	}
	result := &ExperimentSuccessResult{State: "not enough data", Reason: "Stored success definition is invalid."}
	if err != nil || config.ValidateExperimentSuccess(definition.Success) != nil {
		return result
	}
	bar := definition.Success
	result.Metric, result.Direction, result.MinEffect = bar.Metric, bar.Direction, bar.MinEffect
	for _, guard := range bar.Guards {
		result.Guards = append(result.Guards, ExperimentGuardResult{Guard: guard, State: "not enough data", Reason: "Success evaluation is gated."})
	}
	if !r.Integrity.Complete {
		result.Reason = r.Integrity.Reason
		return result
	}
	if r.Ratio == nil || *r.Ratio <= 0 || *r.Ratio >= 1 {
		result.Reason = "Allocation ratio endpoints cannot support a two-arm comparison."
		return result
	}
	metrics := make(map[string]ExperimentMetricComparison, len(r.Metrics))
	for _, m := range r.Metrics {
		metrics[m.Metric] = m
	}
	// Both reliability observations are required, even when the primary is
	// itself a reliability metric. A regression in either one gates the bar.
	for _, name := range []string{"job_failure_rate", "off_plan_rate"} {
		m := metrics[name]
		if !experimentSuccessDataAvailable(m) {
			result.Reason = fmt.Sprintf("%s: reliability observations are incomplete.", name)
			return result
		}
	}
	primary := metrics[bar.Metric]
	if !experimentSuccessDataAvailable(primary) {
		result.Reason = fmt.Sprintf("%s: %s", bar.Metric, primary.Reason)
		return result
	}
	minimum, _ := config.ParseExperimentMinEffect(bar.MinEffect)
	threshold, ok := experimentSuccessThreshold(minimum, primary.Default.Value)
	if !ok {
		result.Reason = "Primary relative effect is unavailable for a zero or undefined default baseline."
		return result
	}
	// Evaluate every requested guard's data before declaring a failed or met bar.
	// Unrelated unavailable metrics (including finding labels) do not gate it.
	unavailable, guardsMet := false, true
	for i, text := range bar.Guards {
		guard, _ := config.ParseExperimentGuard(text)
		m := metrics[guard.Metric]
		g := &result.Guards[i]
		if !experimentSuccessDataAvailable(m) {
			g.Reason = fmt.Sprintf("%s: %s", guard.Metric, m.Reason)
			unavailable = true
			continue
		}
		bound, valid := experimentSuccessThreshold(guard.Effect, m.Default.Value)
		if !valid {
			g.Reason = "Relative guard is unavailable for a zero or undefined default baseline."
			unavailable = true
			continue
		}
		met := (guard.Operator == "<=" && m.Interval.High <= bound) || (guard.Operator == ">=" && m.Interval.Low >= bound)
		g.State, g.Reason = "bar met", "Conservative confidence bound meets the guard."
		if !met {
			g.State, g.Reason = "bar not met", "Conservative confidence bound does not meet the guard."
			guardsMet = false
		}
	}
	if unavailable {
		result.Reason = "Required guard observations or relative baselines are unavailable."
		return result
	}
	for _, name := range []string{"job_failure_rate", "off_plan_rate"} {
		m := metrics[name]
		if *m.PValue <= .05 && m.Interval.Low > 0 {
			result.State, result.Reason = "bar not met", "A statistically detectable increase in job failure or off-plan execution gates success."
			return result
		}
	}
	// A zero minimum still requires a detected improvement in the declared
	// direction; a positive bootstrap interval alone cannot establish it.
	orientedLow := primary.Interval.Low
	if bar.Direction == "lower" {
		orientedLow = -primary.Interval.High
	}
	result.State = "bar not met"
	switch {
	case *primary.PValue > .05 || orientedLow <= 0:
		result.Reason = "The primary metric has no statistically detectable improvement in the declared direction."
	case orientedLow < threshold:
		result.Reason = "The primary confidence bound does not establish the minimum improvement."
	case !guardsMet:
		result.Reason = "One or more conservative guard bounds are not met."
	default:
		result.State, result.Reason = "bar met", "Detected improvement meets the minimum confidence bound and every guard."
	}
	return result
}

func experimentSuccessDataAvailable(m ExperimentMetricComparison) bool {
	return m.State != "not enough data" && m.Default.Complete && m.Experiment.Complete && m.Default.Value != nil && m.Experiment.Value != nil && m.Interval != nil && m.PValue != nil
}

func experimentSuccessThreshold(effect config.ExperimentEffect, baseline *float64) (float64, bool) {
	value := effect.Value
	if effect.Relative {
		if baseline == nil || *baseline == 0 {
			return 0, false
		}
		value = math.Abs(*baseline) * (value / 100)
	}
	return value, !math.IsInf(value, 0) && !math.IsNaN(value)
}
