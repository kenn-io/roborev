package kata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"uuid"

	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// PublicationMaxRequestBytes is the pinned Kata v0.19.0 Huma v2.37.3
// default request-body limit. Huma rejects a body of this size or larger.
const PublicationMaxRequestBytes = 1 << 20
const PublicationAPISchemaVersion = "0.28.0"

const (
	publicationReviewKey  = "roborev.review_uuid"
	publicationHashKey    = "roborev.document_hash"
	publicationPolicyKey  = "roborev.policy"
	publicationVersionKey = "roborev.version"
	publicationTimeKey    = "roborev.completed_at"
	publicationCountKey   = "roborev.finding_count"
)

// PublicationProject is a freshly resolved project identity. Resolve it again
// before each write and compare its UID with the saved authorization.
type PublicationProject struct {
	ID   int64
	Name string
	UID  string
}

// Publication is the deterministic, preflighted set of requests for one review.
// Its request bodies are private so they cannot change after preflight.
type Publication struct {
	ReviewUUID   uuid.UUID
	DocumentHash string
	Policy       string
	CompletedAt  time.Time
	Fingerprint  string
	issue        gen.CreateIssueRequestBody
	roots        []gen.CommentRequestBody
	digests      []string
	reviewID     uuid.UUID
	fingerprint  string
}

// PublicationRoot describes one expected root's observed state. Correlation
// markers are untrusted and do not prove authorship.
type PublicationRoot struct {
	Position int
	State    string // complete, missing, edited, ambiguous, unavailable, truncated
	Comment  *gen.CommentOut
}

// PublicationIssue contains the full unfiltered issue response. Truncated is
// true when Kata omitted comments or explicitly reported a truncated result.
type PublicationIssue struct {
	Issue     gen.ShowIssueOut
	Comments  []gen.CommentOut
	Truncated bool
}

