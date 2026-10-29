package storage

import (
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExperimentComparisonExactPermutation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		n int
		p float64
	}{{3, 0.1}, {4, 2.0 / 70}} {
		t.Run(string(rune('0'+tt.n)), func(t *testing.T) {
			d := make([]float64, tt.n)
			e := make([]float64, tt.n)
			for i := range e {
				e[i] = 1
			}
			got := compareExperimentSubjects(d, e)
			assert := assert.New(t)
			assert.InDelta(tt.p, got.PValue, 1e-12)
			assert.InDelta(1.0, got.Delta, 1e-12)
			assert.Equal([2]float64{1, 1}, got.Interval)
			assert.False(got.Approximate)
			if tt.n == 3 {
				assert.Equal(20, got.Permutations)
			} else {
				assert.Equal(70, got.Permutations)
			}
		})
	}
}

func TestExperimentComparisonDegenerate(t *testing.T) {
	t.Parallel()
	got := compareExperimentSubjects([]float64{2, 2, 2, 2}, []float64{2, 2, 2, 2})
	assert.InDelta(t, 0.0, got.Delta, 1e-12)
	assert.InDelta(t, 1.0, got.PValue, 1e-12)
	assert.Equal(t, [2]float64{0, 0}, got.Interval)
	assert.InDelta(t, 0.0, got.Threshold, 1e-12)
	got = compareExperimentSubjects([]float64{0, 1, 2, 3, 4, 5, 6, 7}, []float64{8, 9, 10, 11, 12, 13, 14, 15})
	assert.True(t, got.Approximate)
	assert.Equal(t, 4096, got.Permutations)
	assert.GreaterOrEqual(t, got.PValue, 1.0/4097)
}

func TestExperimentComparisonGates(t *testing.T) {
	t.Parallel()
	baseline := ExperimentMetricValue{Value: new(0.0), Subjects: 4, Units: 4, ExpectedUnits: 4, Complete: true}
	treatment := ExperimentMetricValue{Value: new(1.0), Subjects: 4, Units: 4, ExpectedUnits: 4, Complete: true}
	for _, tt := range []struct {
		name, metric           string
		integrity, reliability bool
		n                      int
		complete               bool
		p                      float64
		lo, hi                 float64
		want                   string
	}{
		{"detected lower better", "cost_per_review", true, false, 4, true, .03, -2, -1, "better"},
		{"detected higher worse", "job_failure_rate", true, false, 4, true, .03, 1, 2, "worse"},
		{"descriptive count", "findings_high_per_review", true, false, 4, true, .03, 1, 2, "higher"},
		{"p gate", "cost_per_review", true, false, 4, true, .1, 1, 2, "no detectable difference"},
		{"interval gate", "cost_per_review", true, false, 4, true, .03, -1, 2, "no detectable difference"},
		{"small n", "cost_per_review", true, false, 3, true, .03, 1, 2, "not enough data"},
		{"coverage", "cost_per_review", true, false, 4, false, .03, 1, 2, "not enough data"},
		{"integrity", "cost_per_review", false, false, 4, true, .03, 1, 2, "not enough data"},
		{"reliability", "cost_per_review", true, true, 4, true, .03, 1, 2, "reliability worse"},
		{"availability before reliability", "cost_per_review", true, true, 3, true, .03, 1, 2, "not enough data"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := ExperimentMetricComparison{Metric: tt.metric, Default: baseline, Experiment: treatment, Delta: new(1.0), PValue: new(tt.p), Interval: &ExperimentInterval{Low: tt.lo, High: tt.hi}}
			m.Experiment.Subjects = tt.n
			m.Experiment.Complete = tt.complete
			gateExperimentComparison(&m, 4, tt.integrity, "integrity", tt.reliability)
			assert.Equal(t, tt.want, m.State)
			if tt.want == "not enough data" {
				assert.Nil(t, m.PValue)
			}
		})
	}
}

func FuzzExperimentComparison(f *testing.F) {
	f.Add([]byte{0, 0, 0, 3, 3, 3}, uint8(3))
	f.Add([]byte{2, 2, 2, 2, 2, 2, 2, 2}, uint8(4))
	f.Fuzz(func(t *testing.T, b []byte, cut uint8) {
		if len(b) < 4 {
			t.Skip()
		}
		if len(b) > 16 {
			b = b[:16]
		}
		n := 2 + int(cut)%(len(b)-3)
		d := make([]float64, n)
		e := make([]float64, len(b)-n)
		for i, v := range b {
			value := float64(int(v)-128) / 8
			if i < n {
				d[i] = value
			} else {
				e[i-n] = value
			}
		}
		got := compareExperimentSubjects(d, e)
		require.False(t, math.IsNaN(got.PValue))
		assert.GreaterOrEqual(t, got.PValue, 0.0)
		assert.LessOrEqual(t, got.PValue, 1.0)
		slices.Reverse(d)
		slices.Reverse(e)
		assert.Equal(t, got, compareExperimentSubjects(d, e))
		swapped := compareExperimentSubjects(e, d)
		assert.InDelta(t, -got.Delta, swapped.Delta, 1e-10)
		assert.InDelta(t, got.PValue, swapped.PValue, 1e-10)
		for i := range d {
			d[i] *= 2
		}
		for i := range e {
			e[i] *= 2
		}
		scaled := compareExperimentSubjects(d, e)
		assert.InDelta(t, got.Delta*2, scaled.Delta, 1e-10)
		assert.InDelta(t, got.Threshold*2, scaled.Threshold, 1e-10)
		assert.InDelta(t, got.Interval[0]*2, scaled.Interval[0], 1e-10)
		assert.InDelta(t, got.Interval[1]*2, scaled.Interval[1], 1e-10)
		assert.InDelta(t, got.PValue, scaled.PValue, 1e-10)
	})
}

// Identical arm RNG streams incorrectly cancel sampling variation in shifted
// distributions, yielding [1,1] here despite substantial subject variation.
func TestExperimentComparisonIndependentBootstrap(t *testing.T) {
	t.Parallel()
	got := compareExperimentSubjects([]float64{0, 1, 2, 3}, []float64{1, 2, 3, 4})
	assert.Less(t, got.Interval[0], 0.0)
	assert.Greater(t, got.Interval[1], 1.0)
}
