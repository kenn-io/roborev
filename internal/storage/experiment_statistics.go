package storage

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// The report contract specifies 4,096 resamples: Monte Carlo standard error
// near p=.05 is about .0034. This is an approximation budget, not an input cap.
const experimentResamples = 4096

var experimentReportMetrics = config.ExperimentReportMetrics()

type experimentInference struct {
	Delta        float64
	Interval     [2]float64
	PValue       float64
	Threshold    float64
	Approximate  bool
	Permutations int
}

func experimentMean(values []float64) float64 {
	// Dividing first avoids overflowing the sum for finite large observations.
	mean := 0.0
	for _, v := range values {
		mean += v / float64(len(values))
	}
	return mean
}

func compareExperimentSubjects(defaults, experimental []float64) experimentInference {
	defaults = slices.Clone(defaults)
	experimental = slices.Clone(experimental)
	slices.Sort(defaults)
	slices.Sort(experimental)
	result := experimentInference{Delta: experimentMean(experimental) - experimentMean(defaults)}
	pooled := append(slices.Clone(defaults), experimental...)
	slices.Sort(pooled)
	n := len(defaults)
	small := min(n, len(experimental))
	// Stop counting once exact enumeration exceeds the resampling budget.
	allocations := 1.0
	for i := 1; i <= small; i++ {
		allocations *= float64(len(pooled)-small+i) / float64(i)
		if allocations > experimentResamples {
			break
		}
	}
	differences := make([]float64, 0, experimentResamples)
	difference := func(chosen []int) float64 {
		left, right := make([]float64, 0, small), make([]float64, 0, len(pooled)-small)
		j := 0
		for i, v := range pooled {
			if j < len(chosen) && chosen[j] == i {
				left = append(left, v)
				j++
			} else {
				right = append(right, v)
			}
		}
		delta := experimentMean(right) - experimentMean(left)
		if n != small {
			delta = -delta
		}
		return delta
	}
	if allocations <= experimentResamples {
		chosen := make([]int, small)
		var enumerate func(int, int)
		enumerate = func(depth, start int) {
			if depth == small {
				differences = append(differences, math.Abs(difference(chosen)))
				return
			}
			for i := start; i <= len(pooled)-(small-depth); i++ {
				chosen[depth] = i
				enumerate(depth+1, i+1)
			}
		}
		enumerate(0, 0)
	} else {
		result.Approximate = true
		rng := rand.New(rand.NewPCG(0x726f626f, 0x726576))
		for range experimentResamples {
			indices := rng.Perm(len(pooled))[:small]
			slices.Sort(indices)
			differences = append(differences, math.Abs(difference(indices)))
		}
	}
	result.Permutations = len(differences)
	extreme := 0
	observed := math.Abs(result.Delta)
	for _, d := range differences {
		if d >= observed || math.Abs(d-observed) <= max(d, observed)*1e-12 {
			extreme++
		}
	}
	if result.Approximate {
		result.PValue = float64(extreme+1) / float64(len(differences)+1)
	} else {
		result.PValue = float64(extreme) / float64(len(differences))
	}
	slices.Sort(differences)
	result.Threshold = percentileSorted(differences, .95)
	// Independent fixed streams preserve within-arm sampling variation; neither
	// stream depends on database row order. Finite bootstrap draws need not
	// produce exactly mirrored endpoints when arms are swapped.
	defaultRNG := rand.New(rand.NewPCG(0x626f6f74, 0x7374726170))
	experimentRNG := rand.New(rand.NewPCG(0x7375626a, 0x65637473))
	bootstrap := make([]float64, experimentResamples)
	resampleMean := func(values []float64, rng *rand.Rand) float64 {
		sample := make([]float64, len(values))
		for i := range sample {
			sample[i] = values[rng.IntN(len(values))]
		}
		slices.Sort(sample)
		return experimentMean(sample)
	}
	for i := range bootstrap {
		bootstrap[i] = resampleMean(experimental, experimentRNG) - resampleMean(defaults, defaultRNG)
	}
	slices.Sort(bootstrap)
	result.Interval = [2]float64{percentileSorted(bootstrap, .025), percentileSorted(bootstrap, .975)}
	return result
}

func experimentMetricSubjects(units []experimentReportUnit, arm, metric string) (ExperimentMetricValue, []float64) {
	groups := map[string][]float64{}
	result := ExperimentMetricValue{}
	for _, u := range units {
		if u.arm != arm {
			continue
		}
		if u.expected[metric] {
			result.ExpectedUnits++
		}
		if value, ok := u.values[metric]; ok {
			groups[u.subject] = append(groups[u.subject], value)
			result.Units++
		}
	}
	values := make([]float64, 0, len(groups))
	for _, observations := range groups {
		slices.Sort(observations)
		value := experimentMean(observations)
		switch metric {
		case "duration_p50_seconds":
			value = percentileSorted(observations, .5)
		case "duration_p90_seconds":
			value = percentileSorted(observations, .9)
		}
		values = append(values, value)
	}
	slices.Sort(values)
	result.Subjects = len(values)
	if len(values) > 0 {
		result.Value = new(experimentMean(values))
	}
	if result.ExpectedUnits > 0 {
		result.Coverage = new(float64(result.Units) / float64(result.ExpectedUnits))
	}
	result.Complete = result.ExpectedUnits > 0 && result.Units == result.ExpectedUnits
	return result, values
}

