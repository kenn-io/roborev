package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"

	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// KataPublicationStatus reports observed identities and server attribution.
type KataPublicationStatus struct {
	ReviewUUID    uuid.UUID
	DocumentHash  string
	DestinationID string
	Project       kata.PublicationProject
	Issue         *kata.PublicationIssue
	Roots         []kata.PublicationRoot
	State         string
}

type kataPublisher struct {
	db    *storage.DB
	cfg   ConfigGetter
	mu    sync.Mutex
	locks map[uuid.UUID]*kataPublicationLock
	now   func() time.Time
}

// kataPreparedAttempt owns immutable copies of the inputs to one sealed
// request set. Remote observations and destination resolution are never cached.
type kataPreparedAttempt struct {
	publication      *kata.Publication
	reviewID         uuid.UUID
	hash             string
	completed        time.Time
	policy, teammate string
	options          kata.PublicationOptions
}

func (a *kataPreparedAttempt) matches(record *storage.KataPublicationRecord, review *storage.Review, selection *kataSelection, teammate string) bool {
	return a.publication != nil && review.UUID != nil && *review.UUID == a.reviewID &&
		record.ReviewUUID == a.reviewID && record.DocumentHash == a.hash &&
		review.CreatedAt.Equal(a.completed) && selection.seed.Policy == a.policy && teammate == a.teammate &&
		slices.Equal(selection.options.Labels, a.options.Labels) && sameKataPriority(selection.options.Priority, a.options.Priority)
}

var errKataSelectionChanged = errors.New("kata publication selection changed during write")

type kataPublicationLock struct {
	ch    chan struct{}
	users int
}

func newKataPublisher(db *storage.DB, cfg ConfigGetter) *kataPublisher {
	return &kataPublisher{db: db, cfg: cfg, locks: make(map[uuid.UUID]*kataPublicationLock), now: time.Now}
}

