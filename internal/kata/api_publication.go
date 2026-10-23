package kata

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
)

// MaxRequestBytes is Kata's 1 MiB JSON request limit. At Kata v0.19.0,
// internal/daemon/handlers_issues.go:80 and handlers_metadata.go:27 register
// create and metadata writes without MaxBodyBytes. Its pinned Huma v2.37.3
// huma.go:1378-1382 supplies 1024*1024 bytes; readBody at 2114-2128
// rejects a body at that size too. Size the serialized request,
// including JSON escaping and the metadata guard's previous value.
const MaxRequestBytes = 1 << 20

// MaxUIDBytes is the canonical ASCII ULID width from Kata v0.19.0
// internal/uid/uid.go:14-17. Comment receipts are validated at this boundary;
// publication preflight can therefore reserve their complete serialized size.
const MaxUIDBytes = 26

func boundedReceiptUID(value string) bool {
	return value != "" && len(value) <= MaxUIDBytes && strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r > 0x7e || r == '"' || r == '\\' }) < 0
}

// ValidateRequestSize checks the exact deterministic JSON encoding used by the
// API transport. It reports ErrRequestRejected without including payload text.
func ValidateRequestSize(body any) error {
	_, err := marshalRequest(body)
	return err
}

func marshalRequest(body any) ([]byte, error) {
	encoded, err := json.Marshal(body, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("%w: encode JSON request", ErrRequestRejected)
	}
	if len(encoded) >= MaxRequestBytes {
		return nil, fmt.Errorf("%w: JSON request is %d bytes; Kata requires fewer than %d bytes", ErrRequestRejected, len(encoded), MaxRequestBytes)
	}
	return encoded, nil
}

// Endpoint returns this client's normalized destination identity. Unix socket
// identities retain their socket path rather than the transport's dummy host.
func (c *APIClient) Endpoint() string { return c.endpoint }

