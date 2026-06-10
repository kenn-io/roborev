package kata

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseRefs(t *testing.T) {
	tests := []struct {
		name    string
		msg     string
		project string
		want    []string
	}{
		{"kata marker", "Closes: kata#abc4", "roborev", []string{"abc4"}},
		{"bound project ref", "fix roborev#abc4", "roborev", []string{"abc4"}},
		{"both forms dedup", "kata#abc4 and roborev#abc4", "roborev", []string{"abc4"}},
		{"two distinct refs", "kata#abc4 kata#def5", "roborev", []string{"abc4", "def5"}},
		{"uppercase normalized", "KATA#ABC4", "roborev", []string{"abc4"}},
		{"no refs", "just a normal commit", "roborev", nil},
		{"word boundary", "mykata#abc4", "roborev", nil},
		{"empty project only kata", "kata#abc4 roborev#def5", "", []string{"abc4"}},
		{"foreign project ignored", "other#abc4", "roborev", nil},
		{"non-crockford id rejected", "see kata#look", "roborev", nil},
		{"crockford boundary chars", "kata#hjkm", "roborev", []string{"hjkm"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ParseRefs(tt.msg, tt.project))
		})
	}
}
