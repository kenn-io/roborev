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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	gen "go.kenn.io/kata/pkg/client/generated"
	"golang.org/x/net/http/httpguts"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// PublicationMaxRequestBytes is the pinned Kata v0.19.0 Huma v2.37.3
// default request-body limit. Huma rejects a body of this size or larger.
const (
	PublicationMaxRequestBytes  = 1 << 20
	PublicationAPISchemaVersion = "0.28.0"
)

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

// Publication holds deterministic expected requests for one review. Only a
// preflighted publication can write; its request bodies remain private.
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
	Position    int
	EndPosition *int   // inclusive end for a run of unobserved positions
	State       string // complete, missing, edited, ambiguous, unavailable, truncated
	Comment     *gen.CommentOut
}

// PublicationIssue contains the full unfiltered issue response. Truncated is
// true when Kata omitted comments or explicitly reported a truncated result.
type PublicationIssue struct {
	Issue     gen.ShowIssueOut
	Legacy    Issue
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
	project, info, err := c.selectProjectObject(ctx, name)
	if err != nil {
		return PublicationProject{}, err
	}
	if !schemaAtLeast(info.APISchemaVersion, PublicationAPISchemaVersion) {
		return PublicationProject{}, fmt.Errorf("%w: publication needs API %s", ErrIncompatible, PublicationAPISchemaVersion)
	}
	if project.ID <= 0 || project.Name == "" || !project.Active || project.DeletedAt != nil || project.UID == "" {
		return PublicationProject{}, fmt.Errorf("%w: project identity changed or has no UID", ErrProjectNotFound)
	}
	return PublicationProject{ID: project.ID, Name: project.Name, UID: project.UID}, nil
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
		Labels            []gen.IssueLabel `json:"labels"`
		Links             []gen.LinkOut    `json:"links"`
		CommentsTruncated *bool            `json:"comments_truncated"`
	}
	if _, err := c.tr.do(ctx, http.MethodGet, path+"/"+ref, nil, nil, nil, &response, http.StatusOK); err != nil {
		return PublicationIssue{}, err
	}
	if response.Issue.UID == "" || (!strings.EqualFold(response.Issue.UID, ref) && !strings.EqualFold(response.Issue.ShortID, ref)) || response.Issue.ProjectID != project.ID || (response.Issue.ProjectUID != nil && *response.Issue.ProjectUID != project.UID) {
		return PublicationIssue{}, fmt.Errorf("%w: issue identity mismatch", ErrInvalidResponse)
	}
	legacy, err := mapShownIssue(project.Name, response.Issue, response.Labels, response.Links)
	if err != nil {
		return PublicationIssue{}, err
	}
	out := PublicationIssue{Issue: response.Issue, Legacy: legacy, Truncated: response.Comments.Kind() != jsontext.KindBeginArray || (response.CommentsTruncated != nil && *response.CommentsTruncated)}
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

// PublicationOptions are hook-specific issue settings included in preflight.
type PublicationOptions struct {
	Labels   []string
	Priority *int
}