// NormalizeEndpoint validates and normalizes a destination without reading a
// credential or connecting to Kata. Capture uses the same identity as Endpoint.
func NormalizeEndpoint(raw string) (string, error) {
	endpoint, err := config.ParseKataEndpoint(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	return normalizedEndpoint(endpoint), nil
}

func normalizedEndpoint(endpoint config.KataEndpoint) string {
	if endpoint.Kind == config.KataEndpointUnix {
		return (&url.URL{Scheme: "unix", Path: endpoint.SocketPath}).String()
	}
	u := *endpoint.URL
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "443" && u.Scheme == "https" || port == "80" && u.Scheme == "http" {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

// ResolveProject resolves an explicit hook project, falling back to the
// workspace/default only when name is empty. It includes the stable project UID.
func (c *APIClient) ResolveProject(ctx context.Context, name string) (Binding, error) {
	if name == "" {
		binding, err := c.Binding(ctx)
		if err != nil {
			return Binding{}, err
		}
		name = binding.Project
	}
	if _, err := c.Info(ctx); err != nil {
		return Binding{}, err
	}
	// Resolve a fresh identity before each delivery attempt. Reusing the name
	// cache could silently pin a deleted-and-recreated project to its old UID.
	projects, err := c.refreshProjects(ctx)
	if err != nil {
		return Binding{}, err
	}
	project, ok := projects[name]
	if !ok {
		return Binding{}, fmt.Errorf("%w: %q", ErrProjectNotFound, name)
	}
	if project.UID == "" {
		return Binding{}, fmt.Errorf("%w: project has no UID", ErrInvalidResponse)
	}
	return Binding{Project: project.Name, ProjectUID: project.UID}, nil
}

// ShowComplete reads every comment belonging to the issue. Kata v0.19.0's
// internal/commentref/graph.go:315 limits only thread selections; an
// unfiltered show is complete without a limit or cursor. Missing/null comments
// and explicit truncation cannot establish completeness and are refused.
func (c *APIClient) ShowComplete(ctx context.Context, ref string) (Issue, error) {
	return c.showIssue(ctx, ref, true)
}

// apiShowResponse retains proof that the server supplied a comments array.
// Generated response slices alone cannot distinguish absent/null from empty.
type apiShowResponse struct {
	shown         gen.ShowIssueResponseBody
	commentsArray bool
}

func (r *apiShowResponse) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &r.shown); err != nil {
		return err
	}
	var fields struct {
		Comments jsontext.Value `json:"comments"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	r.commentsArray = fields.Comments.Kind() == jsontext.KindBeginArray
	return nil
}

func metadataRequest(key string, previous, value any) (gen.PatchIssueMetadataRequestBody, error) {
	if strings.TrimSpace(key) == "" {
		return gen.PatchIssueMetadataRequestBody{}, fmt.Errorf("%w: metadata key is required", ErrRequestRejected)
	}
	guard := &gen.MetadataPatchGuard_OneOf{}
	if previous == nil {
		guard.N, guard.B = 2, gen.MetadataPatchGuard_OneOf_1{Key: key, IfAbsent: gen.True}
	} else {
		encoded, err := json.Marshal(previous, json.Deterministic(true))
		if err != nil {
			return gen.PatchIssueMetadataRequestBody{}, fmt.Errorf("%w: encode metadata guard", ErrRequestRejected)
		}
		guard.N, guard.A = 1, gen.MetadataPatchGuard_OneOf_0{Key: key, IfValue: string(encoded)}
	}
	return gen.PatchIssueMetadataRequestBody{Actor: new(WriteActor), Patch: map[string]any{key: value}, Guard: &gen.MetadataPatchGuard{MetadataPatchGuard_OneOf: guard}}, nil
}

// ValidateMetadataMutation checks the entire guarded request before any write.
func ValidateMetadataMutation(key string, previous, value any) error {
	request, err := metadataRequest(key, previous, value)
	if err != nil {
		return err
	}
	return ValidateRequestSize(request)
}

// MutateMetadata compares the current value of key and patches it atomically
// at this client's endpoint. Nil previous means the key must be absent. The
// server's typed errors, including guard conflicts and claim denials, pass
// through unchanged; this method never acquires a claim or chooses a new host.
func (c *APIClient) MutateMetadata(ctx context.Context, ref, key string, previous, value any) (Issue, error) {
	project, id, err := splitRef(ref)
	if err != nil {
		return Issue{}, err
	}
	request, err := metadataRequest(key, previous, value)
	if err != nil {
		return Issue{}, err
	}
	if err := ValidateRequestSize(request); err != nil {
		return Issue{}, err
	}
	path, resolved, err := c.issuesPath(ctx, project)
	if err != nil {
		return Issue{}, err
	}
	var response gen.PatchIssueMetadataResponseBody
	if _, err := c.tr.do(ctx, http.MethodPost, path+"/"+id+"/metadata", nil, request, nil, &response, http.StatusOK); err != nil {
		return Issue{}, err
	}
	issue := response.Issue
	if issue.UID == "" || (!strings.EqualFold(issue.ShortID, id) && issue.UID != id) {
		return Issue{}, fmt.Errorf("%w: metadata response identifies a different issue", ErrInvalidResponse)
	}
	return Issue{ShortID: issue.ShortID, QualifiedID: resolved.Name + "#" + issue.ShortID, UID: issue.UID, ProjectUID: cmp.Or(str(issue.ProjectUID), resolved.UID), Title: issue.Title, Body: issue.Body, Status: issue.Status, Metadata: issue.Metadata, Attribution: attributionOf(issue.Author, issue.Teammate, issue.SourceActor, issue.AccountableActor)}, nil
}

// ValidateCreateRequest checks the entire issue create request, including
// metadata and serialization overhead, without contacting Kata.
func ValidateCreateRequest(req CreateReq) error {
	body, err := createRequest(req)
	if err != nil {
		return err
	}
	return ValidateRequestSize(body)
}

func createRequest(req CreateReq) (gen.CreateIssueRequestBody, error) {
	if strings.TrimSpace(req.Title) == "" {
		return gen.CreateIssueRequestBody{}, fmt.Errorf("%w: title is required", ErrRequestRejected)
	}
	body := gen.CreateIssueRequestBody{Actor: new(WriteActor), Title: req.Title, Body: &req.Body, Labels: req.Labels, Metadata: req.Metadata}
	if req.ForceNew {
		body.ForceNew = new(true)
	}
	if req.Priority != nil {
		body.Priority = new(int64(*req.Priority))
	}
	return body, nil
}
