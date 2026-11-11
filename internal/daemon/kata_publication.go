package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
	"uuid"

	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/procutil"
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	record, err := p.db.GetKataPublication(ctx, jobID)
	if err != nil {
		return nil, err
	}
	unlock, err := p.lock(ctx, record.ReviewUUID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return p.run(ctx, jobID, true)
}

func (p *kataPublisher) Inspect(ctx context.Context, jobID int64) (*KataPublicationStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
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
		return p.inspectUnmarked(ctx, record)
	}
	return p.run(ctx, jobID, false)
}

func (p *kataPublisher) current(ctx context.Context, jobID int64) (*storage.KataPublicationRecord, *storage.Review, *kataSelection, *kata.APIClient, kata.PublicationProject, *kata.Publication, error) {
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
		return nil, nil, nil, nil, zero, nil, errors.New("Kata publication hook is no longer authorized")
	}
	if record.Marker != nil && (record.Marker.Version != 1 || (record.Marker.State != "pending" && record.Marker.State != "complete") || (record.Marker.ProjectUID == "") != (record.Marker.RequestFingerprint == "") || record.Marker.KataPublicationSeed != selection.seed || record.Marker.DocumentHash != record.DocumentHash) {
		return nil, nil, nil, nil, zero, nil, errors.New("Kata publication authorization changed")
	}
	opts := []kata.APIOption{kata.WithoutWorkspace()}
	name := selection.seed.Project
	if selection.seed.AliasKind != "" {
		opts = []kata.APIOption{kata.WithWorkspaceAlias(kata.ProjectAlias{Kind: selection.seed.AliasKind, Identity: selection.seed.AliasIdentity})}
		name = ""
	}
	client, err := kata.NewAPIClient("", cfg.Integrations.Kata, opts...)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	project, err := client.ResolvePublicationProject(ctx, name)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	raw, err := json.Marshal(review.StructuredOutput)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	hash, err := storage.KataDocumentHash(raw)
	if err != nil || hash != record.DocumentHash {
		return nil, nil, nil, nil, zero, nil, storage.ErrReviewChanged
	}
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	publication, err := kata.PreparePublicationWithOptions(record.ReviewUUID, record.DocumentHash, review.CreatedAt, doc, selection.seed.Policy, cfg.Integrations.Kata.ResolvedTeammate(), selection.options)
	if err != nil {
		return nil, nil, nil, nil, zero, nil, err
	}
	if record.Marker != nil && ((record.Marker.ProjectUID != "" && record.Marker.ProjectUID != project.UID) || (record.Marker.RequestFingerprint != "" && record.Marker.RequestFingerprint != publication.Fingerprint)) {
		return nil, nil, nil, nil, zero, nil, errors.New("Kata publication destination or request changed")
	}
	return record, review, selection, client, project, publication, nil
}