func normalizedPublicationEndpoint(endpoint config.KataEndpoint) string {
	if endpoint.Kind == config.KataEndpointUnix {
		return (&url.URL{Scheme: "unix", Path: endpoint.SocketPath}).String()
	}
	u := *endpoint.URL
	u.Scheme = strings.ToLower(u.Scheme)
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
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

// NormalizePublicationEndpoint obtains the destination identity offline.
func NormalizePublicationEndpoint(raw string) (string, error) {
	endpoint, err := config.ParseKataEndpoint(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	return normalizedPublicationEndpoint(endpoint), nil
}

// PublicationEndpoint includes the socket or URL base path in the identity.
func (c *APIClient) PublicationEndpoint() string { return c.endpoint }

// ResolvePublicationProject preserves the client's workspace alias selection,
// and requires the publication-specific API schema and a stable project UID.
func (c *APIClient) ResolvePublicationProject(ctx context.Context, name string) (PublicationProject, error) {
	id, selected, info, err := c.selectProject(ctx, name)
	if err != nil {
		return PublicationProject{}, err
	}
	if !schemaAtLeast(info.APISchemaVersion, PublicationAPISchemaVersion) {
		return PublicationProject{}, fmt.Errorf("%w: publication needs API %s", ErrIncompatible, PublicationAPISchemaVersion)
	}
	var projects gen.ListProjectsResponseBody
	if _, err := c.tr.do(ctx, http.MethodGet, "/api/v1/projects", nil, nil, nil, &projects, http.StatusOK); err != nil {
		return PublicationProject{}, err
	}
	for _, p := range projects.Projects {
		if p.ID == id && p.Name == selected && p.Active && p.DeletedAt == nil && p.UID != "" {
			return PublicationProject{ID: id, Name: p.Name, UID: p.UID}, nil
		}
	}
	return PublicationProject{}, fmt.Errorf("%w: project identity changed or has no UID", ErrProjectNotFound)
}

func publicationPath(project PublicationProject) (string, error) {
	if project.ID <= 0 || project.Name == "" || project.UID == "" {
		return "", fmt.Errorf("%w: invalid publication project", ErrRequestRejected)
	}
	return "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues", nil
}

// SearchPublicationIssues reads all matching issues, including closed ones.
// The caller must reject multiple matches and compare the complete envelope.
func (c *APIClient) SearchPublicationIssues(ctx context.Context, project PublicationProject, id uuid.UUID) ([]gen.IssueOut, error) {
	path, err := publicationPath(project)
	if err != nil {
		return nil, err
	}
	if id == uuid.Nil() {
		return nil, fmt.Errorf("%w: missing review UUID", ErrRequestRejected)
	}
	return c.listAllIssuePages(ctx, path, url.Values{"meta": {publicationReviewKey + "=" + id.String()}}, true)
}

// ShowPublicationIssue reads the unfiltered issue and every comment. Truncated
// or absent comment arrays remain visible but never establish completeness.
func (c *APIClient) ShowPublicationIssue(ctx context.Context, project PublicationProject, ref string) (PublicationIssue, error) {
	path, err := publicationPath(project)
	if err != nil {
		return PublicationIssue{}, err
	}
	if err := validatePathSegment(ref); err != nil {
		return PublicationIssue{}, err
	}
	var response struct {
		Issue             gen.ShowIssueOut `json:"issue"`
		Comments          jsontext.Value   `json:"comments"`
		CommentsTruncated *bool            `json:"comments_truncated"`
	}
	if _, err := c.tr.do(ctx, http.MethodGet, path+"/"+ref, nil, nil, nil, &response, http.StatusOK); err != nil {
		return PublicationIssue{}, err
	}
	if response.Issue.UID == "" || (!strings.EqualFold(response.Issue.UID, ref) && !strings.EqualFold(response.Issue.ShortID, ref)) || response.Issue.ProjectID != project.ID {
		return PublicationIssue{}, fmt.Errorf("%w: issue identity mismatch", ErrInvalidResponse)
	}
	out := PublicationIssue{Issue: response.Issue, Truncated: response.Comments.Kind() != jsontext.KindBeginArray || (response.CommentsTruncated != nil && *response.CommentsTruncated)}
	if response.Comments.Kind() == jsontext.KindBeginArray {
		if err := json.Unmarshal(response.Comments, &out.Comments); err != nil {
			return PublicationIssue{}, fmt.Errorf("%w: invalid comments", ErrInvalidResponse)
		}
	}
	return out, nil
}

func publicationDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func publicationMarker(id uuid.UUID, position int, digest string) string {
	return fmt.Sprintf("<!-- roborev:v1 review=%s position=%d digest=%s -->", id, position, digest)
}

// PreparePublication renders every request and preflights the exact encoded
// bodies before any issue or comment can be posted. Summary mode includes no
// review text, paths, source labels, diagnostics, or fixes.
func PreparePublication(id uuid.UUID, hash string, completed time.Time, doc structuredreview.Document, policy, teammate string) (*Publication, error) {
	if id == uuid.Nil() || len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" || completed.IsZero() || doc.Legacy != nil || (policy != "comments" && policy != "summary") {
		return nil, fmt.Errorf("%w: invalid publication identity or policy", ErrRequestRejected)
	}
	if err := config.ValidateKataTeammate(teammate); err != nil {
		return nil, fmt.Errorf("%w: invalid teammate", ErrRequestRejected)
	}
	p := &Publication{ReviewUUID: id, reviewID: id, DocumentHash: hash, Policy: policy, CompletedAt: completed.UTC()}
	body := "A roborev review reported findings. Each finding has a separate comment."
	p.issue = gen.CreateIssueRequestBody{Actor: new(WriteActor), Title: "Roborev review findings", Body: &body, Labels: []string{RoborevLabel}, ForceNew: new(true), Metadata: map[string]any{
		publicationVersionKey: 1, publicationReviewKey: id.String(), publicationHashKey: hash,
		publicationPolicyKey: policy, publicationTimeKey: p.CompletedAt.Format(time.RFC3339Nano), publicationCountKey: len(doc.Findings),
	}}
	for i, finding := range doc.Findings {
		text := fmt.Sprintf("Finding %d from a roborev review.", i+1)
		if policy == "comments" {
			one := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "Review finding.", Findings: []structuredreview.Finding{finding}, SourceLabels: doc.SourceLabels}
			text = one.Markdown("")
		}
		digest := publicationDigest([]byte(text))
		p.digests = append(p.digests, digest)
		p.roots = append(p.roots, gen.CommentRequestBody{Actor: new(WriteActor), Teammate: &teammate, Body: text + "\n\n" + publicationMarker(id, i, digest)})
	}
	h := sha256.New()
	requests := []any{p.issue}
	for _, root := range p.roots {
		requests = append(requests, root)
	}
	for _, request := range requests {
		encoded, err := encodeRequest(request)
		if err != nil {
			return nil, fmt.Errorf("%w: encode publication request", ErrRequestRejected)
		}
		if len(encoded) >= PublicationMaxRequestBytes {
			return nil, fmt.Errorf("%w: publication request is %d bytes; Kata requires fewer than %d", ErrRequestRejected, len(encoded), PublicationMaxRequestBytes)
		}
		fmt.Fprintf(h, "%d:", len(encoded))
		_, _ = h.Write(encoded)
	}
	p.Fingerprint = hex.EncodeToString(h.Sum(nil))
	p.fingerprint = p.Fingerprint
	return p, nil
}

