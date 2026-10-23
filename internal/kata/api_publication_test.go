package kata

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata/katafake"
)

func TestIssueMetadataJSONCompatibility(t *testing.T) {
	t.Parallel()
	const legacy = `{"short_id":"task1","qualified_id":"reviews#task1","title":"Task","body":"Requirement","status":"open","labels":[]}`
	for _, tc := range []struct {
		name         string
		metadata     map[string]any
		wantMetadata map[string]any
		want         string
	}{
		{name: "absent", want: legacy},
		{name: "empty", metadata: map[string]any{}, want: legacy},
		{name: "explicit", metadata: map[string]any{"publication": "retained"}, wantMetadata: map[string]any{"publication": "retained"}, want: strings.TrimSuffix(legacy, "}") + `,"metadata":{"publication":"retained"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			issue := Issue{ShortID: "task1", QualifiedID: "reviews#task1", Title: "Task", Body: "Requirement", Status: "open", Labels: []string{}, Metadata: tc.metadata}
			raw, err := json.Marshal(issue)
			require.NoError(t, err)
			assert.JSONEq(tc.want, string(raw))
			var restored Issue
			require.NoError(t, json.Unmarshal(raw, &restored))
			assert.Equal(tc.wantMetadata, restored.Metadata)
		})
	}
}

func TestAPIClientResolveExplicitProject(t *testing.T) {
	f := katafake.New(t)
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	b, err := c.ResolveProject(t.Context(), "other")
	require.NoError(t, err)
	assert.Equal(t, Binding{Project: "other", ProjectUID: f.Projects[1].UID}, b)
	_, err = c.ResolveProject(t.Context(), "missing")
	require.ErrorIs(t, err, ErrProjectNotFound)
}

func TestAPIClientEndpointIdentity(t *testing.T) {
	c, err := NewAPIClient(t.TempDir(), config.KataIntegrationConfig{Enabled: true, Endpoint: "https://KATA.example.com:443/proxy/", APIKey: testAPIKey})
	require.NoError(t, err)
	assert.Equal(t, "https://kata.example.com/proxy", c.Endpoint())
}

func TestAPIClientPublicationMetadataAndCompleteComments(t *testing.T) {
	f := katafake.New(t)
	issue := f.AddIssue(7, "abc4", "Review", "body")
	issue.Out.Metadata = map[string]any{"roborev.action": "action"}
	for i := range 73 {
		issue.Comments = append(issue.Comments, gen.CommentOut{UID: fmt.Sprintf("C%025d", i), Body: fmt.Sprintf("comment %d", i), IssueUID: new(issue.Out.UID)})
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	listed, err := c.ListProject(t.Context(), "reviews", ListOpts{Status: "all", Unlimited: true})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, issue.Out.Metadata, listed[0].Metadata)
	assert.Equal(t, issue.Out.UID, listed[0].UID)
	shown, err := c.ShowComplete(t.Context(), "abc4")
	require.NoError(t, err)
	assert.Equal(t, issue.Out.Metadata, shown.Metadata)
	require.Len(t, shown.Comments, 73)
	assert.Equal(t, "comment 72", shown.Comments[72].Body)
	assert.Equal(t, issue.Out.UID, shown.Comments[72].IssueUID)
}

func TestAPIClientShowCompleteRejectsIncompleteComments(t *testing.T) {
	for name, fields := range map[string]string{"missing": "", "null": `,"comments":null`, "truncated": `,"comments":[],"comments_truncated":true`, "wrong type": `,"comments":{}`} {
		t.Run(name, func(t *testing.T) {
			f := katafake.New(t)
			f.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/issues/abc4") {
					return false
				}
				_, err := fmt.Fprintf(w, `{"issue":{"short_id":"abc4","uid":"I1"},"labels":[],"links":[]%s}`, fields)
				assert.NoError(t, err)
				return true
			}
			_, err := apiClientFor(t, f, bindWorkspace(t, "reviews")).ShowComplete(t.Context(), "abc4")
			require.ErrorIs(t, err, ErrInvalidResponse)
		})
	}
}

func TestAPIClientGuardedMetadata(t *testing.T) {
	for _, previous := range []any{nil, map[string]any{"version": 1, "body": "old"}} {
		t.Run(fmt.Sprint(previous), func(t *testing.T) {
			f := katafake.New(t)
			f.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/metadata") {
					return false
				}
				var body map[string]any
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
					return true
				}
				assert.Equal(t, "roborev", body["actor"])
				assert.Equal(t, map[string]any{"roborev.findings": map[string]any{"version": float64(1)}}, body["patch"])
				guard := body["guard"].(map[string]any)
				if previous == nil {
					assert.Equal(t, map[string]any{"key": "roborev.findings", "if_absent": true}, guard)
				} else {
					assert.Equal(t, map[string]any{"key": "roborev.findings", "if_value": `{"body":"old","version":1}`}, guard)
				}
				f.Reply(w, gen.PatchIssueMetadataResponseBody{Issue: gen.Issue{ShortID: "abc4", UID: "I1", Metadata: map[string]any{"roborev.findings": map[string]any{"version": 1}}}})
				return true
			}
			got, err := apiClientFor(t, f, bindWorkspace(t, "reviews")).MutateMetadata(t.Context(), "abc4", "roborev.findings", previous, map[string]any{"version": 1})
			require.NoError(t, err)
			assert.Equal(t, "I1", got.UID)
			assert.Equal(t, map[string]any{"roborev.findings": map[string]any{"version": float64(1)}}, got.Metadata)
		})
	}
}

func TestAPIClientGuardedMetadataPreservesTypedErrors(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{{"claim_denied", 409}, {"federated_read_only", 409}, {"metadata_guard_failed", 409}} {
		t.Run(tc.code, func(t *testing.T) {
			f := katafake.New(t)
			f.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/metadata") {
					return false
				}
				f.Fail(w, tc.status, tc.code, "write refused")
				return true
			}
			_, err := apiClientFor(t, f, bindWorkspace(t, "reviews")).MutateMetadata(t.Context(), "abc4", "roborev.findings", nil, map[string]any{})
			require.Error(t, err)
			apiErr, ok := AsAPIError(err)
			require.True(t, ok)
			assert.Equal(t, tc.status, apiErr.Status)
			assert.Equal(t, tc.code, apiErr.Code)
		})
	}
}

func TestAPIRequestSizeBoundary(t *testing.T) {
	// JSON quotes contribute two bytes. Huma rejects a body at exactly 1 MiB.
	require.NoError(t, ValidateRequestSize(strings.Repeat("x", (1<<20)-3)))
	require.ErrorIs(t, ValidateRequestSize(strings.Repeat("x", (1<<20)-2)), ErrRequestRejected)
	// A guard contains JSON text, so its quote/backslash escapes count too.
	f := katafake.New(t)
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	_, err := c.Create(t.Context(), CreateReq{Title: "t", Body: strings.Repeat("x", 1<<20)})
	require.ErrorIs(t, err, ErrRequestRejected)
	_, err = c.MutateMetadata(t.Context(), "abc4", "roborev.findings", strings.Repeat(`"`, 1<<19), "x")
	require.ErrorIs(t, err, ErrRequestRejected)
	for _, request := range f.AllRequests() {
		assert.NotEqual(t, http.MethodPost, request.Method)
	}
}

func TestValidateCreateRequestIncludesJSONOverhead(t *testing.T) {
	req := CreateReq{Title: "t", Body: strings.Repeat("x", (1<<20)-100)}
	require.NoError(t, ValidateCreateRequest(req))
	req.Metadata = map[string]any{"publication": strings.Repeat("x", 100)}
	require.ErrorIs(t, ValidateCreateRequest(req), ErrRequestRejected)
	req = CreateReq{Title: "t", Metadata: map[string]any{"bad": make(chan int)}}
	err := ValidateCreateRequest(req)
	require.ErrorIs(t, err, ErrRequestRejected)
	assert.NotContains(t, err.Error(), "bad")
}

func TestAPIClientCommentReceiptPreservesBodyAndEdit(t *testing.T) {
	f := katafake.New(t)
	edited := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/comments") {
			return false
		}
		f.Reply(w, gen.CommentResponseBody{Comment: gen.Comment{UID: "C0000000000000000000000001", Body: "actual stored body", EditedAt: &edited, Author: "roborev", Teammate: new("worker")}})
		return true
	}
	got, err := apiClientFor(t, f, bindWorkspace(t, "reviews")).Comment(t.Context(), CommentReq{Issue: "abc4", Body: "requested body", Teammate: "worker", IdempotencyKey: "operation"})
	require.NoError(t, err)
	assert.Equal(t, "actual stored body", got.Body)
	assert.Equal(t, &edited, got.EditedAt)
}