func (p *kataPublisher) run(ctx context.Context, jobID int64, write bool) (*KataPublicationStatus, error) {
	record, review, _, client, project, publication, err := p.current(ctx, jobID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if client != nil {
			client.CloseIdleConnections()
		}
	}()
	status := &KataPublicationStatus{ReviewUUID: record.ReviewUUID, DocumentHash: record.DocumentHash, DestinationID: kataDestinationID(client.PublicationEndpoint(), project.UID), Project: project, State: "missing"}
	if write && record.Marker == nil {
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
			if !write {
				return status, nil
			}
			err = p.post(ctx, jobID, record, review, project, publication, func(ctx context.Context, c *kata.APIClient, dest kata.PublicationProject, pub *kata.Publication) error {
				_, err := c.CreatePublicationIssue(ctx, dest, pub)
				return err
			})
			if err != nil {
				return status, err
			}
			client.CloseIdleConnections()
			record, review, _, client, project, publication, err = p.current(ctx, jobID)
			if err != nil {
				return status, err
			}
			continue
		}
		found := candidates[0]
		if !publication.MatchesIssueEnvelope(found.Metadata) {
			status.State = "ambiguous"
			return status, errors.New("Kata issue metadata differs")
		}
		issue, err := client.ShowPublicationIssue(ctx, project, found.UID)
		if err != nil {
			return status, err
		}
		if !publication.MatchesIssueEnvelope(issue.Issue.Metadata) {
			status.State = "ambiguous"
			return status, errors.New("Kata issue metadata changed")
		}
		status.Issue = &issue
		status.Roots = publication.Roots(issue)
		if issue.Truncated {
			status.State = "truncated"
			return status, errors.New("Kata comments are truncated")
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
					return status, fmt.Errorf("Kata root %d is %s", root.Position, root.State)
				}
			}
		}
		if missing < 0 {
			if write && record.Marker != nil && record.Marker.State != "complete" {
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
		if !write {
			return status, nil
		}
		if issue.Issue.ClosedAt != nil || issue.Issue.Status == "closed" {
			return status, errors.New("closed Kata issue has missing roots")
		}
		err = p.post(ctx, jobID, record, review, project, publication, func(ctx context.Context, c *kata.APIClient, dest kata.PublicationProject, pub *kata.Publication) error {
			_, err := c.CreatePublicationRoot(ctx, dest, issue.Issue.UID, pub, missing)
			return err
		})
		if err != nil {
			return status, err
		}
		client.CloseIdleConnections()
		record, review, _, client, project, publication, err = p.current(ctx, jobID)
		if err != nil {
			return status, err
		}
	}
}

func (p *kataPublisher) post(ctx context.Context, jobID int64, record *storage.KataPublicationRecord, review *storage.Review, project kata.PublicationProject, publication *kata.Publication, send func(context.Context, *kata.APIClient, kata.PublicationProject, *kata.Publication) error) error {
	deadline := review.CreatedAt.Add(7 * 24 * time.Hour)
	if !p.now().Add(kata.DefaultRequestTimeout).Before(deadline) {
		return errors.New("Kata publication window expired")
	}
	current, _, _, client, dest, prepared, err := p.current(ctx, jobID)
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
		return errors.New("Kata publication window expired")
	}
	checkout, err := kataCheckout(ctx, review.Job)
	if err != nil {
		return err
	}
	postCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return p.db.RunKataPublication(postCtx, record.ReviewUUID, record.DocumentHash, func(ctx context.Context) error {
		if !p.now().Add(kata.DefaultRequestTimeout).Before(deadline) {
			return errors.New("Kata publication window expired")
		}
		locked, err := p.db.GetKataPublicationByUUID(ctx, record.ReviewUUID)
		if err != nil {
			return err
		}
		if locked.Marker == nil || locked.Marker.KataPublicationSeed != current.Marker.KataPublicationSeed || locked.Marker.ProjectUID != dest.UID || locked.Marker.RequestFingerprint != prepared.Fingerprint {
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
		selection, err := selectKataPublication(ctx, cfg, job, checkout)
		if err != nil {
			return err
		}
		if selection == nil || selection.seed != locked.Marker.KataPublicationSeed {
			return errors.New("Kata publication authorization changed")
		}
		raw, err := json.Marshal(review.StructuredOutput)
		if err != nil {
			return err
		}
		doc, err := structuredreview.Decode(raw)
		if err != nil {
			return err
		}
		latest, err := kata.PreparePublicationWithOptions(locked.ReviewUUID, locked.DocumentHash, review.CreatedAt, doc, selection.seed.Policy, cfg.Integrations.Kata.ResolvedTeammate(), selection.options)
		if err != nil {
			return err
		}
		if latest.Fingerprint != locked.Marker.RequestFingerprint {
			return errors.New("Kata publication request changed")
		}
		if !p.now().Add(kata.DefaultRequestTimeout).Before(deadline) {
			return errors.New("Kata publication window expired")
		}
		return send(ctx, client, dest, prepared)
	})
}