// IssueKey and RootKey use the review UUID rather than the mutable job UUID.
func (p *Publication) IssueKey() string { return "roborev:review:" + p.ReviewUUID.String() }
func (p *Publication) RootKey(position int) (string, error) {
	if position < 0 || position >= len(p.roots) {
		return "", fmt.Errorf("%w: invalid finding position", ErrRequestRejected)
	}
	return fmt.Sprintf("roborev:review:%s:%d:%s", p.ReviewUUID, position, p.digests[position]), nil
}

func (p *Publication) ready() bool {
	return p != nil && p.fingerprint != "" && p.Fingerprint == p.fingerprint && p.ReviewUUID == p.reviewID &&
		p.issue.Metadata[publicationReviewKey] == p.ReviewUUID.String() &&
		p.issue.Metadata[publicationHashKey] == p.DocumentHash &&
		p.issue.Metadata[publicationPolicyKey] == p.Policy &&
		p.issue.Metadata[publicationTimeKey] == p.CompletedAt.UTC().Format(time.RFC3339Nano)
}

// MatchesIssueEnvelope checks all correlation fields, not author trust.
func (p *Publication) MatchesIssueEnvelope(metadata map[string]any) bool {
	for key, expected := range p.issue.Metadata {
		a, err := encodeRequest(expected)
		if err != nil {
			return false
		}
		b, err := encodeRequest(metadata[key])
		if err != nil || string(a) != string(b) {
			return false
		}
	}
	return true
}

// Roots reconstructs mappings from complete comment bodies and markers.
// Missing markers cannot be attributed to a position and remain missing.
func (p *Publication) Roots(issue PublicationIssue) []PublicationRoot {
	out := make([]PublicationRoot, len(p.roots))
	for i, root := range p.roots {
		out[i] = PublicationRoot{Position: i, State: "missing"}
		prefix := fmt.Sprintf("<!-- roborev:v1 review=%s position=%d ", p.ReviewUUID, i)
		var candidates []gen.CommentOut
		for _, comment := range issue.Comments {
			if comment.ReplyToUID == nil && strings.Contains(comment.Body, prefix) {
				candidates = append(candidates, comment)
			}
		}
		switch {
		case len(candidates) > 1:
			out[i].State = "ambiguous"
		case len(candidates) == 1:
			comment := candidates[0]
			out[i].Comment = &comment
			if comment.Body != root.Body || comment.EditedAt != nil {
				out[i].State = "edited"
			} else if comment.UID == "" || comment.Handle == nil || *comment.Handle == "" {
				out[i].State = "unavailable"
			} else if issue.Truncated {
				out[i].State = "truncated"
			} else {
				out[i].State = "complete"
			}
		case issue.Truncated:
			out[i].State = "truncated"
		}
	}
	return out
}

// CreatePublicationIssue sends exactly the preflighted issue request. The
// caller is responsible for unique metadata search and destination rechecks.
func (c *APIClient) CreatePublicationIssue(ctx context.Context, project PublicationProject, p *Publication) (gen.MutationResponseBody, error) {
	var out gen.MutationResponseBody
	path, err := publicationPath(project)
	if err != nil {
		return out, err
	}
	if !p.ready() {
		return out, errors.New("publication was not preflighted")
	}
	_, err = c.tr.do(ctx, http.MethodPost, path, nil, p.issue, http.Header{"Idempotency-Key": {p.IssueKey()}}, &out, http.StatusOK, http.StatusCreated)
	if err == nil && (out.Issue.UID == "" || out.Issue.ProjectID != project.ID || !p.MatchesIssueEnvelope(out.Issue.Metadata)) {
		return gen.MutationResponseBody{}, fmt.Errorf("%w: created issue identity or envelope mismatch", ErrInvalidResponse)
	}
	return out, err
}

// CreatePublicationRoot sends exactly one preflighted comment request.
func (c *APIClient) CreatePublicationRoot(ctx context.Context, project PublicationProject, issueUID string, p *Publication, position int) (gen.CommentResponseBody, error) {
	var out gen.CommentResponseBody
	path, err := publicationPath(project)
	if err != nil {
		return out, err
	}
	if err := validatePathSegment(issueUID); err != nil {
		return out, err
	}
	if !p.ready() {
		return out, errors.New("publication was not preflighted")
	}
	key, err := p.RootKey(position)
	if err != nil {
		return out, err
	}
	_, err = c.tr.do(ctx, http.MethodPost, path+"/"+issueUID+"/comments", nil, p.roots[position], http.Header{"Idempotency-Key": {key}}, &out, http.StatusOK, http.StatusCreated)
	if err == nil && (out.Comment.UID == "" || out.Comment.Body != p.roots[position].Body || out.Comment.ReplyToUID != nil || !strings.EqualFold(out.Issue.UID, issueUID)) {
		return gen.CommentResponseBody{}, fmt.Errorf("%w: created comment identity or body mismatch", ErrInvalidResponse)
	}
	return out, err
}
