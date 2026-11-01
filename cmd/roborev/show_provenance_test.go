package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/provenance"
	"go.kenn.io/roborev/internal/storage"
)

func TestShowSessionProvenanceSanitizesControls(t *testing.T) {
	job := &storage.ReviewJob{SessionProvenance: []provenance.Commit{{CommitSHA: "0123456789abcdef0123456789abcdef01234567", Coverage: "why-selected", Sessions: []provenance.Session{{SessionID: "pi:writer", Agent: "pi", Source: "transcript", Availability: "available", Turn: &provenance.Turn{Prompt: "first\x1b[2Jsecond", Why: "commit-call"}}}}}}
	out := formatShowSessionProvenance(job)
	assert.Contains(t, out, "pi:writer")
	assert.Contains(t, out, "up to three sessions")
	assert.NotContains(t, out, "\x1b")
	assert.Empty(t, formatShowSessionProvenance(nil))
}