func TestAPIClientResolveProjectRefreshesStableIdentity(t *testing.T) {
	f := katafake.New(t)
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	first, err := c.ResolveProject(t.Context(), "reviews")
	require.NoError(t, err)
	require.Equal(t, "P0000000000000000000000007", first.ProjectUID)
	f.Projects[0].UID = "P0000000000000000000000009"
	second, err := c.ResolveProject(t.Context(), "reviews")
	require.NoError(t, err)
	assert.Equal(t, "P0000000000000000000000009", second.ProjectUID)
}

func TestAPIClientIssueAttributionOnReadAndList(t *testing.T) {
	f := katafake.New(t)
	issue := f.AddIssue(7, "abc4", "Review", "")
	issue.Out.Author = "credential-owner"
	issue.Out.Teammate = new("worker")
	issue.Out.SourceActor = new("source")
	issue.Out.AccountableActor = new("accountable")
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/issues/abc4") {
			return false
		}
		f.Reply(w, gen.ShowIssueResponseBody{Issue: gen.ShowIssueOut{ShortID: "abc4", UID: issue.Out.UID, Author: "credential-owner", Teammate: new("worker"), SourceActor: new("source"), AccountableActor: new("accountable")}, Labels: []gen.IssueLabel{}, Links: []gen.LinkOut{}, Comments: []gen.CommentOut{}})
		return true
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	listed, err := c.List(t.Context(), ListOpts{})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	want := Attribution{Author: "credential-owner", Teammate: "worker", SourceActor: "source", AccountableActor: "accountable"}
	assert.Equal(t, want, listed[0].Attribution)
	shown, err := c.Show(t.Context(), "abc4")
	require.NoError(t, err)
	assert.Equal(t, want, shown.Attribution)
}

