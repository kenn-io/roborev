package config

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ExperimentSuccess declares an immutable improvement objective and acceptable
// experiment-minus-default bounds. Effects use metric units or relative percent.
type ExperimentSuccess struct {
	Metric    string   `toml:"metric" json:"metric" comment:"Report metric used as the experiment's improvement objective."`
	Direction string   `toml:"direction" json:"direction" comment:"Required improvement direction: higher or lower."`
	MinEffect string   `toml:"min_effect" json:"min_effect" comment:"Nonnegative minimum improvement in metric units or percent of abs(default)."`
	Guards    []string `toml:"guards,omitempty" json:"guards,omitempty" comment:"Acceptable change bounds: metric <= or >= signed number, optionally percent."`
}

var experimentMetrics = []string{
	"cost_per_review", "duration_p50_seconds", "duration_p90_seconds",
	"fail_verdict_rate", "findings_critical_per_review", "findings_high_per_review",
	"findings_medium_per_review", "findings_low_per_review", "job_failure_rate",
	"off_plan_rate", "refuted_per_100", "confirmed_per_review", "cost_per_confirmed",
}

// ExperimentReportMetrics returns the exact names accepted in success bars and
// emitted by reports. A copy keeps callers from changing the registry.
func ExperimentReportMetrics() []string { return slices.Clone(experimentMetrics) }

// ExperimentEffect is a finite change, in metric units or percent of abs(default).
type ExperimentEffect struct {
	Value    float64
	Relative bool
}

// ExperimentGuard bounds an experiment-minus-default change.
type ExperimentGuard struct {
	Metric   string
	Operator string
	Effect   ExperimentEffect
}

var (
	experimentNumber = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
	experimentGuard  = regexp.MustCompile(`^([a-z][a-z0-9_]*)\s*(<=|>=)\s*(\S+)$`)
)

func parseExperimentEffect(raw string) (ExperimentEffect, error) {
	text := strings.TrimSpace(raw)
	effect := ExperimentEffect{Relative: strings.HasSuffix(text, "%")}
	if effect.Relative {
		text = strings.TrimSuffix(text, "%")
	}
	if !experimentNumber.MatchString(text) {
		return effect, fmt.Errorf("invalid effect %q: expected a finite number or percent", raw)
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return effect, fmt.Errorf("invalid nonfinite effect %q", raw)
	}
	effect.Value = value
	return effect, nil
}

// ParseExperimentMinEffect rejects signed negative and nonfinite minimums.
func ParseExperimentMinEffect(raw string) (ExperimentEffect, error) {
	effect, err := parseExperimentEffect(raw)
	if err == nil && effect.Value < 0 {
		err = fmt.Errorf("minimum effect must be nonnegative")
	}
	return effect, err
}

// ParseExperimentGuard parses one bound; it never evaluates expressions.
func ParseExperimentGuard(raw string) (ExperimentGuard, error) {
	parts := experimentGuard.FindStringSubmatch(strings.TrimSpace(raw))
	if parts == nil || !slices.Contains(experimentMetrics, parts[1]) {
		return ExperimentGuard{}, fmt.Errorf("invalid guard %q: expected report_metric <= or >= number[%%]", raw)
	}
	effect, err := parseExperimentEffect(parts[3])
	if err != nil {
		return ExperimentGuard{}, err
	}
	return ExperimentGuard{Metric: parts[1], Operator: parts[2], Effect: effect}, nil
}

// ValidateExperimentSuccess is shared with stored-definition report evaluation.
func ValidateExperimentSuccess(success *ExperimentSuccess) error {
	if success == nil {
		return nil
	}
	if !slices.Contains(experimentMetrics, success.Metric) {
		return fmt.Errorf("unknown success metric %q", success.Metric)
	}
	if success.Direction != "higher" && success.Direction != "lower" {
		return fmt.Errorf("success direction must be higher or lower")
	}
	if _, err := ParseExperimentMinEffect(success.MinEffect); err != nil {
		return fmt.Errorf("success min_effect: %w", err)
	}
	for _, guard := range success.Guards {
		if _, err := ParseExperimentGuard(guard); err != nil {
			return fmt.Errorf("success guards: %w", err)
		}
	}
	return nil
}

func cloneExperimentDefinition(definition ExperimentDefinition) ExperimentDefinition {
	if definition.Enabled != nil {
		definition.Enabled = new(*definition.Enabled)
	}
	if definition.Ratio != nil {
		definition.Ratio = new(*definition.Ratio)
	}
	definition.Workflows = slices.Clone(definition.Workflows)
	definition.Config = cloneExperimentMap(definition.Config)
	if definition.Success != nil {
		success := *definition.Success
		success.Guards = slices.Clone(success.Guards)
		definition.Success = &success
	}
	return definition
}

// UnmarshalTOML accepts a numeric absolute minimum as well as a string. The
// canonical definition always stores its deterministic string representation.
func (s *ExperimentSuccess) UnmarshalTOML(value any) error {
	fields, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("success must be a table")
	}
	var decoded ExperimentSuccess
	for key, value := range fields {
		switch key {
		case "metric", "direction":
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("success %s must be a string", key)
			}
			if key == "metric" {
				decoded.Metric = text
			} else {
				decoded.Direction = text
			}
		case "min_effect":
			switch minimum := value.(type) {
			case string:
				decoded.MinEffect = minimum
			case int64:
				decoded.MinEffect = strconv.FormatInt(minimum, 10)
			case float64:
				decoded.MinEffect = strconv.FormatFloat(minimum, 'g', -1, 64)
			default:
				return fmt.Errorf("success min_effect must be a number or percent string")
			}
		case "guards":
			values, ok := value.([]any)
			if !ok {
				return fmt.Errorf("success guards must be an array of strings")
			}
			for _, v := range values {
				text, ok := v.(string)
				if !ok {
					return fmt.Errorf("success guards must be an array of strings")
				}
				decoded.Guards = append(decoded.Guards, text)
			}
		default:
			return fmt.Errorf("unknown success key %q", key)
		}
	}
	*s = decoded
	return nil
}
