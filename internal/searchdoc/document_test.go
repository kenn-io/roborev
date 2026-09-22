package searchdoc_test

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/storage"
)

func TestSearchDocumentRendersStructuredReviewDeterministically(t *testing.T) {
	timestamp := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
	responseUUIDA := uuid.MustParse("00000000-0000-4000-8000-00000000000a")
	responseUUIDB := uuid.MustParse("00000000-0000-4000-8000-00000000000b")
	source := storage.SearchReviewSource{
		ReviewID:      42,
		JobID:         24,
		RepoID:        7,
		ReviewUUID:    "00000000-0000-4000-8000-000000000042",
		JobUUID:       "00000000-0000-4000-8000-000000000024",
		RepoName:      "widgets",
		Branch:        "feature/search",
		GitRef:        "abc1234",
		CommitSHA:     "abc1234567890abcdef1234567890abcdef12345",
		CommitSubject: "Keep review history searchable",
		ReviewType:    "security",
		PanelRole:     storage.PanelRoleMember,
		PanelRunUUID:  "00000000-0000-4000-8000-000000000099",
		Agent:         "codex",
		Verdict:       "fail",
		StructuredOutput: storage.StructuredOutput{
			"schema_version": float64(2),
			"summary":        "The retry state can be lost.",
			"verdict":        "fail",
			"findings": []any{
				map[string]any{
					"severity": "high",
					"location": "worker.go:42",
					"problem":  "The cursor advances before persistence.",
					"fix":      "Persist the cursor before acknowledging the batch.",
				},
				map[string]any{
					"severity": "low",
					"location": nil,
					"problem":  "The helper name hides its purpose.",
					"fix":      "Rename the helper.",
				},
			},
		},
		Responses: []storage.Response{
			{ID: 8, Responder: "zoe", Response: "Second by UUID.", CreatedAt: timestamp, UUID: &responseUUIDB},
			{ID: 7, Responder: "alice", Response: "First by ID.", CreatedAt: timestamp},
			{ID: 8, Responder: "bob", Response: "First by UUID.", CreatedAt: timestamp, UUID: &responseUUIDA},
			{ID: 9, Responder: "sam", Response: "Earlier response.", CreatedAt: timestamp.Add(-time.Minute)},
		},
	}

	wantContent := "Commit: Keep review history searchable\n" +
		"Review type: security\n" +
		"Verdict: fail\n" +
		"Summary: The retry state can be lost.\n" +
		"Finding: high worker.go:42\n" +
		"Problem: The cursor advances before persistence.\n" +
		"Fix: Persist the cursor before acknowledging the batch.\n" +
		"Finding: low\n" +
		"Problem: The helper name hides its purpose.\n" +
		"Fix: Rename the helper.\n" +
		"Response by sam: Earlier response.\n" +
		"Response by bob: First by UUID.\n" +
		"Response by zoe: Second by UUID.\n" +
		"Response by alice: First by ID."
	wantIdentifiers := "widgets\nfeature/search\nabc1234\n" +
		"abc1234567890abcdef1234567890abcdef12345\n" +
		"00000000-0000-4000-8000-000000000024\n" +
		"00000000-0000-4000-8000-000000000042\n" +
		"security\ncodex"
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(wantContent)))

	doc := searchdoc.Render(source)
	assert.Equal(t, source.ReviewUUID, doc.DocKey)
	assert.Equal(t, source.PanelRunUUID, doc.GroupKey)
	assert.Equal(t, wantContent, doc.Content)
	assert.Equal(t, wantHash, doc.ContentHash)
	assert.Equal(t, wantIdentifiers, doc.Identifiers)
	assert.Equal(t, source, doc.Source)
}

func TestSearchDocumentRendersLegacyReviewWithLocalKey(t *testing.T) {
	source := storage.SearchReviewSource{
		ReviewID:   17,
		JobID:      11,
		JobUUID:    "00000000-0000-4000-8000-000000000011",
		RepoName:   "widgets",
		GitRef:     "base..head",
		ReviewType: "default",
		Agent:      "claude",
		Verdict:    "pass",
		Output:     "No actionable findings.",
	}

	doc := searchdoc.Render(source)
	assert.Equal(t, "local:17", doc.DocKey)
	assert.Equal(t, source.JobUUID, doc.GroupKey)
	assert.Equal(t, "Review type: default\nVerdict: pass\nReview: No actionable findings.", doc.Content)
	assert.Equal(t, "widgets\nbase..head\n00000000-0000-4000-8000-000000000011\ndefault\nclaude", doc.Identifiers)
}

func TestSearchDocumentResponseOrderingStableAcrossLocalIDs(t *testing.T) {
	timestamp := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
	responseUUIDA := uuid.MustParse("00000000-0000-4000-8000-00000000000a")
	responseUUIDB := uuid.MustParse("00000000-0000-4000-8000-00000000000b")
	base := storage.SearchReviewSource{
		ReviewID:   17,
		JobID:      11,
		JobUUID:    "00000000-0000-4000-8000-000000000011",
		ReviewType: "default",
		Output:     "Review body.",
	}

	firstDatabase := base
	firstDatabase.Responses = []storage.Response{
		{ID: 1, Responder: "uuid-b", Response: "Stable B.", CreatedAt: timestamp, UUID: &responseUUIDB},
		{ID: 2, Responder: "legacy", Response: "Legacy response.", CreatedAt: timestamp},
		{ID: 3, Responder: "uuid-a", Response: "Stable A.", CreatedAt: timestamp, UUID: &responseUUIDA},
	}
	secondDatabase := base
	secondDatabase.Responses = []storage.Response{
		{ID: 30, Responder: "uuid-b", Response: "Stable B.", CreatedAt: timestamp, UUID: &responseUUIDB},
		{ID: 20, Responder: "legacy", Response: "Legacy response.", CreatedAt: timestamp},
		{ID: 10, Responder: "uuid-a", Response: "Stable A.", CreatedAt: timestamp, UUID: &responseUUIDA},
	}

	first := searchdoc.Render(firstDatabase)
	second := searchdoc.Render(secondDatabase)
	assert.Equal(t, first.Content, second.Content)
	assert.Equal(t, first.ContentHash, second.ContentHash)
	assert.Less(t, strings.Index(first.Content, "Stable A."), strings.Index(first.Content, "Stable B."))
	assert.Less(t, strings.Index(first.Content, "Stable B."), strings.Index(first.Content, "Legacy response."))
}

func TestSearchDocumentRendersRestoredLegacyMarkdown(t *testing.T) {
	source := storage.SearchReviewSource{
		StructuredOutput: storage.StructuredOutput{
			"schema_version": 0,
			"legacy": map[string]any{
				"markdown":         "### High\nA pending update is lost.",
				"recorded_verdict": false,
			},
		},
	}

	assert.Equal(t, "Review: ### High\nA pending update is lost.", searchdoc.Render(source).Content)
}