func TestValidateMetadataMutationExactGuardBoundary(t *testing.T) {
	// The old value is JSON text inside a JSON string; its own quote characters
	// add four bytes. This literal pins the complete empty-value request shape.
	const emptyRequest = `{"actor":"roborev","guard":{"if_value":"\"\"","key":"k"},"patch":{"k":"v"}}`
	n := (1 << 20) - len(emptyRequest) - 1
	require.NoError(t, ValidateMetadataMutation("k", strings.Repeat("x", n), "v"))
	require.ErrorIs(t, ValidateMetadataMutation("k", strings.Repeat("x", n+1), "v"), ErrRequestRejected)
}

func TestAPIClientListUnlimitedRejectsExplicitlyIncompleteDiscovery(t *testing.T) {
	for name, tc := range map[string]struct{ schema, body string }{
		"legacy incomplete":  {"0.26.0", `{"issues":[],"complete":false}`},
		"legacy truncated":   {"0.26.0", `{"issues":[],"truncated":true}`},
		"legacy cursor":      {"0.26.0", `{"issues":[],"next_cursor":"next"}`},
		"terminal truncated": {"0.28.0", `{"issues":[],"complete":true,"truncated":true}`},
	} {
		t.Run(name, func(t *testing.T) {
			f := katafake.New(t)
			f.APISchema = tc.schema
			f.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/issues") {
					return false
				}
				_, err := w.Write([]byte(tc.body))
				assert.NoError(t, err)
				return true
			}
			_, err := apiClientFor(t, f, bindWorkspace(t, "reviews")).ListProject(t.Context(), "reviews", ListOpts{Unlimited: true, Status: "all"})
			require.ErrorIs(t, err, ErrInvalidResponse)
		})
	}
}

