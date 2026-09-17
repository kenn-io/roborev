package storage_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestCompareResponsesUsesStableCrossDatabaseOrder(t *testing.T) {
	timestamp := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
	firstUUID := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	secondUUID := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	responses := []storage.Response{
		{ID: 4, Response: "legacy", CreatedAt: timestamp},
		{ID: 30, UUID: &secondUUID, Response: "second UUID", CreatedAt: timestamp},
		{ID: 20, UUID: &firstUUID, Response: "first UUID, second local ID", CreatedAt: timestamp},
		{ID: 10, UUID: &firstUUID, Response: "first UUID, first local ID", CreatedAt: timestamp},
		{ID: 5, Response: "earlier", CreatedAt: timestamp.Add(-time.Minute)},
	}

	slices.SortFunc(responses, storage.CompareResponses)

	assert.Equal(t, []string{
		"earlier",
		"first UUID, first local ID",
		"first UUID, second local ID",
		"second UUID",
		"legacy",
	}, responseBodies(responses))
}

func responseBodies(responses []storage.Response) []string {
	bodies := make([]string, len(responses))
	for i, response := range responses {
		bodies[i] = response.Response
	}
	return bodies
}
