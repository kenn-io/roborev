package config

import (
	"math"
	"strconv"
	"testing"

	tomlv2 "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This literal was captured from the canonicalizer before success existed.
func TestExperimentSuccessHashCompatibility(t *testing.T) {
	definition := ExperimentDefinition{Ratio: new(0.5), Workflows: []ExperimentWorkflow{ExperimentWorkflowReview}, Config: map[string]any{"reuse_review_session": true}}
	encoded, hash, err := canonicalizeExperimentDefinition(definition)
	require.NoError(t, err)
	assert.Equal(t, `{"ratio":0.5,"workflows":["review"],"config":{"reuse_review_session":true}}`, string(encoded)) //nolint:testifylint // encoded-compare: canonical bytes must stay identical for hash compatibility.
	assert.Equal(t, "9ffe6eda74586fe06bafd800c17b93d44441a4a57105376e5ef6a30f6d684e90", hash)
}

func successRepoTOML(success string) string {
	return "[experiments.cost-v1]\nenabled=true\nratio=0.5\nworkflows=[\"review\"]\n[experiments.cost-v1.config]\nreuse_review_session=true\n[experiments.cost-v1.success]\n" + success
}

func TestExperimentSuccessValidation(t *testing.T) {
	t.Run("unknown metric", func(t *testing.T) {
		dir := t.TempDir()
		writeRepoConfigStr(t, dir, successRepoTOML("metric='unknown'\ndirection='lower'\nmin_effect='0'\n"))
		_, err := LoadRepoConfig(dir)
		require.Error(t, err)
	})
	t.Run("canonical success", func(t *testing.T) {
		dir := t.TempDir()
		writeRepoConfigStr(t, dir, successRepoTOML("metric='cost_per_review'\ndirection='lower'\nmin_effect='10%'\nguards=['job_failure_rate <= +0.01']\n"))
		cfg, err := LoadRepoConfig(dir)
		require.NoError(t, err)
		encoded, _, err := canonicalizeExperimentDefinition(cfg.Experiments["cost-v1"])
		require.NoError(t, err)
		assert.Contains(t, string(encoded), `"success":{"metric":"cost_per_review","direction":"lower","min_effect":"10%","guards":["job_failure_rate <= +0.01"]}`)
	})
}

func TestExperimentSuccessNumericTOML(t *testing.T) {
	dir := t.TempDir()
	writeRepoConfigStr(t, dir, successRepoTOML("metric='cost_per_review'\ndirection='lower'\nmin_effect=0.2\n"))
	cfg, err := LoadRepoConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "0.2", cfg.Experiments["cost-v1"].Success.MinEffect)
}

func TestExperimentSuccessImmutable(t *testing.T) {
	definition := ExperimentDefinition{Enabled: new(true), Ratio: new(0.5), Workflows: []ExperimentWorkflow{ExperimentWorkflowReview}, Config: map[string]any{"reuse_review_session": true}, Success: &ExperimentSuccess{Metric: "cost_per_review", Direction: "lower", MinEffect: "10%", Guards: []string{"job_failure_rate <= +0.01"}}}
	global := &Config{Experiments: map[string]ExperimentDefinition{"cost-v1": definition}}
	_, hash, err := canonicalizeExperimentDefinition(definition)
	require.NoError(t, err)
	merged, err := mergeExperimentDefinitions(global, &RepoConfig{Experiments: map[string]ExperimentDefinition{"cost-v1": {Enabled: new(false)}}})
	require.NoError(t, err)
	copied := merged["cost-v1"]
	copied.Success.MinEffect = "20%"
	copied.Success.Guards[0] = "job_failure_rate <= +0.02"
	assert.Equal(t, "10%", global.Experiments["cost-v1"].Success.MinEffect)
	assert.Equal(t, "job_failure_rate <= +0.01", global.Experiments["cost-v1"].Success.Guards[0])
	_, changed, err := canonicalizeExperimentDefinition(copied)
	require.NoError(t, err)
	assert.NotEqual(t, hash, changed)
	_, err = mergeExperimentDefinitions(global, &RepoConfig{Experiments: map[string]ExperimentDefinition{"cost-v1": {Enabled: new(true), Success: copied.Success}}})
	require.ErrorContains(t, err, "new experiment ID")
	require.Error(t, validateExperimentEntries(map[string]ExperimentDefinition{"new-v1": {Enabled: new(true), Success: copied.Success}}, false))
}

