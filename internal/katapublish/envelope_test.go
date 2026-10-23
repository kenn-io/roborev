package katapublish

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
)

func fixtureRequest() Request {
	return Request{OperationID: "operation-1", Endpoint: "https://ledger.example", Project: "reviews", ProjectUID: "project-1", Policy: "comments", ReviewUUID: "review-1", Teammate: "reviewer", Create: kata.CreateReq{Title: "Review findings"}, Entries: []Entry{{Selector: "f:one", ReviewUUID: "review-1", DocumentHash: "hash-1", Index: 0, Body: "Finding evidence"}}}
}

func TestEnvelopeRefusesUnknownAndChangedMappings(t *testing.T) {
	req := fixtureRequest()
	env, err := newEnvelope(req, req.ProjectUID)
	require.NoError(t, err)
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	_, err = DecodeEnvelope(raw)
	require.NoError(t, err)
	for _, bad := range []string{strings.Replace(string(raw), `"version":1`, `"version":2`, 1), strings.Replace(string(raw), `"version":1`, `"version":1,"other":true`, 1), strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(raw), `"index":0,`, "", 1), strings.Replace(string(raw), `"attribution_hash":""`, `"attribution_hash":"bad"`, 1)} {
		_, err = DecodeEnvelope([]byte(bad))
		require.Error(t, err, bad)
	}
	env.Entries[0].DocumentHash = "changed"
	require.Error(t, env.validateRequest(req, req.ProjectUID))
}

func TestFingerprintAndMarkersDoNotExposePayload(t *testing.T) {
	req := fixtureRequest()
	first, err := Fingerprint(req)
	require.NoError(t, err)
	req.Entries[0].Body = "confidential-sentinel"
	second, err := Fingerprint(req)
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
	assert.NotContains(t, marker(req, "f:one"), "confidential-sentinel")
	assert.NotContains(t, marker(req, "f:one"), req.ReviewUUID)
}

func FuzzEnvelopeStrictRoundTrip(f *testing.F) {
	f.Add("evidence", 0)
	f.Add("<script>\nsecret", 23)
	f.Add("evidence", -1)
	f.Add("\xff", 0)
	f.Fuzz(func(t *testing.T, body string, index int) {
		req := fixtureRequest()
		req.Entries[0].Body = body
		req.Entries[0].Index = index
		if index < 0 {
			require.ErrorIs(t, validateRequest(req), ErrPending)
			return
		}
		env, err := newEnvelope(req, req.ProjectUID)
		if !utf8.ValidString(body) {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		raw, err := json.Marshal(env)
		require.NoError(t, err)
		decoded, err := DecodeEnvelope(raw)
		require.NoError(t, err)
		require.NoError(t, decoded.validateRequest(req, req.ProjectUID))
	})
}

func TestResponseSchemaRequiresEveryFrozenField(t *testing.T) {
	req := fixtureRequest()
	req.Responses = []Response{{OperationID: "response-1", Fingerprint: "fingerprint", Body: "Evidence", Target: "comment-1", Kind: "reply", Teammate: "reviewer"}}
	env, err := newEnvelope(req, req.ProjectUID)
	require.NoError(t, err)
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	_, err = DecodeEnvelope(raw)
	require.NoError(t, err)
	missing := strings.Replace(string(raw), `"target":"comment-1",`, "", 1)
	_, err = DecodeEnvelope([]byte(missing))
	require.Error(t, err)
}

func FuzzEnvelopeUntrustedInput(f *testing.F) {
	f.Add([]byte(`{"version":2}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"version":1,"entries":[],"responses":null}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		env, err := DecodeEnvelope(data)
		if err != nil {
			return
		}
		require.Equal(t, 1, env.Version)
		require.NotEmpty(t, env.WriterEndpoint)
		require.NotNil(t, env.Entries)
		require.NotNil(t, env.Responses)
		encoded, err := json.Marshal(env)
		require.NoError(t, err)
		same, err := DecodeEnvelope(encoded)
		require.NoError(t, err)
		assert.Equal(t, env, same)
	})
}

func TestEnvelopeRefusesDuplicateEntries(t *testing.T) {
	req := fixtureRequest()
	env, err := newEnvelope(req, req.ProjectUID)
	require.NoError(t, err)
	env.Entries = append(env.Entries, env.Entries[0])
	data, err := json.Marshal(env)
	require.NoError(t, err)
	_, err = DecodeEnvelope(data)
	require.ErrorIs(t, err, ErrPending)
}
