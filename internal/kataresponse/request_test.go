package kataresponse

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/findingid"
)

func validRequest(t *testing.T) Request {
	t.Helper()
	id, err := findingid.New(uuid.New(), 0, strings.Repeat("a", 64))
	require.NoError(t, err)
	return Request{JobID: 42, OperationID: uuid.New().String(), FindingID: id.String(), Kind: "reply", Body: "Evidence from the selected code."} //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
}

func TestValidateRequest(t *testing.T) {
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		t.Run(kind, func(t *testing.T) {
			r := validRequest(t)
			r.Kind = kind
			r.Body = strings.Repeat("界", 40)
			if kind == "supersede" {
				r.Replacement = validRequest(t).FindingID
			}
			require.NoError(t, r.Validate())
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*Request)
	}{
		{"kind", func(r *Request) { r.Kind = "force" }},
		{"job", func(r *Request) { r.JobID = -1 }},
		{"selector", func(r *Request) { r.FindingID = "f:abc" }},
		{"operation", func(r *Request) { r.OperationID = "" }},
		{"replacement", func(r *Request) { r.Kind = "supersede" }},
		{"unexpected replacement", func(r *Request) { r.Replacement = r.FindingID }},
		{"empty", func(r *Request) { r.Body = " \t\n" }},
		{"short", func(r *Request) { r.Kind = "refute"; r.Body = strings.Repeat("界", 39) }},
	} {
		t.Run(tc.name, func(t *testing.T) { r := validRequest(t); tc.change(&r); require.Error(t, r.Validate()) })
	}
}

func TestFingerprintFreezesPayload(t *testing.T) {
	r := validRequest(t)
	first, err := r.Fingerprint()
	require.NoError(t, err)
	again, err := r.Fingerprint()
	require.NoError(t, err)
	assert.Equal(t, first, again)
	for _, change := range []func(*Request){
		func(r *Request) { r.Body += " changed" }, func(r *Request) { r.Kind = "supersede"; r.Replacement = validRequest(t).FindingID }, func(r *Request) { r.TargetComment = "c:123abc" }, func(r *Request) { r.JobID++ }, func(r *Request) { r.FindingID = validRequest(t).FindingID },
	} {
		next := r
		change(&next)
		got, err := next.Fingerprint()
		require.NoError(t, err)
		assert.NotEqual(t, first, got)
	}
}

func TestIdempotencyKeyCaseIsSignificant(t *testing.T) {
	r := validRequest(t)
	r.OperationID = "Fix-42:Attempt-A"
	first, err := r.Fingerprint()
	require.NoError(t, err)
	r.OperationID = strings.ToUpper(r.OperationID)
	require.NoError(t, r.Validate())
	second, err := r.Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
	assert.Equal(t, "FIX-42:ATTEMPT-A", r.OperationID)
}

func TestResponseTargetCanResolveItsOriginalJobInDaemon(t *testing.T) {
	for _, target := range []string{"c:abcdef", "c:ghjkmn", "reviews#ab12:GHJKMN", "01M00000000000000000000000"} {
		r := validRequest(t)
		r.JobID = 0
		r.FindingID = target
		require.NoError(t, r.Validate())
		assert.Equal(t, target, r.FindingID)
	}
	r := validRequest(t)
	r.JobID = 0
	require.NoError(t, r.Validate(), "a complete finding selector supplies the original job")
	r.JobID = -1
	require.Error(t, r.Validate())
}

func TestRequestReasonValidationAndLegacyFingerprint(t *testing.T) {
	r := validRequest(t)
	r.Kind, r.Body = "refute", strings.Repeat("界", 40)
	// This is the pre-reason DTO's deterministic JSON shape.
	raw := []byte(`{"job_id":42,"operation_id":"` + r.OperationID + `","finding_id":"` + r.FindingID + `","kind":"refute","body":"` + r.Body + `"}`)
	sum := sha256.Sum256(raw)
	original, err := r.Fingerprint()
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(sum[:]), original)
	for _, reason := range []string{"false-positive", "spurious", "risk-accepted"} {
		r.Reason = &reason
		require.NoError(t, r.Validate())
		got, err := r.Fingerprint()
		require.NoError(t, err)
		assert.NotEqual(t, original, got)
		saved, err := json.Marshal(r)
		require.NoError(t, err)
		var restored Request
		require.NoError(t, json.Unmarshal(saved, &restored))
		assert.Equal(t, r, restored)
	}
	for _, reason := range []string{"", "unknown", "unspecified"} {
		r.Reason = &reason
		require.Error(t, r.Validate())
	}
	reason := "spurious"
	r.Reason = &reason
	for _, kind := range []string{"reply", "confirm", "supersede"} {
		r.Kind = kind
		require.Error(t, r.Validate())
	}
	r.Kind, r.Reason, r.Body = "refute", nil, "<!-- roborev-refute:v1 reason=spurious -->\n"+strings.Repeat("界", 40)
	require.Error(t, r.Validate())
}
