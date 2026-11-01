package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCISkipReason(t *testing.T) {
	for _, tc := range []struct {
		name       string
		draft      bool
		labels     []string
		skipDrafts bool
		skipLabels []string
		reason     string
	}{
		{"draft opt in", true, nil, true, nil, "Review skipped: draft"},
		{"draft default", true, nil, false, nil, ""},
		{"ready", false, nil, true, nil, ""},
		{"label normalization", false, []string{" Do Not Review "}, false, []string{" do not review "}, "Review skipped: label Do Not Review"},
		{"label on default draft", true, []string{"Skip"}, false, []string{"skip"}, "Review skipped: label Skip"},
		{"draft precedence", true, []string{"Skip"}, true, []string{"skip"}, "Review skipped: draft"},
		{"empty labels", false, []string{"  "}, false, []string{" "}, ""},
		{"unmatched", false, []string{"review"}, false, []string{"skip"}, ""},
		{"first match", false, []string{"SECOND", "First"}, false, []string{"first", "second"}, "Review skipped: label SECOND"},
		{"unicode fold", false, []string{"K"}, false, []string{"k"}, "Review skipped: label K"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, skip := ciSkipReason(tc.draft, tc.labels, tc.skipDrafts, tc.skipLabels)
			assert.Equal(t, tc.reason, reason)
			assert.Equal(t, tc.reason != "", skip)
		})
	}
}

func FuzzCISkipReason(f *testing.F) {
	f.Add(false, false, " skip-review ")
	f.Add(true, true, "")
	f.Add(true, false, "review")
	f.Add(false, true, "K")
	f.Fuzz(func(t *testing.T, draft, enabled bool, label string) {
		// Adding surrounding whitespace must not change whether a label
		// matches itself, including arbitrary Unicode and empty input.
		reason, skip := ciSkipReason(draft, []string{" \t" + label + "\n"}, enabled, []string{label})
		want := ""
		if draft && enabled {
			want = "Review skipped: draft"
		} else if trimmed := strings.TrimSpace(label); trimmed != "" {
			want = "Review skipped: label " + trimmed
		}
		assert.Equal(t, want, reason)
		assert.Equal(t, want != "", skip)
	})
}