func TestValidateCreateRequestExactSerializedBoundary(t *testing.T) {
	const emptyRequest = `{"actor":"roborev","body":"","title":"t"}`
	n := (1 << 20) - len(emptyRequest) - 1
	require.NoError(t, ValidateCreateRequest(CreateReq{Title: "t", Body: strings.Repeat("x", n)}))
	require.ErrorIs(t, ValidateCreateRequest(CreateReq{Title: "t", Body: strings.Repeat("x", n+1)}), ErrRequestRejected)
}

// FuzzMetadataGuardSerializedSize uses an independent byte-count oracle for a
// repeated rune. It catches missing guard bytes and the second layer of JSON
// escaping, including control characters, quotes, backslashes, and Unicode.
func FuzzMetadataGuardSerializedSize(f *testing.F) {
	f.Add(uint32(0), byte('x'))
	f.Add(uint32((1<<20)-71), byte('x'))
	f.Add(uint32(300000), byte('"'))
	f.Add(uint32(150000), byte(0))
	f.Add(uint32(600000), byte(255))
	f.Fuzz(func(t *testing.T, seed uint32, symbol byte) {
		// Bound only materialization to the server's request size: this still
		// reaches rejection for every escaping multiplier and the exact boundary.
		n := int(seed % ((1 << 20) + 1))
		factor := 1
		switch symbol {
		case '"', '\\':
			factor = 4
		case '\b', '\t', '\n', '\f', '\r':
			factor = 3
		default:
			if symbol < 32 {
				factor = 7
			} else if symbol >= 128 {
				factor = 2
			}
		}
		const emptyRequest = `{"actor":"roborev","guard":{"if_value":"\"\"","key":"k"},"patch":{"k":"v"}}`
		expectedBytes := len(emptyRequest) + n*factor
		err := ValidateMetadataMutation("k", strings.Repeat(string(rune(symbol)), n), "v")
		if expectedBytes < (1 << 20) {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrRequestRejected)
		}
	})
}

func FuzzKataEndpointIdentityNormalization(f *testing.F) {
	for _, raw := range []string{"https://KATA.example.com:443/proxy/", "https://kata.example.com/", "http://[::1]:80/", "unix:///run/example-kata.sock", "unix:///run/other-kata.sock", ""} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		normalized, err := NormalizeEndpoint(raw)
		if err != nil {
			require.ErrorIs(t, err, ErrInsecureEndpoint)
			return
		}
		again, err := NormalizeEndpoint(normalized)
		require.NoError(t, err)
		assert.Equal(t, normalized, again)
	})
}

func TestNormalizeEndpointWithoutCredentials(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{" https://KATA.example.com:443/proxy/ ", "https://kata.example.com/proxy"},
		{"http://[::1]:80/", "http://[::1]"},
		{"unix:///run/example-kata.sock", "unix:///run/example-kata.sock"},
		{"unix:///run/other-kata.sock", "unix:///run/other-kata.sock"},
	} {
		normalized, err := NormalizeEndpoint(tc.raw)
		require.NoError(t, err)
		assert.Equal(t, tc.want, normalized)
	}
	_, err := NormalizeEndpoint("http://remote.example.com")
	require.ErrorIs(t, err, ErrInsecureEndpoint)
}