func gateExperimentComparison(m *ExperimentMetricComparison, minSubjects int, integrity bool, integrityReason string, reliabilityWorse bool) {
	m.State = "not enough data"
	reason := ""
	switch {
	case !integrity:
		reason = integrityReason
	case !m.Default.Complete || !m.Experiment.Complete:
		reason = "Required metric coverage is incomplete."
	case m.Default.Subjects < minSubjects || m.Experiment.Subjects < minSubjects:
		reason = "Too few contributing subjects in one or both arms."
	case m.Default.Value == nil || m.Experiment.Value == nil:
		reason = "Metric observations are unavailable."
	}
	if reason != "" {
		m.Reason = reason
		m.Delta = nil
		m.Interval = nil
		m.PValue = nil
		m.RandomizationThreshold = nil
		m.Permutations = 0
		m.Approximate = false
		return
	}
	if reliabilityWorse {
		m.State = "reliability worse"
		m.Reason = "A statistically detectable increase in job failure or off-plan execution gates effect conclusions."
		return
	}
	m.State = "no detectable difference"
	m.Reason = "Exploratory, unadjusted comparison."
	if m.PValue == nil || m.Interval == nil || *m.PValue > .05 || (m.Interval.Low <= 0 && m.Interval.High >= 0) {
		return
	}
	lower := m.Interval.High < 0
	descriptive := m.Metric == "fail_verdict_rate" || strings.HasPrefix(m.Metric, "findings_")
	if descriptive {
		if lower {
			m.State = "lower"
		} else {
			m.State = "higher"
		}
	} else {
		if lower {
			m.State = "better"
		} else {
			m.State = "worse"
		}
	}
}

func buildExperimentComparisons(units []experimentReportUnit, r *ExperimentReport) []ExperimentMetricComparison {
	metrics := make([]ExperimentMetricComparison, 0, len(experimentReportMetrics))
	integrity := r.Integrity.Complete
	reason := r.Integrity.Reason
	if r.Ratio == nil || *r.Ratio <= 0 || *r.Ratio >= 1 {
		integrity = false
		reason = "Allocation ratio endpoints cannot support a two-arm comparison."
	}
	for _, name := range experimentReportMetrics {
		m := ExperimentMetricComparison{Metric: name}
		var defaults, experimental []float64
		m.Default, defaults = experimentMetricSubjects(units, "default", name)
		m.Experiment, experimental = experimentMetricSubjects(units, "experiment", name)
		// Resolve integrity and coverage before doing numerical inference.
		gateExperimentComparison(&m, r.MinSubjects, integrity, reason, false)
		if m.State != "not enough data" {
			result := compareExperimentSubjects(defaults, experimental)
			m.Delta = new(result.Delta)
			m.Interval = &ExperimentInterval{Low: result.Interval[0], High: result.Interval[1]}
			m.PValue = new(result.PValue)
			m.RandomizationThreshold = new(result.Threshold)
			m.Approximate = result.Approximate
			m.Permutations = result.Permutations
		}
		gateExperimentComparison(&m, r.MinSubjects, integrity, reason, false)
		if name == "refuted_per_100" || name == "confirmed_per_review" || name == "cost_per_confirmed" {
			m.Reason = r.Labels.Reason
		}
		metrics = append(metrics, m)
	}
	reliability := false
	for _, m := range metrics {
		if (m.Metric == "job_failure_rate" || m.Metric == "off_plan_rate") && m.State == "worse" {
			reliability = true
		}
	}
	if reliability {
		for i := range metrics {
			m := &metrics[i]
			if m.Metric != "job_failure_rate" && m.Metric != "off_plan_rate" && m.State != "not enough data" {
				gateExperimentComparison(m, r.MinSubjects, integrity, reason, true)
			}
		}
	}
	return metrics
}

func experimentAllocation(defaults, experimental int, ratio *float64) ExperimentAllocation {
	result := ExperimentAllocation{DefaultSubjects: defaults, ExperimentalSubjects: experimental}
	if ratio == nil || *ratio <= 0 || *ratio >= 1 {
		result.Warning = "Allocation ratio endpoints cannot support a two-arm comparison."
		return result
	}
	total := float64(defaults + experimental)
	if total == 0 {
		return result
	}
	expectedDefault := total * (1 - *ratio)
	expectedExperimental := total * *ratio
	result.ExpectedDefault = &expectedDefault
	result.ExpectedExperimental = &expectedExperimental
	statistic := math.Pow(float64(defaults)-expectedDefault, 2)/expectedDefault + math.Pow(float64(experimental)-expectedExperimental, 2)/expectedExperimental
	result.ChiSquare = &statistic
	result.PValue = new(math.Erfc(math.Sqrt(statistic / 2)))
	result.Approximate = expectedDefault < 5 || expectedExperimental < 5
	if *result.PValue <= .05 {
		result.Warning = "Allocation imbalance is a warning, not proof of broken enrollment."
	} else if result.Approximate {
		result.Warning = "Small expected allocation counts make the chi-square diagnostic approximate."
	}
	return result
}
