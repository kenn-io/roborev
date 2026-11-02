package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatchingCISkipLabel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		labels     []string
		skipLabels []string
		label      string
	}{
		{"normalization", []string{" Do Not Review "}, []string{" do not review "}, "Do Not Review"},
		{"empty labels", []string{"  "}, []string{" "}, ""},
		{"unmatched", []string{"review"}, []string{"skip"}, ""},
		{"first match", []string{"SECOND", "First"}, []string{"first", "second"}, "SECOND"},
		{"unicode fold", []string{"K"}, []string{"k"}, "K"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label, skip := matchingCISkipLabel(tc.labels, tc.skipLabels)
			assert.Equal(t, tc.label, label)
			assert.Equal(t, tc.label != "", skip)
		})
	}
}

func FuzzMatchingCISkipLabel(f *testing.F) {
	f.Add(" skip-review ")
	f.Add("")
	f.Add("review")
	f.Add("K")
	f.Fuzz(func(t *testing.T, label string) {
		// Adding surrounding whitespace must not change whether a label
		// matches itself, including arbitrary Unicode and empty input.
		got, skip := matchingCISkipLabel([]string{" \t" + label + "\n"}, []string{label})
		want := strings.TrimSpace(label)
		assert.Equal(t, want, got)
		assert.Equal(t, want != "", skip)
	})
}