// PreparePublicationWithOptions renders and preflights every issue and root
// request. Summary mode excludes review text, paths, sources, diagnostics, and
// fixes. The selected hook options are included in the preflight fingerprint.
func PreparePublicationWithOptions(id uuid.UUID, hash string, completed time.Time, doc structuredreview.Document, policy, teammate string, options PublicationOptions) (*Publication, error) {
	p, err := PublicationFromSource(id, hash, completed, doc, policy)
	if err != nil {
		return nil, err
	}
	if err := config.ValidateKataTeammate(teammate); err != nil {
		return nil, fmt.Errorf("%w: invalid teammate", ErrRequestRejected)
	}
	p.issue.Actor = new(WriteActor)
	labels := append([]string{RoborevLabel}, options.Labels...)
	slices.Sort(labels)
	p.issue.Labels = slices.Compact(labels)
	if options.Priority != nil {
		p.issue.Priority = new(int64(*options.Priority))
	}
	for i := range p.roots {
		p.roots[i].Actor = new(WriteActor)
		p.roots[i].Teammate = &teammate
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

// PublicationFromSource constructs expected roots for inspection without
// authorizing or preflighting writes.
func PublicationFromSource(id uuid.UUID, hash string, completed time.Time, doc structuredreview.Document, policy string) (*Publication, error) {
	if id == uuid.Nil() || len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" || completed.IsZero() || doc.Legacy != nil || (policy != "comments" && policy != "summary") {
		return nil, fmt.Errorf("%w: invalid publication identity or policy", ErrRequestRejected)
	}
	p := &Publication{ReviewUUID: id, reviewID: id, DocumentHash: hash, Policy: policy, CompletedAt: completed.UTC()}
	body := "A roborev review reported findings. Each finding has a separate comment."
	p.issue = gen.CreateIssueRequestBody{Title: "Roborev review findings", Body: &body, Labels: []string{RoborevLabel}, ForceNew: new(true), Metadata: map[string]any{
		publicationVersionKey: 1, publicationReviewKey: id.String(), publicationHashKey: hash,
		publicationPolicyKey: policy, publicationTimeKey: p.CompletedAt.Format(time.RFC3339Nano), publicationCountKey: len(doc.Findings),
	}}
	for i, finding := range doc.Findings {
		text := fmt.Sprintf("Finding %d from a roborev review.", i+1)
		if policy == "comments" {
			one := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "Review finding.", Findings: []structuredreview.Finding{finding}, SourceLabels: doc.SourceLabels}
			text = one.MarkdownFrom("", i+1)
		}
		digest := publicationDigest([]byte(text))
		p.digests = append(p.digests, digest)
		p.roots = append(p.roots, gen.CommentRequestBody{Body: text + "\n\n" + publicationMarker(id, i, digest)})
	}
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
	envelope, err := ReadPublicationEnvelope(p.issue.Metadata)
	if err != nil {
		return nil
	}
	sparse := ReadPublicationRoots(envelope, issue)
	roots := make([]PublicationRoot, 0, len(p.roots))
	for _, root := range sparse {
		end := root.Position
		if root.EndPosition != nil {
			end = *root.EndPosition
		}
		for pos := root.Position; pos <= end; pos++ {
			item := root
			item.Position, item.EndPosition = pos, nil
			roots = append(roots, item)
		}
	}
	for i := range roots {
		if roots[i].Comment != nil && roots[i].State == "complete" && roots[i].Comment.Body != p.roots[i].Body {
			roots[i].State = "edited"
		}
	}
	return roots
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
	out, err = c.postComment(ctx, path, issueUID, p.roots[position], key)
	if err == nil && (out.Comment.UID == "" || out.Comment.Body != p.roots[position].Body || out.Comment.ReplyToUID != nil || !strings.EqualFold(out.Issue.UID, issueUID)) {
		return gen.CommentResponseBody{}, fmt.Errorf("%w: created comment identity or body mismatch", ErrInvalidResponse)
	}
	return out, err
}

// PublicationEnvelope is untrusted correlation metadata returned by Kata.
type PublicationEnvelope struct {
	Version      int       `json:"version"`
	ReviewUUID   uuid.UUID `json:"review_uuid"`
	DocumentHash string    `json:"document_hash"`
	Policy       string    `json:"policy"`
	CompletedAt  time.Time `json:"completed_at"`
	FindingCount int       `json:"finding_count"`
}

