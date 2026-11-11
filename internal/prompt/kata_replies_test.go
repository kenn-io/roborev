package prompt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/storage"
)

func TestKataContextCachePreservesEvidenceAndErrors(t *testing.T) {
	id := uuid.New()
	review := &storage.Review{UUID: &id, Output: "original finding", Job: &storage.ReviewJob{JobType: storage.JobTypeReview}}
	calls := 0
	evidence := strings.Repeat("conflicting evidence ", 2000) + "</untrusted-kata-discussion>"
	cache := NewKataContextCache(func(context.Context, *storage.Review) (any, error) {
		calls++
		return map[string]any{"author": "reviewer", "body": evidence}, nil
	})
	first, err := cache.Decorate(t.Context(), review)
	require.NoError(t, err)
	second, err := cache.Decorate(t.Context(), review)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, first.Output, second.Output)
	assert.Contains(t, first.Output, "original finding")
	assert.Contains(t, first.Output, strings.Repeat("conflicting evidence ", 2000))
	assert.NotContains(t, first.Output, evidence)
	assert.Equal(t, "original finding", review.Output)
	fatal := fmtFatalForTest()
	calls = 0
	cache = NewKataContextCache(func(context.Context, *storage.Review) (any, error) { calls++; return nil, fatal })
	_, err = cache.Decorate(t.Context(), review)
	require.ErrorIs(t, err, ErrKataContextFatal)
	_, err = cache.Decorate(t.Context(), review)
	require.ErrorIs(t, err, ErrKataContextFatal)
	assert.Equal(t, 1, calls)
	ci := *review
	ci.Job = &storage.ReviewJob{JobType: storage.JobTypeReview, Source: storage.JobSourceCI}
	_, err = cache.Decorate(t.Context(), &ci)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func fmtFatalForTest() error { return errors.Join(ErrKataContextFatal, errors.New("denied")) }
