package rebuttal

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAssessment(t *testing.T) {
	for _, outcome := range []string{"confirmed", "refuted", "superseded"} {
		t.Run(outcome, func(t *testing.T) {
			a, err := Parse(`{"outcome":"` + outcome + `","explanation":"Verified against the source: the guard handles this case."}`)
			require.NoError(t, err)
			assert.Equal(t, outcome, a.Outcome)
		})
	}
	for _, input := range []string{"", `null`, `{}`, `{"outcome":"pass","explanation":"long"}`, `{"outcome":"refuted","explanation":""}`, `{"outcome":"refuted","explanation":"   "}`, `{"outcome":"refuted","explanation":"Verified against the source: the guard handles this case.","extra":true}`, `{"outcome":"refuted","explanation":"Verified against the source: the guard handles this case."} {}`} {
		_, err := Parse(input)
		assert.Error(t, err, input)
	}
}

func TestParseAssessmentAllowsConciseExplanation(t *testing.T) {
	const explanation = "The caller checks the value."
	a, err := Parse(`{"outcome":"refuted","explanation":"` + explanation + `"}`)
	require.NoError(t, err)
	assert.Equal(t, explanation, a.Explanation)
}

func FuzzAssessmentRoundTrip(f *testing.F) {
	f.Add("refuted", "The original finding does not apply because the guard is present.")
	f.Add("invalid", "")
	f.Add("refuted", "\xff")
	f.Add("\xff", "The explanation remains valid UTF-8.")
	f.Fuzz(func(t *testing.T, outcome, explanation string) {
		outcome = strings.ToValidUTF8(outcome, "\uFFFD")
		explanation = strings.ToValidUTF8(explanation, "\uFFFD")
		b, err := json.Marshal(Assessment{Outcome: outcome, Explanation: explanation})
		require.NoError(t, err)
		a, err := Parse(string(b))
		if err != nil {
			return
		}
		assert.Contains(t, []string{"confirmed", "refuted", "superseded"}, a.Outcome)
		assert.NotEmpty(t, a.Explanation)
	})
}

func TestParseAssessmentSingleFence(t *testing.T) {
	payload := `{"outcome":"refuted","explanation":"Verified the guard rejects this input before reaching the handler."}`
	for _, header := range []string{"```json", "```", "```JSON"} {
		a, err := Parse(header + "\n" + payload + "\n```")
		require.NoError(t, err)
		assert.Equal(t, "refuted", a.Outcome)
	}
	for _, input := range []string{
		"```json\n" + payload,
		"```go\n" + payload + "\n```",
		"```json\n" + payload + "\n```\nExtra prose",
		"```json\n" + payload + "\n```\n```json\n" + payload + "\n```",
		"```json\n" + payload + " {}\n```",
	} {
		_, err := Parse(input)
		require.Error(t, err)
	}
}

func TestAssessmentErrorsWithholdUntrustedContent(t *testing.T) {
	for _, input := range []string{`{"outcome":"confidential-sentinel","explanation":"evidence"}`, `{"outcome":"refuted","explanation":"evidence","confidential-sentinel":true}`} {
		_, err := Parse(input)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "confidential-sentinel")
	}
}