// Authorize creates the local authorization for an explicitly requested,
// eligible failing review. It never replaces an existing marker.
func (p *kataPublisher) Authorize(ctx context.Context, jobID int64, policy string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
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
		return errors.New("Kata publication window expired")
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

// inspectUnmarked discovers correlation on a machine that did not authorize
// the write; it never creates a marker or sends a POST.
func (p *kataPublisher) inspectUnmarked(ctx context.Context, record *storage.KataPublicationRecord) (*KataPublicationStatus, error) {
	review, err := p.db.GetReviewByJobIDContext(ctx, record.JobID)
	if err != nil {
		return nil, err
	}
	if review.UUID == nil || *review.UUID != record.ReviewUUID || review.Job == nil {
		return nil, storage.ErrReviewChanged
	}
	cfg := p.cfg.Config()
	if cfg == nil || !cfg.Integrations.Kata.Enabled {
		return nil, errors.New("Kata integration disabled")
	}
	checkout, err := kataCheckout(ctx, review.Job)
	if err != nil {
		return nil, err
	}
	client, err := kata.NewAPIClient(checkout, cfg.Integrations.Kata)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	project, err := client.ResolvePublicationProject(ctx, "")
	if err != nil {
		return nil, err
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
	policy, ok := candidates[0].Metadata["roborev.policy"].(string)
	if !ok {
		status.State = "ambiguous"
		return status, errors.New("Kata issue has no publication policy")
	}
	raw, err := json.Marshal(review.StructuredOutput)
	if err != nil {
		return status, err
	}
	hash, err := storage.KataDocumentHash(raw)
	if err != nil || hash != record.DocumentHash {
		return status, storage.ErrReviewChanged
	}
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return status, err
	}
	pub, err := kata.PreparePublication(record.ReviewUUID, record.DocumentHash, review.CreatedAt, doc, policy, cfg.Integrations.Kata.ResolvedTeammate())
	if err != nil {
		return status, err
	}
	if !pub.MatchesIssueEnvelope(candidates[0].Metadata) {
		status.State = "ambiguous"
		return status, errors.New("Kata issue metadata differs")
	}
	issue, err := client.ShowPublicationIssue(ctx, project, candidates[0].UID)
	if err != nil {
		return status, err
	}
	if !pub.MatchesIssueEnvelope(issue.Issue.Metadata) {
		status.State = "ambiguous"
		return status, errors.New("Kata issue metadata changed")
	}
	status.Issue, status.Roots = &issue, pub.Roots(issue)
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
	if job.WorktreePath == "" {
		return job.RepoPath, nil
	}
	gitOutput := func(dir string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		procutil.HideConsole(cmd)
		cmd.Dir = dir
		out, err := cmd.Output()
		return gitpkg.NormalizeMSYSPath(string(out)), err
	}
	canonical := func(path string) string {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return filepath.Clean(path)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err == nil {
			return filepath.Clean(resolved)
		}
		return filepath.Clean(absolute)
	}
	root, err := gitOutput(job.WorktreePath, "rev-parse", "--show-toplevel")
	if err == nil {
		// A linked worktree's .git file identifies its root even when a shared
		// core.worktree setting makes --show-toplevel point elsewhere.
		if info, statErr := os.Stat(filepath.Join(job.WorktreePath, ".git")); statErr == nil && !info.IsDir() {
			root = job.WorktreePath
		}
		if canonical(root) == canonical(job.WorktreePath) {
			common, commonErr := gitOutput(job.WorktreePath, "rev-parse", "--git-common-dir")
			repoCommon, repoErr := gitOutput(job.RepoPath, "rev-parse", "--git-common-dir")
			if commonErr == nil && repoErr == nil {
				if !filepath.IsAbs(common) {
					common = filepath.Join(job.WorktreePath, common)
				}
				if !filepath.IsAbs(repoCommon) {
					repoCommon = filepath.Join(job.RepoPath, repoCommon)
				}
				if canonical(common) == canonical(repoCommon) {
					return job.WorktreePath, nil
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return job.RepoPath, nil
}