func TestExperimentSuccessMalformedEffects(t *testing.T) {
	for _, text := range []string{"", "-1", "-0.1%", "NaN", "+Inf", "-Inf%", "1e309", "1%%", "0x1p2", "1_000", "1 %", "1+2"} {
		t.Run(text, func(t *testing.T) { _, err := ParseExperimentMinEffect(text); require.Error(t, err) })
	}
	for _, text := range []string{"unknown <= 1", "cost_per_review < 1", "cost_per_review => 1", "cost_per_review <= NaN", "cost_per_review <= 1 extra", "cost_per_review <= 1% + 2", "cost_per_review == 1"} {
		t.Run(text, func(t *testing.T) { _, err := ParseExperimentGuard(text); require.Error(t, err) })
	}
	require.Error(t, ValidateExperimentSuccess(&ExperimentSuccess{Metric: "cost_per_review", Direction: "sideways", MinEffect: "0"}))
	require.Error(t, ValidateExperimentSuccess(&ExperimentSuccess{Metric: "cost_per_review", Direction: "lower", MinEffect: "0", Guards: []string{"unknown <= 1"}}))
}

func TestExperimentSuccessSignedRelativeGuard(t *testing.T) {
	guard, err := ParseExperimentGuard(" cost_per_review >= -12.5% ")
	require.NoError(t, err)
	assert.Equal(t, "cost_per_review", guard.Metric)
	assert.Equal(t, ">=", guard.Operator)
	assert.InDelta(t, -12.5, guard.Effect.Value, 0)
	assert.True(t, guard.Effect.Relative)
}

func FuzzExperimentSuccessParsing(f *testing.F) {
	for _, seed := range []string{"0", "1.5", "20%", "-1", "NaN", "+Inf", "1e309", "cost_per_review <= +30%", "job_failure_rate >= -0.01", "cost_per_review < 1"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		effect, err := ParseExperimentMinEffect(text)
		if err == nil {
			assert.GreaterOrEqual(t, effect.Value, 0.0)
			assert.False(t, math.IsNaN(effect.Value))
			assert.False(t, math.IsInf(effect.Value, 0))
			encoded := strconv.FormatFloat(effect.Value, 'g', -1, 64)
			if effect.Relative {
				encoded += "%"
			}
			again, parseErr := ParseExperimentMinEffect(encoded)
			require.NoError(t, parseErr)
			assert.Equal(t, effect, again)
		}
		guard, err := ParseExperimentGuard(text)
		if err == nil {
			assert.Contains(t, ExperimentReportMetrics(), guard.Metric)
			assert.Contains(t, []string{"<=", ">="}, guard.Operator)
			assert.False(t, math.IsNaN(guard.Effect.Value))
			assert.False(t, math.IsInf(guard.Effect.Value, 0))
			encoded := guard.Metric + " " + guard.Operator + " " + strconv.FormatFloat(guard.Effect.Value, 'g', -1, 64)
			if guard.Effect.Relative {
				encoded += "%"
			}
			again, parseErr := ParseExperimentGuard(encoded)
			require.NoError(t, parseErr)
			assert.Equal(t, guard, again)
		}
	})
}

func TestExperimentSuccessTOMLNonfiniteMinimum(t *testing.T) {
	for _, minimum := range []string{"nan", "inf", "-inf", "-0.1", "true"} {
		t.Run(minimum, func(t *testing.T) {
			dir := t.TempDir()
			writeRepoConfigStr(t, dir, successRepoTOML("metric='cost_per_review'\ndirection='lower'\nmin_effect="+minimum+"\n"))
			_, err := LoadRepoConfig(dir)
			require.Error(t, err)
		})
	}
}

func TestExperimentSuccessTOMLRoundtrip(t *testing.T) {
	for _, minimum := range []string{"0", "0.2", "'20%'"} {
		t.Run(minimum, func(t *testing.T) {
			dir := t.TempDir()
			writeRepoConfigStr(t, dir, successRepoTOML("metric='cost_per_review'\ndirection='lower'\nmin_effect="+minimum+"\nguards=['job_failure_rate <= +0']\n"))
			original, err := LoadRepoConfig(dir)
			require.NoError(t, err)
			before, hash, err := canonicalizeExperimentDefinition(original.Experiments["cost-v1"])
			require.NoError(t, err)
			encoded, err := tomlv2.Marshal(original)
			require.NoError(t, err)
			writeRepoConfigStr(t, dir, string(encoded))
			reloaded, err := LoadRepoConfig(dir)
			require.NoError(t, err)
			after, reloadedHash, err := canonicalizeExperimentDefinition(reloaded.Experiments["cost-v1"])
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
			assert.Equal(t, hash, reloadedHash)
		})
	}
}
