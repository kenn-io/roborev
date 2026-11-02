package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/provenance"
	"go.kenn.io/roborev/internal/storage"
)

func TestReviewContentSessionProvenance(t *testing.T) {
	t.Parallel()
	review := &storage.Review{Output: "review output", Job: &storage.ReviewJob{SessionProvenance: []provenance.Commit{{CommitSHA: "0123456789abcdef0123456789abcdef01234567", Coverage: "trailers", Sessions: []provenance.Session{{SessionID: "pi:writer", Agent: "pi", Source: "trailer", Availability: "unknown", WebURL: "\x1b[2Junsafe"}}}}}}
	out := (model{}).reviewContentString(review)
	assert.Contains(t, out, "pi:writer")
	assert.Contains(t, out, "review output")
	assert.NotContains(t, out, "\x1b")
}
