package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey() storeKey {
	return storeKey{
		Harness: "codex", Model: "gpt-x", Reasoning: "thorough",
		PromptVersion: "p-0000000a", Reviewer: "stock", PR: "owner_repo_1-abcdef01",
	}
}

func TestStoreReusesOnlyTheSamePrompt(t *testing.T) {
	assert := assert.New(t)
	root, k := t.TempDir(), testKey()

	dir, err := putSample(root, k, sampleMeta{PromptSHA256: "hash-a", CreatedAt: time.Now()}, []byte(`{"a":1}`), "a")
	require.NoError(t, err)
	assert.Equal(filepath.Join(k.dir(root), "1"), dir)

	found, err := findSample(root, k, "hash-a")
	require.NoError(t, err)
	assert.Equal(dir, found)

	found, err = findSample(root, k, "hash-b")
	require.NoError(t, err)
	assert.Empty(found, "a review from another prompt is not reused")

	found, err = findSample(root, k, "")
	require.NoError(t, err)
	assert.Empty(found, "an unknown prompt hash never matches")
}

func TestStoreNumbersSamplesAndKeepsIdenticalReviews(t *testing.T) {
	assert := assert.New(t)
	root, k := t.TempDir(), testKey()

	first, err := putSample(root, k, sampleMeta{PromptSHA256: "h"}, []byte(`{"n":1}`), "1")
	require.NoError(t, err)
	second, err := putSample(root, k, sampleMeta{PromptSHA256: "h"}, []byte(`{"n":2}`), "2")
	require.NoError(t, err)
	again, err := putSample(root, k, sampleMeta{PromptSHA256: "h"}, []byte(`{"n":2}`), "2")
	require.NoError(t, err)

	assert.Equal("1", filepath.Base(first))
	assert.Equal("2", filepath.Base(second))
	assert.Equal("3", filepath.Base(again), "two reviews that match byte for byte are two samples")

	dirs, metas, err := samples(root, k)
	require.NoError(t, err)
	assert.Len(dirs, 3)
	assert.Equal(sha256Hex([]byte(`{"n":1}`)), metas[0].ReviewSHA256)
}

func TestStoreRefusesChangedInstructionsUnderOneName(t *testing.T) {
	root, k := t.TempDir(), testKey()
	k.Reviewer = "systematic"
	_, err := putSample(root, k, sampleMeta{Instructions: "old"}, []byte(`{}`), "")
	require.NoError(t, err)

	_, err = putSample(root, k, sampleMeta{Instructions: "new"}, []byte(`{"x":1}`), "")
	assert.ErrorContains(t, err, "other instructions")
}

func TestReviewerName(t *testing.T) {
	tests := []struct {
		member, reviewType, instructions, want string
	}{
		{"regular", "default", "", "stock"},
		{"", "", "", "stock"},
		{"security", "security", "", "security"},
		{"systematic", "default", "Use a method.", "systematic"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, reviewerName(tt.member, tt.reviewType, tt.instructions))
	}
}

func TestHashablePromptMasksTheDate(t *testing.T) {
	a := "Intro\n\nCurrent date: 2026-10-21 (UTC)\n\nDiff"
	b := "Intro\n\nCurrent date: 2026-10-22 (UTC)\n\nDiff"
	assert.Equal(t, hashablePrompt(a), hashablePrompt(b))
	assert.NotEqual(t, hashablePrompt(a), hashablePrompt("Intro\n\nCurrent date: 2026-10-21 (UTC)\n\nOther diff"))
}
