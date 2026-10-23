package katapublish

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"net/http"
	"strings"
	"testing"
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