// ReadPublicationEnvelope validates every field of a v1 correlation envelope.
func ReadPublicationEnvelope(metadata map[string]any) (PublicationEnvelope, error) {
	var out PublicationEnvelope
	raw, err := json.Marshal(metadata)
	if err != nil {
		return out, err
	}
	var wire struct {
		Version   int    `json:"roborev.version"`
		Review    string `json:"roborev.review_uuid"`
		Hash      string `json:"roborev.document_hash"`
		Policy    string `json:"roborev.policy"`
		Completed string `json:"roborev.completed_at"`
		Count     *int   `json:"roborev.finding_count"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return out, fmt.Errorf("invalid publication envelope: %w", err)
	}
	id, err := uuid.Parse(wire.Review) //nolint:forbidigo // Parse the review UUID at the foreign Kata metadata text boundary.
	if err != nil || id == uuid.Nil() || wire.Version != 1 || len(wire.Hash) != 64 || strings.Trim(wire.Hash, "0123456789abcdef") != "" || (wire.Policy != "comments" && wire.Policy != "summary") || wire.Count == nil || *wire.Count < 0 {
		return out, errors.New("invalid publication envelope")
	}
	completed, err := time.Parse(time.RFC3339Nano, wire.Completed)
	if err != nil || completed.IsZero() {
		return out, errors.New("invalid publication completion time")
	}
	return PublicationEnvelope{Version: wire.Version, ReviewUUID: id, DocumentHash: wire.Hash, Policy: wire.Policy, CompletedAt: completed, FindingCount: *wire.Count}, nil
}

// ReadPublicationRoots verifies markers and current bodies without a local review.
// Correlation is untrusted and does not establish the comment's author.
func ReadPublicationRoots(envelope PublicationEnvelope, issue PublicationIssue) []PublicationRoot {
	// Only observed markers consume space. Ranges cover the rest, regardless
	// of how large the untrusted finding count is.
	candidates := make(map[int][]gen.CommentOut)
	uncorrelated := false
	prefix := fmt.Sprintf("<!-- roborev:v1 review=%s position=", envelope.ReviewUUID)
	for _, comment := range issue.Comments {
		if comment.ReplyToUID != nil {
			continue
		}
		remaining := comment.Body
		seen := make(map[int]bool)
		for {
			_, suffix, found := strings.Cut(remaining, prefix)
			if !found {
				break
			}
			remaining = suffix
			positionText, _, hasSpace := strings.Cut(suffix, " ")
			position, err := strconv.Atoi(positionText)
			if hasSpace && err == nil && position >= 0 && position < envelope.FindingCount && strconv.Itoa(position) == positionText && !seen[position] {
				candidates[position] = append(candidates[position], comment)
				seen[position] = true
			}
		}
		if len(seen) == 0 {
			uncorrelated = true
		}
	}
	positions := make([]int, 0, len(candidates))
	for position := range candidates {
		positions = append(positions, position)
	}
	slices.Sort(positions)
	missingState := "missing"
	if issue.Truncated {
		missingState = "truncated"
	} else if uncorrelated {
		missingState = "ambiguous"
	}
	var roots []PublicationRoot
	addMissing := func(start, end int) {
		if start > end {
			return
		}
		root := PublicationRoot{Position: start, State: missingState}
		if end > start {
			root.EndPosition = new(end)
		}
		roots = append(roots, root)
	}
	next := 0
	for _, position := range positions {
		addMissing(next, position-1)
		root := PublicationRoot{Position: position}
		comments := candidates[position]
		if len(comments) > 1 {
			root.State = "ambiguous"
		} else {
			comment := comments[0]
			root.Comment = &comment
			markerPrefix := fmt.Sprintf("%s%d ", prefix, position)
			markerAt := strings.LastIndex(comment.Body, "\n\n"+markerPrefix)
			valid := false
			if markerAt >= 0 {
				text := comment.Body[:markerAt]
				valid = comment.Body[markerAt+2:] == publicationMarker(envelope.ReviewUUID, position, publicationDigest([]byte(text)))
			}
			switch {
			case !valid || comment.EditedAt != nil:
				root.State = "edited"
			case comment.UID == "" || comment.Handle == nil || *comment.Handle == "":
				root.State = "unavailable"
			case issue.Truncated:
				root.State = "truncated"
			default:
				root.State = "complete"
			}
		}
		roots = append(roots, root)
		next = position + 1
	}
	addMissing(next, envelope.FindingCount-1)
	return roots
}

// ResolvePublicationTarget selects a comment in a verified finding thread in
// the complete issue. Correlation markers do not establish author trust.
func ResolvePublicationTarget(project PublicationProject, issue PublicationIssue, target string) (string, error) {
	if issue.Truncated || issue.Issue.UID == "" || issue.Issue.ID <= 0 || project.ID <= 0 || project.UID == "" || project.Name == "" || issue.Issue.ProjectID != project.ID {
		return "", fmt.Errorf("%w: incomplete issue identity or comments", ErrRequestRejected)
	}
	envelope, err := ReadPublicationEnvelope(issue.Issue.Metadata)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRequestRejected, err)
	}
	roots := ReadPublicationRoots(envelope, issue)
	byUID := make(map[string]gen.CommentOut, len(issue.Comments))
	var matches []gen.CommentOut
	for _, comment := range issue.Comments {
		if comment.UID == "" {
			return "", fmt.Errorf("%w: comment missing UID", ErrInvalidResponse)
		}
		key := strings.ToLower(comment.UID)
		if _, exists := byUID[key]; exists {
			return "", fmt.Errorf("%w: duplicate comment UID", ErrInvalidResponse)
		}
		byUID[key] = comment
		if strings.EqualFold(comment.UID, target) || (len(target) >= 8 && strings.EqualFold(target[:2], "c:") && strings.HasSuffix(key, strings.ToLower(target[2:]))) {
			matches = append(matches, comment)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%w: target comment not found", ErrNotFound)
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("%w: ambiguous target comment", ErrConflict)
	}
	selected := matches[0].UID
	seen := make(map[string]bool)
	current := matches[0]
	for {
		key := strings.ToLower(current.UID)
		if seen[key] {
			return "", fmt.Errorf("%w: cyclic comment ancestry", ErrRequestRejected)
		}
		seen[key] = true
		if current.IssueID != issue.Issue.ID || (current.IssueUID != nil && !strings.EqualFold(*current.IssueUID, issue.Issue.UID)) || (current.ProjectID != nil && *current.ProjectID != project.ID) || (current.ProjectUID != nil && *current.ProjectUID != project.UID) || (current.ProjectName != nil && *current.ProjectName != project.Name) {
			return "", fmt.Errorf("%w: foreign comment", ErrRequestRejected)
		}
		if current.ReplyToUID == nil {
			for _, root := range roots {
				if root.State == "complete" && root.Comment != nil && strings.EqualFold(root.Comment.UID, current.UID) {
					return selected, nil
				}
			}
			return "", fmt.Errorf("%w: target is not in a complete finding thread", ErrRequestRejected)
		}
		parent, ok := byUID[strings.ToLower(*current.ReplyToUID)]
		if !ok {
			return "", fmt.Errorf("%w: missing comment ancestor", ErrRequestRejected)
		}
		current = parent
	}
}

// ValidateReply checks the producer's evidence and idempotency constraints.
func ValidateReply(kind, body, key string) error {
	switch gen.CommentRequestBodyKind(kind) {
	case gen.Reply, gen.Confirm, gen.Refute, gen.Supersede:
	default:
		return fmt.Errorf("%w: invalid reply kind", ErrRequestRejected)
	}
	normalized := strings.Join(strings.Fields(body), " ")
	if normalized == "" || ((kind == "confirm" || kind == "refute") && utf8.RuneCountInString(normalized) < 40) {
		return fmt.Errorf("%w: insufficient reply evidence", ErrRequestRejected)
	}
	if key == "" || !httpguts.ValidHeaderFieldValue(key) || strings.TrimSpace(key) != key {
		return fmt.Errorf("%w: invalid idempotency key", ErrRequestRejected)
	}
	return nil
}

func (c *APIClient) postComment(ctx context.Context, path, issueUID string, request gen.CommentRequestBody, key string) (gen.CommentResponseBody, error) {
	var out gen.CommentResponseBody
	encoded, err := encodeRequest(request)
	if err != nil || len(encoded) >= PublicationMaxRequestBytes {
		return out, fmt.Errorf("%w: comment request exceeds Kata's request limit", ErrRequestRejected)
	}
	_, err = c.tr.do(ctx, http.MethodPost, path+"/"+issueUID+"/comments", nil, request, http.Header{"Idempotency-Key": {key}}, &out, http.StatusOK, http.StatusCreated)
	return out, err
}

// Comment submits a typed response. The caller resolves and validates its target
// against a complete issue immediately before calling it.
func (c *APIClient) Comment(ctx context.Context, project PublicationProject, issueUID, target, kind, body, key, teammate string) (gen.CommentResponseBody, error) {
	var out gen.CommentResponseBody
	path, err := publicationPath(project)
	if err != nil {
		return out, err
	}
	if err = validatePathSegment(issueUID); err != nil {
		return out, err
	}
	if err = validatePathSegment(target); err != nil {
		return out, err
	}
	if err = ValidateReply(kind, body, key); err != nil {
		return out, err
	}
	if err = config.ValidateKataTeammate(teammate); err != nil {
		return out, fmt.Errorf("%w: invalid teammate", ErrRequestRejected)
	}
	k := gen.CommentRequestBodyKind(kind)
	request := gen.CommentRequestBody{Actor: new(WriteActor), Teammate: &teammate, Body: body, ReplyTo: &target, Kind: &k}
	out, err = c.postComment(ctx, path, issueUID, request, key)
	if err != nil {
		return out, err
	}
	comment := out.Comment
	if !strings.EqualFold(out.Issue.UID, issueUID) || out.Issue.ProjectID != project.ID || (out.Issue.ProjectUID != nil && *out.Issue.ProjectUID != project.UID) || out.Issue.ID <= 0 || comment.IssueID != out.Issue.ID || comment.UID == "" || comment.Author == "" || comment.Body != body || comment.EditedAt != nil || comment.ReplyToUID == nil || *comment.ReplyToUID != target || comment.ReplyKind == nil || *comment.ReplyKind != kind || comment.Teammate == nil || *comment.Teammate != teammate {
		return gen.CommentResponseBody{}, fmt.Errorf("%w: comment receipt mismatch", ErrInvalidResponse)
	}
	return out, nil
}
