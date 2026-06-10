package katatest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
)

func TestFakeClientImplementsInterface(t *testing.T) {
	var _ kata.Client = &FakeClient{}
}

func TestFakeShowRecordsRefs(t *testing.T) {
	f := &FakeClient{Issues: map[string]kata.Issue{"abc4": {ShortID: "abc4", Title: "T"}}}
	iss, err := f.Show(context.Background(), "abc4")
	require.NoError(t, err)
	assert.Equal(t, "T", iss.Title)
	assert.Equal(t, []string{"abc4"}, f.ShowRefs)
}

func TestFakeCreateRecordsReqs(t *testing.T) {
	f := &FakeClient{CreateResult: kata.CreateResult{ShortID: "z1"}}
	_, err := f.Create(context.Background(), kata.CreateReq{Title: "t"})
	require.NoError(t, err)
	require.Len(t, f.CreateReqs, 1)
	assert.Equal(t, "t", f.CreateReqs[0].Title)
}
