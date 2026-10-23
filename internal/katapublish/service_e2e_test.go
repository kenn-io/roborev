//go:build kata && !windows

package katapublish_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/internal/katapublish"
)

// Real Kata's lookalike checks and guarded metadata decoding are observable
// protocol behavior that an in-memory receipt fixture cannot establish.
func TestPublicationLedgerE2E(t *testing.T) {
	ctx := t.Context()
	server := katatest.StartReal(t, ctx)
	cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: server.SocketEndpoint(t, ctx), DefaultProject: "e2etest", Teammate: "roborev"}
	client, err := kata.NewAPIClient(server.Repo, cfg)
	require.NoError(t, err)
	defer client.CloseIdleConnections()
	binding, err := client.ResolveProject(ctx, "e2etest")
	require.NoError(t, err)
	request := katapublish.Request{OperationID: "publication-full", Endpoint: client.Endpoint(), Project: "e2etest", ProjectUID: binding.ProjectUID, Policy: "comments", ReviewUUID: "review-e2e", Teammate: "roborev", Create: kata.CreateReq{Title: "Review findings", Body: "Inspect the review locally", ForceNew: true}, Entries: []katapublish.Entry{{Selector: "f:full", ReviewUUID: "review-e2e", DocumentHash: strings.Repeat("a", 64), Body: "SYNTHETIC_FULL_FINDING"}}}
	var saved katapublish.State
	checkpoint := func(state katapublish.State) error { saved = state; return nil }
	full, err := katapublish.Deliver(ctx, client, request, katapublish.State{}, checkpoint)
	require.NoError(t, err)
	assert.True(t, full.Done)
	// Without explicit distinct-publication authorization the real provider
	// refuses this identical title/body, even under an unrelated replay key.
	_, err = client.Create(ctx, kata.CreateReq{Title: request.Create.Title, Body: request.Create.Body, IdempotencyKey: "unrelated-lookalike"})
	require.Error(t, err)
	assert.Equal(t, "duplicate_candidates", kata.ErrorCode(err))
	summaryRequest := request
	summaryRequest.OperationID = "publication-summary"
	summaryRequest.Policy = "summary"
	summaryRequest.Entries = []katapublish.Entry{{Selector: "f:summary", ReviewUUID: "review-e2e", DocumentHash: strings.Repeat("a", 64), Body: "Finding details are available locally."}}
	summary, err := katapublish.Deliver(ctx, client, summaryRequest, katapublish.State{}, checkpoint)
	require.NoError(t, err)
	assert.True(t, summary.Done)
	assert.NotEqual(t, full.IssueUID, summary.IssueUID)
	for i := range 55 {
		_, err = client.Comment(ctx, kata.CommentReq{Project: "e2etest", Issue: full.IssueUID, Body: fmt.Sprintf("Unrelated comment %d", i), IdempotencyKey: fmt.Sprintf("unrelated-comment-%d", i)})
		require.NoError(t, err)
	}
	recovered := full
	recovered.Receipts = nil
	recovered.FirstAttempt = map[string]time.Time{"issue": time.Now().Add(-8 * 24 * time.Hour), "f:full": time.Now().Add(-8 * 24 * time.Hour)}
	recovered, err = katapublish.Deliver(context.Background(), client, request, recovered, checkpoint)
	require.NoError(t, err)
	assert.True(t, recovered.Done)
	assert.Equal(t, full.Receipts["f:full"].UID, recovered.Receipts["f:full"].UID)
	assert.Equal(t, recovered.IssueUID, saved.IssueUID)
	complete, err := client.ShowComplete(ctx, full.IssueRef)
	require.NoError(t, err)
	assert.Len(t, complete.Comments, 56)
}