func (p *kataPublisher) lock(ctx context.Context, id uuid.UUID) (func(), error) {
	p.mu.Lock()
	entry := p.locks[id]
	if entry == nil {
		entry = &kataPublicationLock{ch: make(chan struct{}, 1)}
		p.locks[id] = entry
	}
	entry.users++
	p.mu.Unlock()
	release := func() {
		p.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(p.locks, id)
		}
		p.mu.Unlock()
	}
	select {
	case entry.ch <- struct{}{}:
		return func() { <-entry.ch; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// Publish is the shared recovery/manual entry point. A missing marker never
// authorizes a write; callers can use Inspect to discover a synced review.
func (p *kataPublisher) Publish(ctx context.Context, jobID int64) (*KataPublicationStatus, error) {
	record, err := p.db.GetKataPublication(ctx, jobID)
	if err != nil {
		return nil, err
	}
	unlock, err := p.lock(ctx, record.ReviewUUID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if record.Marker == nil {
		return nil, errors.New("review has no local publication authorization")
	}
	return p.run(ctx, jobID)
}

func (p *kataPublisher) Inspect(ctx context.Context, jobID int64) (*KataPublicationStatus, error) {
	record, err := p.db.GetKataPublication(ctx, jobID)
	if err != nil {
		return nil, err
	}
	unlock, err := p.lock(ctx, record.ReviewUUID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return p.inspectPublication(ctx, record)
}

func (p *kataPublisher) current(ctx context.Context, jobID int64, attempt *kataPreparedAttempt) (*storage.KataPublicationRecord, *storage.Review, *kataSelection, *kata.APIClient, kata.PublicationProject, *kata.Publication, error) {
	var zero kata.PublicationProject
	record, err := p.db.GetKataPublication(ctx, jobID)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	review, err := p.db.GetReviewByJobIDContext(ctx, jobID)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	if review.UUID == nil || *review.UUID != record.ReviewUUID || review.Job == nil {
		return nil, nil, nil, nil, zero, nil, storage.ErrReviewChanged
	}
	cfg := p.cfg.Config()
	checkout, err := kataCheckout(ctx, review.Job)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	selection, err := selectKataPublication(ctx, cfg, review.Job, checkout)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	if selection == nil {
		return nil, nil, nil, nil, zero, nil, errors.New("kata publication hook is no longer authorized")
	}
	if record.Marker != nil && (record.Marker.Version != 1 || (record.Marker.State != "pending" && record.Marker.State != "complete") || (record.Marker.ProjectUID == "") != (record.Marker.RequestFingerprint == "") || !sameKataAuthorization(record.Marker.KataPublicationSeed, selection.seed) || record.Marker.DocumentHash != record.DocumentHash) {
		return nil, nil, nil, nil, zero, nil, errors.New("kata publication authorization changed")
	}
	selection.inputs.credential, err = selection.inputs.connection.ResolveAPIKey()
	if err != nil {
		return nil, nil, nil, nil, zero, nil, fmt.Errorf("%w: %w", kata.ErrAuthenticationRequired, err)
	}
	client, name, err := kataSelectionClient(selection.inputs.connection, selection.seed)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	project, err := client.ResolvePublicationProject(ctx, name)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	if selection.resolvedUID != "" && selection.resolvedUID != project.UID {
		return nil, nil, nil, nil, zero, nil, errors.New("kata publication destination changed")
	}
	raw, err := json.Marshal(review.StructuredOutput)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	hash, err := storage.KataDocumentHash(raw)
	if err != nil || hash != record.DocumentHash {
		return nil, nil, nil, nil, zero, nil, storage.ErrReviewChanged
	}
	if attempt.publication == nil {
		doc, err := structuredreview.Decode(raw)
		if err != nil {
			return nil, nil, nil, nil, zero, nil, err
		}
		teammate := cfg.Integrations.Kata.ResolvedTeammate()
		publication, err := kata.PreparePublicationWithOptions(record.ReviewUUID, record.DocumentHash, review.CreatedAt, doc, selection.seed.Policy, teammate, selection.options)
		if err != nil {
			return nil, nil, nil, nil, zero, nil, err
		}
		attempt.publication, attempt.reviewID, attempt.hash, attempt.completed = publication, record.ReviewUUID, record.DocumentHash, review.CreatedAt
		attempt.policy, attempt.teammate = selection.seed.Policy, teammate
		attempt.options.Labels = slices.Clone(selection.options.Labels)
		if selection.options.Priority != nil {
			attempt.options.Priority = new(*selection.options.Priority)
		}
	} else if !attempt.matches(record, review, selection, cfg.Integrations.Kata.ResolvedTeammate()) {
		return nil, nil, nil, nil, zero, nil, errors.New("kata publication destination or request changed")
	}
	publication := attempt.publication
	if record.Marker != nil && ((record.Marker.ProjectUID != "" && record.Marker.ProjectUID != project.UID) || (record.Marker.RequestFingerprint != "" && record.Marker.RequestFingerprint != publication.Fingerprint)) {
		return nil, nil, nil, nil, zero, nil, errors.New("kata publication destination or request changed")
	}
	return record, review, selection, client, project, publication, nil
}

func (p *kataPublisher) run(ctx context.Context, jobID int64) (*KataPublicationStatus, error) {
	attempt := &kataPreparedAttempt{}
	record, review, _, client, project, publication, err := p.current(ctx, jobID, attempt)
	if err != nil {
		return nil, err
	}
	defer func() {
		if client != nil {
			client.CloseIdleConnections()
		}
	}()
	status := &KataPublicationStatus{ReviewUUID: record.ReviewUUID, DocumentHash: record.DocumentHash, DestinationID: kataDestinationID(client.PublicationEndpoint(), project.UID), Project: project, State: "missing"}
	if record.Marker == nil {
		return status, errors.New("review has no local publication authorization")
	}
	for {
		candidates, err := client.SearchPublicationIssues(ctx, project, record.ReviewUUID)
		if err != nil {
			return status, err
		}
		if len(candidates) > 1 {
			status.State = "ambiguous"
			return status, errors.New("multiple Kata publication issues")
		}
		if len(candidates) == 0 {
			err = p.post(ctx, jobID, record, review, project, publication, attempt, func(ctx context.Context, c *kata.APIClient, dest kata.PublicationProject, pub *kata.Publication) error {
				_, err := c.CreatePublicationIssue(ctx, dest, pub)
				return err
			})
			if err != nil && !errors.Is(err, errKataSelectionChanged) {
				return status, err
			}
			client.CloseIdleConnections()
			record, review, _, client, project, publication, err = p.current(ctx, jobID, attempt)
			if err != nil {
				return status, err
			}
			continue
		}
		found := candidates[0]
		if !publication.MatchesIssueEnvelope(found.Metadata) {
			status.State = "ambiguous"
			return status, errors.New("kata issue metadata differs")
		}
		issue, err := client.ShowPublicationIssue(ctx, project, found.UID)
		if err != nil {
			return status, err
		}
		if !publication.MatchesIssueEnvelope(issue.Issue.Metadata) {
			status.State = "ambiguous"
			return status, errors.New("kata issue metadata changed")
		}
		status.Issue = &issue
		status.Roots = publication.Roots(issue)
		if issue.Truncated {
			status.State = "truncated"
			return status, errors.New("kata comments are truncated")
		}
		status.State = "complete"
		missing := -1
		for _, root := range status.Roots {
			if root.State != "complete" {
				status.State = "partial"
				if root.State == "missing" && missing < 0 {
					missing = root.Position
				}
				if root.State != "missing" {
					return status, fmt.Errorf("kata root %d is %s", root.Position, root.State)
				}
			}
		}
		if missing < 0 {
			if record.Marker != nil && record.Marker.State != "complete" {
				next := *record.Marker
				next.ProjectUID = project.UID
				next.RequestFingerprint = publication.Fingerprint
				next.State = "complete"
				if err := p.db.UpdateKataPublication(ctx, record, next); err != nil {
					return status, err
				}
			}
			return status, nil
		}
		if issue.Issue.ClosedAt != nil || issue.Issue.Status == "closed" {
			return status, errors.New("closed Kata issue has missing roots")
		}
		err = p.post(ctx, jobID, record, review, project, publication, attempt, func(ctx context.Context, c *kata.APIClient, dest kata.PublicationProject, pub *kata.Publication) error {
			_, err := c.CreatePublicationRoot(ctx, dest, issue.Issue.UID, pub, missing)
			return err
		})
		if err != nil && !errors.Is(err, errKataSelectionChanged) {
			return status, err
		}
		client.CloseIdleConnections()
		record, review, _, client, project, publication, err = p.current(ctx, jobID, attempt)
		if err != nil {
			return status, err
		}
	}
}

func (p *kataPublisher) post(ctx context.Context, jobID int64, record *storage.KataPublicationRecord, review *storage.Review, project kata.PublicationProject, publication *kata.Publication, attempt *kataPreparedAttempt, send func(context.Context, *kata.APIClient, kata.PublicationProject, *kata.Publication) error) error {
	deadline := review.CreatedAt.Add(7 * 24 * time.Hour)
	if !p.now().Add(kata.DefaultRequestTimeout).Before(deadline) {
		return errors.New("kata publication window expired")
	}
	current, _, selection, client, dest, prepared, err := p.current(ctx, jobID, attempt)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if current.ReviewUUID != record.ReviewUUID || current.DocumentHash != record.DocumentHash || current.Marker == nil || dest.UID != project.UID || prepared.Fingerprint != publication.Fingerprint {
		return storage.ErrReviewChanged
	}
	if current.Marker.ProjectUID == "" {
		next := *current.Marker
		next.ProjectUID = dest.UID
		next.RequestFingerprint = prepared.Fingerprint
		if err := p.db.UpdateKataPublication(ctx, current, next); err != nil {
			return err
		}
	}
	if !p.now().Add(kata.DefaultRequestTimeout).Before(deadline) {
		return errors.New("kata publication window expired")
	}
	checkout, err := kataCheckout(ctx, review.Job)
	if err != nil {
		return err
	}
	postCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return p.db.RunKataPublication(postCtx, record.ReviewUUID, record.DocumentHash, func(ctx context.Context) error {
		if !p.now().Add(kata.DefaultRequestTimeout).Before(deadline) {
			return errors.New("kata publication window expired")
		}
		locked, err := p.db.GetKataPublicationByUUID(ctx, record.ReviewUUID)
		if err != nil {
			return err
		}
		if locked.Marker == nil || locked.Marker.Version != 1 || (locked.Marker.State != "pending" && locked.Marker.State != "complete") || locked.Marker.DocumentHash != locked.DocumentHash || locked.Marker.KataPublicationSeed != current.Marker.KataPublicationSeed || locked.Marker.ProjectUID != dest.UID || locked.Marker.RequestFingerprint != prepared.Fingerprint {
			return storage.ErrReviewChanged
		}
		cfg := p.cfg.Config()
		job, err := p.db.GetJobByIDContext(ctx, jobID)
		if err != nil {
			return err
		}
		if job.RepoPath != review.Job.RepoPath || job.WorktreePath != review.Job.WorktreePath {
			return storage.ErrReviewChanged
		}
		inputs, err := collectKataSelection(ctx, cfg, job, checkout)
		if err != nil {
			return err
		}
		if inputs != nil {
			inputs.credential, err = inputs.connection.ResolveAPIKey()
			if err != nil {
				return fmt.Errorf("%w: %w", kata.ErrAuthenticationRequired, err)
			}
		}
		if !selection.inputs.equal(inputs) {
			return errKataSelectionChanged
		}
		if !sameKataAuthorization(selection.seed, locked.Marker.KataPublicationSeed) {
			return errors.New("kata publication authorization changed")
		}
		if selection.resolvedUID != "" && selection.resolvedUID != dest.UID {
			return errors.New("kata publication destination changed")
		}
		latestReview, err := p.db.GetReviewByJobIDContext(ctx, jobID)
		if err != nil {
			return err
		}
		latestSource, err := json.Marshal(latestReview.StructuredOutput)
		if err != nil {
			return err
		}
		latestHash, err := storage.KataDocumentHash(latestSource)
		if err != nil {
			return err
		}
		if locked.JobID != jobID || latestReview.Job == nil || latestReview.Job.ID != jobID || latestHash != attempt.hash || !attempt.matches(locked, latestReview, selection, cfg.Integrations.Kata.ResolvedTeammate()) {
			return errors.New("kata publication request changed")
		}

		if !p.now().Add(kata.DefaultRequestTimeout).Before(deadline) {
			return errors.New("kata publication window expired")
		}
		return send(ctx, client, dest, prepared)
	})
}

// Authorize creates the local authorization for an explicitly requested,
// eligible failing review. It never replaces an existing marker.
func (p *kataPublisher) Authorize(ctx context.Context, jobID int64, policy string) error {
	record, err := p.db.GetKataPublication(ctx, jobID)
	if err != nil {
		return err
	}
	unlock, err := p.lock(ctx, record.ReviewUUID)
	if err != nil {
		return err
	}
	defer unlock()
	review, err := p.db.GetReviewByJobIDContext(ctx, jobID)
	if err != nil {
		return err
	}
	if review.UUID == nil || *review.UUID != record.ReviewUUID || review.Job == nil {
		return storage.ErrReviewChanged
	}
	if !p.now().Add(kata.DefaultRequestTimeout).Before(review.CreatedAt.Add(7 * 24 * time.Hour)) {
		return errors.New("kata publication window expired")
	}
	checkout, err := kataCheckout(ctx, review.Job)
	if err != nil {
		return err
	}
	selection, err := selectKataPublication(ctx, p.cfg.Config(), review.Job, checkout)
	if err != nil {
		return err
	}
	if selection == nil || selection.seed.Policy != policy {
		return errors.New("no matching Kata publication hook")
	}
	return p.db.AuthorizeKataPublication(ctx, record, selection.seed)
}

// inspectPublication reads only the selected destination. It does not require
// the authorization or request fingerprint that govern subsequent writes.
func (p *kataPublisher) inspectPublication(ctx context.Context, record *storage.KataPublicationRecord) (*KataPublicationStatus, error) {
	review, err := p.db.GetReviewByJobIDContext(ctx, record.JobID)
	if err != nil {
		return nil, err
	}
	if review.UUID == nil || *review.UUID != record.ReviewUUID || review.Job == nil {
		return nil, storage.ErrReviewChanged
	}
	cfg := p.cfg.Config()
	if cfg == nil || !cfg.Integrations.Kata.Enabled {
		return nil, errors.New("kata integration disabled")
	}
	checkout, err := kataCheckout(ctx, review.Job)
	if err != nil {
		return nil, err
	}
	var seed *storage.KataPublicationSeed
	if record.Marker != nil {
		if record.Marker.Version != 1 || record.Marker.DocumentHash != record.DocumentHash {
			return nil, storage.ErrReviewChanged
		}
		endpoint, err := kata.NormalizePublicationEndpoint(cfg.Integrations.Kata.Endpoint)
		if err != nil {
			return nil, err
		}
		if endpoint != record.Marker.Endpoint {
			return nil, errors.New("kata publication destination changed")
		}
		seed = &record.Marker.KataPublicationSeed
	} else {
		name, err := kataInspectionProject(cfg, review.Job, checkout)
		if err != nil {
			return nil, err
		}
		if name != "" {
			seed = &storage.KataPublicationSeed{Project: name}
		}
	}
	var client *kata.APIClient
	var name string
	if seed == nil {
		client, err = kata.NewAPIClient(checkout, cfg.Integrations.Kata)
	} else {
		client, name, err = kataSelectionClient(cfg.Integrations.Kata, *seed)
	}
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	project, err := client.ResolvePublicationProject(ctx, name)
	if err != nil {
		return nil, err
	}
	if record.Marker != nil && record.Marker.ProjectUID != "" && record.Marker.ProjectUID != project.UID {
		return nil, errors.New("kata publication destination changed")
	}
	status := &KataPublicationStatus{ReviewUUID: record.ReviewUUID, DocumentHash: record.DocumentHash, DestinationID: kataDestinationID(client.PublicationEndpoint(), project.UID), Project: project, State: "missing"}
	candidates, err := client.SearchPublicationIssues(ctx, project, record.ReviewUUID)
	if err != nil || len(candidates) == 0 {
		return status, err
	}
	if len(candidates) != 1 {
		status.State = "ambiguous"
		return status, errors.New("multiple Kata publication issues")
	}
	raw, err := json.Marshal(review.StructuredOutput)
	if err != nil {
		return status, err
	}
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return status, err
	}
	matches := func(metadata map[string]any) bool {
		envelope, err := kata.ReadPublicationEnvelope(metadata)
		return err == nil && envelope.ReviewUUID == record.ReviewUUID && envelope.DocumentHash == record.DocumentHash && envelope.CompletedAt.Equal(review.CreatedAt) && envelope.FindingCount == len(doc.Findings) && (record.Marker == nil || envelope.Policy == record.Marker.Policy)
	}
	if !matches(candidates[0].Metadata) {
		status.State = "ambiguous"
		return status, errors.New("kata issue metadata differs")
	}
	issue, err := client.ShowPublicationIssue(ctx, project, candidates[0].UID)
	if err != nil {
		return status, err
	}
	if !matches(issue.Issue.Metadata) {
		status.State = "ambiguous"
		return status, errors.New("kata issue metadata changed")
	}
	envelope, _ := kata.ReadPublicationEnvelope(issue.Issue.Metadata)
	expected, err := kata.PublicationFromSource(record.ReviewUUID, record.DocumentHash, review.CreatedAt, doc, envelope.Policy)
	if err != nil {
		return status, err
	}
	status.Issue, status.Roots = &issue, expected.Roots(issue)
	status.State = "complete"
	if issue.Truncated {
		status.State = "truncated"
	} else {
		for _, root := range status.Roots {
			if root.State != "complete" {
				status.State = "partial"
				break
			}
		}
	}
	return status, nil
}

// kataDestinationID identifies the configured service and project without exposing the endpoint.
func kataDestinationID(endpoint, projectUID string) string {
	sum := sha256.Sum256([]byte(endpoint + "\x00" + projectUID))
	return hex.EncodeToString(sum[:])
}

// kataCheckout validates the caller's worktree using the attempt context, so
// an unresponsive Git process cannot keep publication alive at shutdown.
func kataCheckout(ctx context.Context, job *storage.ReviewJob) (string, error) {
	if job.WorktreePath != "" && gitpkg.ValidateWorktreeForRepoContext(ctx, job.WorktreePath, job.RepoPath) {
		return job.WorktreePath, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return job.RepoPath, nil
}
