package katapublish

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katafake"
)

// Exercise the real provider adapter, including ULID targets, typed errors,
// and metadata mutation responses that do not contain comment receipts.
func TestResponseDeliveryThroughKataAPI(t *testing.T) {
	server := katafake.New(t)
	stored := server.AddIssue(7, "one", "Review findings", "")
	client, err := kata.NewAPIClient("", config.KataIntegrationConfig{Enabled: true, Endpoint: server.URL(), APIKey: "test-credential", DefaultProject: "reviews", Teammate: "roborev"}, kata.WithoutWorkspace())
	require.NoError(t, err)
	publication := fixtureRequest()
	publication.Endpoint = client.Endpoint()
	publication.ProjectUID = server.Projects[0].UID
	fingerprint, err := Fingerprint(publication)
	require.NoError(t, err)
	env, err := newEnvelope(publication, publication.ProjectUID)
	require.NoError(t, err)
	root, err := client.Comment(t.Context(), kata.CommentReq{Project: "reviews", Issue: stored.Out.UID, Body: publication.Entries[0].Body + "\n\n" + marker(publication, publication.Entries[0].Selector), Teammate: publication.Teammate, IdempotencyKey: "root"})
	require.NoError(t, err)
	env.Entries[0].Pending = ""
	env.Entries[0].ReceiptUID = root.UID
	env.Entries[0].AttributionHash = attributionHash(root.Attribution)
	stored.Out.Metadata = map[string]any{ActionKey: action(publication, publication.ProjectUID), FingerprintKey: fingerprint, EnvelopeKey: env}
	server.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/metadata") {
			return false
		}
		var body struct {
			Patch map[string]any `json:"patch"`
			Guard struct {
				Key     string `json:"key"`
				IfValue string `json:"if_value"`
			} `json:"guard"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		old, err := json.Marshal(stored.Out.Metadata[body.Guard.Key], json.Deterministic(true))
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		// Kata v0.19.0 CheckMetadataPatchGuard compares normalized JSON values,
		// not object member order after HTTP decoding changes structs into maps.
		actual, expected := jsontext.Value(old), jsontext.Value(body.Guard.IfValue)
		if !assert.NoError(t, actual.Canonicalize()) || !assert.NoError(t, expected.Canonicalize()) {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		if string(actual) != string(expected) {
			server.Fail(w, http.StatusConflict, "metadata_guard_failed", "guard changed")
			return true
		}
		maps.Copy(stored.Out.Metadata, body.Patch)
		server.Reply(w, gen.PatchIssueMetadataResponseBody{Issue: gen.Issue{ShortID: stored.Out.ShortID, UID: stored.Out.UID, ProjectUID: stored.Out.ProjectUID, Metadata: stored.Out.Metadata}})
		return true
	}
	req := publication
	req.RootAttributionHash = attributionHash(root.Attribution)
	req.OperationID = uuid.New().String()
	req.IssueUID = stored.Out.UID
	req.PublicationOperationID = publication.OperationID
	req.PublicationFingerprint = fingerprint
	req.PublicationProjectUID = publication.ProjectUID //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
	req.Responses = []Response{{Root: root.UID, Target: root.UID, OperationID: req.OperationID, Fingerprint: "intent", Body: "The caller validates this input before reaching the selected path.", Kind: "refute", Teammate: "reviewer"}}
	req.PublicationEntries = append([]LedgerEntry{}, env.Entries...)
	state, err := Deliver(t.Context(), client, req, State{}, func(State) error { return nil })
	require.NoError(t, err)
	assert.True(t, state.Done)
	firstReceipt := state.Receipts[req.OperationID].UID
	assert.NotEmpty(t, firstReceipt)
	// Inspect the body actually posted by Deliver through the native API.
	// Keep the publisher's true recovery suffix through every subsequent edit.
	suffix := "\n\n" + marker(req, req.OperationID)
	reader, err := kata.NewAPIClient("", config.KataIntegrationConfig{Enabled: true, Endpoint: server.URL(), APIKey: "test-credential", DefaultProject: "reviews"}, kata.WithoutWorkspace())
	require.NoError(t, err)
	defer reader.CloseIdleConnections()
	shown, err := reader.ShowComplete(t.Context(), stored.Out.UID)
	require.NoError(t, err)
	require.Len(t, shown.Comments, 2)
	assert.Equal(t, req.Responses[0].Body+suffix, shown.Comments[1].Body)
	assert.Equal(t, req.Responses[0].Body, shown.Comments[1].Evidence)
	assert.Equal(t, "unspecified", shown.Comments[1].Reason)
	assert.Equal(t, "absent", shown.Comments[1].ReasonState)
	assert.True(t, shown.Comments[1].EvidenceValid)
	original := stored.Comments[1]
	edited := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, reason := range []string{"", "false-positive", "spurious", "risk-accepted"} {
		prefix := ""
		state, wantReason := "absent", "unspecified"
		if reason != "" {
			prefix = "<!-- roborev-refute:v1 reason=" + reason + " -->\n"
			state, wantReason = "explicit", reason
		}
		for _, evidence := range []string{"", "short", strings.Repeat("界", 39), strings.Repeat("界", 40)} {
			raw := prefix + evidence + suffix
			stored.Comments[1].Body = raw
			stored.Comments[1].EditedAt = &edited
			shown, err = reader.ShowComplete(t.Context(), stored.Out.UID)
			require.NoError(t, err)
			comment := shown.Comments[1]
			assert.Equal(t, raw, comment.Body)
			assert.Equal(t, evidence, comment.Evidence)
			assert.Equal(t, wantReason, comment.Reason)
			assert.Equal(t, state, comment.ReasonState)
			assert.Equal(t, kata.EvidenceRunes(evidence) >= 40, comment.EvidenceValid)
			assert.Equal(t, original.Author, comment.Author)
			assert.Equal(t, &edited, comment.EditedAt)
			thread, err := reader.Thread(t.Context(), kata.ThreadReq{Issue: stored.Out.UID, Root: root.UID})
			require.NoError(t, err)
			require.Len(t, thread.Comments, 2)
			assert.Equal(t, comment.CommentBody, thread.Comments[1].CommentBody)
		}
	}
	stored.Comments[1] = original
	// Publish a genuine unmarked confirmation too, so both kinds traverse the
	// actual delivery framing before native edits shorten their evidence.
	confirmReq := req
	confirmReq.OperationID = "confirmation-operation"
	confirmReq.Responses = append([]Response{}, req.Responses...)
	confirmReq.Responses[0].OperationID = confirmReq.OperationID
	confirmReq.Responses[0].Fingerprint = "confirmation-intent"
	confirmReq.Responses[0].Kind = "confirm"
	confirmReq.Responses[0].Body = strings.Repeat("界", 40)
	confirmed, err := Deliver(t.Context(), client, confirmReq, State{}, func(State) error { return nil })
	require.NoError(t, err)
	require.True(t, confirmed.Done)
	require.Len(t, stored.Comments, 3)
	for _, record := range []struct {
		index  int
		marker string
	}{{1, marker(req, req.OperationID)}, {2, marker(confirmReq, confirmReq.OperationID)}} {
		original := stored.Comments[record.index]
		for _, separator := range []string{" ", "\r\n\r\n"} {
			for _, evidence := range []string{"  short", strings.Repeat("界", 39), strings.Repeat("界", 40)} {
				// Native edits normalize frame whitespace while retaining its true token.
				raw := evidence + separator + record.marker + "\n"
				stored.Comments[record.index].Body = raw
				stored.Comments[record.index].EditedAt = &edited
				shown, err = reader.ShowComplete(t.Context(), stored.Out.UID)
				require.NoError(t, err)
				assert.Equal(t, raw, shown.Comments[record.index].Body)
				assert.Equal(t, evidence+separator+"\n", shown.Comments[record.index].Evidence)
				assert.Equal(t, kata.EvidenceRunes(evidence) >= 40, shown.Comments[record.index].EvidenceValid)
				assert.Equal(t, "absent", shown.Comments[record.index].ReasonState)
			}
		}
		stored.Comments[record.index] = original
	}

	// Keep the real frame when appending human prose after an existing reply.
	for _, record := range []struct {
		index  int
		marker string
	}{{1, marker(req, req.OperationID)}, {2, marker(confirmReq, confirmReq.OperationID)}} {
		original := stored.Comments[record.index]
		reasons := []string{""}
		if record.index == 1 {
			reasons = append(reasons, "false-positive", "spurious", "risk-accepted")
		}
		for _, reason := range reasons {
			prefix := ""
			if reason != "" {
				prefix = "<!-- roborev-refute:v1 reason=" + reason + " -->\n"
			}
			for _, after := range []string{"more", strings.Repeat("界", 33), strings.Repeat("界", 34)} {
				raw := prefix + "short\n\n" + record.marker + "\n" + after
				stored.Comments[record.index].Body = raw
				stored.Comments[record.index].EditedAt = &edited
				shown, err = reader.ShowComplete(t.Context(), stored.Out.UID)
				require.NoError(t, err)
				comment := shown.Comments[record.index]
				assert.Equal(t, raw, comment.Body)
				assert.Equal(t, "short\n"+after, comment.Evidence)
				assert.Equal(t, kata.EvidenceRunes("short\n"+after) >= 40, comment.EvidenceValid)
				assert.Equal(t, original.Author, comment.Author)
				assert.Equal(t, &edited, comment.EditedAt)
				thread, err := reader.Thread(t.Context(), kata.ThreadReq{Issue: stored.Out.UID, Root: root.UID})
				require.NoError(t, err)
				assert.Equal(t, comment.CommentBody, thread.Comments[record.index].CommentBody)
			}
		}
		stored.Comments[record.index] = original
	}
	// A repeated refutation gets the provider's existing comment and remains
	// unresolved. It never becomes an automatic reply to that comment.
	req.OperationID = uuid.New().String()
	req.Responses[0].OperationID = req.OperationID
	req.Responses[0].Fingerprint = "second-intent" //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
	duplicate, err := Deliver(t.Context(), client, req, State{}, func(State) error { return nil })
	require.ErrorIs(t, err, kata.ErrDuplicateReply)
	assert.False(t, duplicate.Done)
	assert.Equal(t, firstReceipt, duplicate.Receipts["duplicate:"+req.OperationID].UID)
}
