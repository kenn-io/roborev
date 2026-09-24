package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/storage"
)

// VectorExchange is the shared review-vector cache in the sync database.
// *storage.VectorExchange implements it.
type VectorExchange interface {
	Target(context.Context) (string, error)
	TouchGeneration(context.Context, storage.VectorGeneration) error
	Lookup(context.Context, string, []storage.VectorKey) ([]storage.VectorRecord, error)
	Claim(context.Context, string, storage.VectorKey, time.Duration) (bool, error)
	Publish(context.Context, string, []storage.VectorRecord) error
	Release(context.Context, string, []storage.VectorKey) error
	Discard(context.Context, string, storage.VectorKey) error
	CollectGarbage(context.Context, time.Duration) (int64, error)
}

// SetVectorExchange enables the shared vector cache. Call it before Run; a
// nil exchange keeps today's local-only behavior.
func (r *Reconciler) SetVectorExchange(exchange VectorExchange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exchange = exchange
	if exchange != nil && r.embedder != nil {
		r.health.SourceStatus = SourceUnreachable
	}
}

func (r *Reconciler) vectorExchange() VectorExchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exchange
}

func exchangeStatusFor(err error) string {
	if errors.Is(err, storage.ErrVectorExchangeUnsupported) {
		return SourceUnsupported
	}
	return SourceUnreachable
}

// fillSharedGeneration is fillGeneration for a daemon whose sync database
// holds the shared vector cache. Shared documents are imported or claimed;
// only local documents, claimed documents and fallback-due documents reach
// the provider.
func (r *Reconciler) fillSharedGeneration(ctx context.Context, exchange VectorExchange) (bool, error) {
	exchangeCtx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	target, targetErr := exchange.Target(exchangeCtx)
	cancel()
	status := SourceOK
	if targetErr != nil {
		status = exchangeStatusFor(targetErr)
	}
	if status == SourceUnsupported {
		r.setExchangeHealth(status, ExchangeCounts{})
		return r.fillGeneration(ctx)
	}

	model := r.embedder.Generation()
	key, err := r.index.EnsureGeneration(ctx, model)
	if err != nil {
		return false, err
	}
	now := r.config.Now()
	cutoff := now.Add(-r.config.LocalFallbackAfter)
	if err := r.index.observeSharedPending(ctx, key, now); err != nil {
		return false, err
	}

	exchangeMore := false
	if status == SourceOK {
		exchangeCtx, cancel := context.WithTimeout(ctx, exchangeTimeout)
		r.maintainExchange(exchangeCtx, exchange, model, now)
		exchangeMore, err = r.importShared(exchangeCtx, exchange, key, target, model.Dimensions, now, cutoff)
		cancel()
		if err != nil {
			if !isExchangeFailure(err) {
				return false, err
			}
			status = SourceUnreachable
		}
	}

	counts, err := r.index.GenerationCounts(ctx, key)
	if err != nil {
		return false, err
	}
	activeGeneration, err := r.activeFingerprint(ctx)
	if err != nil {
		return false, err
	}
	if err := r.refreshExchangeHealth(ctx, status, key, now, cutoff); err != nil {
		return false, err
	}
	r.beginGeneration(key, counts, activeGeneration)
	if activeGeneration != key {
		if activated, err := r.tryActivateShared(ctx, key, now, cutoff); err != nil {
			return false, err
		} else if activated {
			activeGeneration = key
		}
	}

	var fillErr error
	allowClaimed := status == SourceOK
	fillable, err := r.index.localFillBacklog(ctx, key, now, cutoff, allowClaimed)
	if err != nil {
		return false, err
	}
	if fillable > 0 {
		fillCtx, cancel := context.WithTimeout(ctx, r.config.MaxFillTime)
		_, fillErr = r.index.Fill(
			fillCtx,
			&turnLimitedStore{
				Store:     &localFillStore{Store: r.index.vectors, index: r.index, now: now, cutoff: cutoff, allowClaimed: allowClaimed},
				remaining: r.config.MaxFillBatches,
			},
			key,
			encodeDocuments(r.embedder),
			max(r.embedder.BatchSize(), 1),
			nil,
		)
		cancel()
	}

	if status == SourceOK {
		exchangeCtx, cancel := context.WithTimeout(ctx, exchangeTimeout)
		publishMore, err := r.publishShared(exchangeCtx, exchange, key, target, model.Dimensions, now)
		cancel()
		if err != nil {
			if !isExchangeFailure(err) {
				return false, err
			}
			status = SourceUnreachable
		}
		exchangeMore = exchangeMore || publishMore
	}

	if activeGeneration != key && fillErr == nil {
		if activated, err := r.tryActivateShared(ctx, key, now, cutoff); err != nil {
			return false, err
		} else if activated {
			activeGeneration = key
		}
	}
	countsAfter, err := r.index.GenerationCounts(ctx, key)
	if err != nil {
		return false, err
	}
	// Exchange counts first: a reader that sees the new backlog also sees them.
	if err := r.refreshExchangeHealth(ctx, status, key, now, cutoff); err != nil {
		return false, err
	}
	r.updateGenerationHealth(key, countsAfter, activeGeneration)
	if err := r.scheduleExchange(ctx, key, now); err != nil {
		return false, err
	}

	remaining, err := r.index.localFillBacklog(ctx, key, now, cutoff, status == SourceOK)
	if err != nil {
		return false, err
	}
	more := remaining > 0 || exchangeMore
	if fillErr != nil {
		if errors.Is(fillErr, context.Canceled) || errors.Is(fillErr, context.DeadlineExceeded) {
			return more, fillErr
		}
		return more, providerFailure(fillErr)
	}
	return more, nil
}

func (r *Reconciler) activeFingerprint(ctx context.Context) (string, error) {
	active, ok, err := r.index.ActiveGeneration(ctx)
	if err != nil || !ok {
		return "", err
	}
	return active.Fingerprint, nil
}

func (r *Reconciler) tryActivateShared(ctx context.Context, key string, now, cutoff time.Time) (bool, error) {
	blockers, err := r.index.sharedActivationBlockers(ctx, r.index.db, key, now, cutoff)
	if err != nil || blockers != 0 {
		return false, err
	}
	if err := r.index.ActivateSharedGeneration(ctx, key, now, cutoff); err != nil {
		return false, err
	}
	return true, nil
}

// exchangeFailure marks errors from the shared cache, which degrade the
// source status instead of failing the reconciliation turn.
type exchangeFailure struct{ cause error }

func (e *exchangeFailure) Error() string { return "vector exchange: " + e.cause.Error() }
func (e *exchangeFailure) Unwrap() error { return e.cause }

func isExchangeFailure(err error) bool {
	_, ok := errors.AsType[*exchangeFailure](err)
	return ok
}

func (r *Reconciler) maintainExchange(ctx context.Context, exchange VectorExchange, model vector.Generation, now time.Time) {
	r.mu.Lock()
	touchDue := r.lastExchangeTouch.IsZero() || now.Sub(r.lastExchangeTouch) >= exchangeTouchInterval
	gcDue := r.lastExchangeGC.IsZero() || now.Sub(r.lastExchangeGC) >= exchangeGCInterval
	r.mu.Unlock()
	if touchDue {
		err := exchange.TouchGeneration(ctx, storage.VectorGeneration{
			Fingerprint: model.Fingerprint(), Model: model.Model,
			Dimensions: model.Dimensions, Params: model.Params,
		})
		if err == nil {
			r.mu.Lock()
			r.lastExchangeTouch = now
			r.mu.Unlock()
		}
	}
	if gcDue {
		if _, err := exchange.CollectGarbage(ctx, exchangeUnusedGeneration); err == nil {
			r.mu.Lock()
			r.lastExchangeGC = now
			r.mu.Unlock()
		}
	}
}

// importShared looks up due shared documents, imports exact matches, and
// claims misses this daemon should embed. It reports whether more due
// documents remain.
func (r *Reconciler) importShared(
	ctx context.Context, exchange VectorExchange, key, target string, dims int, now, cutoff time.Time,
) (bool, error) {
	candidates, err := r.index.dueSharedCandidates(ctx, key, now, cutoff, exchangeLookupBatch)
	if err != nil || len(candidates) == 0 {
		return false, err
	}
	keys := make([]storage.VectorKey, len(candidates))
	for i, candidate := range candidates {
		keys[i] = storage.VectorKey{ReviewUUID: candidate.DocKey, ContentSHA256: candidate.ContentHash}
	}
	records, err := exchange.Lookup(ctx, key, keys)
	if err != nil {
		return false, &exchangeFailure{cause: err}
	}
	byKey := make(map[storage.VectorKey]storage.VectorRecord, len(records))
	for _, record := range records {
		byKey[record.Key] = record
	}
	claimBudget := r.config.MaxFillBatches
	deferred := false
	for i, candidate := range candidates {
		if record, ok := byKey[keys[i]]; ok {
			var content string
			err := r.index.db.QueryRowContext(ctx,
				`SELECT content FROM review_mirror WHERE doc_key = ? AND content_hash = ?`,
				candidate.DocKey, candidate.ContentHash).Scan(&content)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return false, err
			}
			vectors, invalid := validateExchangeRecord(record, content, dims)
			if invalid == nil {
				err := r.index.SaveGenerationVectors(ctx, key,
					vector.Pending[string]{Doc: candidate.DocKey, Revision: candidate.ContentHash}, vectors)
				if errors.Is(err, vector.ErrStale) {
					continue
				}
				if err != nil {
					return false, err
				}
				if err := r.index.recordExchanged(ctx, key, candidate.DocKey, candidate.ContentHash,
					exchangeOriginPeer, target, now); err != nil {
					return false, err
				}
				continue
			}
			r.addRejected()
			if err := exchange.Discard(ctx, key, keys[i]); err != nil {
				return false, &exchangeFailure{cause: err}
			}
		}
		claimAt := candidate.FirstPendingAt.Add(r.config.PeerClaimDelay)
		eligible := candidate.ShareState == storage.SearchShareOwn || !now.Before(claimAt)
		if eligible && claimBudget > 0 {
			claimBudget--
			held, err := exchange.Claim(ctx, key, keys[i], r.config.ClaimTTL)
			if err != nil {
				return false, &exchangeFailure{cause: err}
			}
			if held {
				if err := r.index.recordClaim(ctx, key, candidate, now.Add(r.config.ClaimTTL)); err != nil {
					return false, err
				}
				continue
			}
		}
		next := now.Add(r.lookupBackoff(candidate.Attempts + 1))
		switch {
		case eligible && claimBudget == 0:
			next = now
			deferred = true
		case !eligible && claimAt.Before(next):
			next = claimAt
		}
		if err := r.index.recordLookup(ctx, key, candidate, next); err != nil {
			return false, err
		}
	}
	return deferred || len(candidates) == exchangeLookupBatch, nil
}

func (r *Reconciler) lookupBackoff(attempt int) time.Duration {
	delay := r.config.LookupMinBackoff
	for i := 1; i < attempt && delay < exchangeLookupMaxBackoff; i++ {
		delay *= 2
	}
	return min(delay, exchangeLookupMaxBackoff)
}

// publishShared uploads covered shared documents missing from the exchange
// and releases claims on documents this daemon finished embedding.
func (r *Reconciler) publishShared(
	ctx context.Context, exchange VectorExchange, key, target string, dims int, now time.Time,
) (bool, error) {
	candidates, err := r.index.publishCandidates(ctx, key, target, exchangeLookupBatch)
	if err != nil {
		return false, err
	}
	records := make([]storage.VectorRecord, 0, len(candidates))
	for _, candidate := range candidates {
		chunks, err := r.index.readChunkVectors(ctx, key, candidate.DocKey)
		if err != nil {
			return false, err
		}
		status := storage.VectorStatusOK
		if len(chunks) == 0 {
			status = storage.VectorStatusSkipped
		}
		records = append(records, storage.VectorRecord{
			Key:    storage.VectorKey{ReviewUUID: candidate.DocKey, ContentSHA256: candidate.ContentHash},
			Status: status, Dims: dims, Chunks: chunks,
		})
	}
	if err := exchange.Publish(ctx, key, records); err != nil {
		return false, &exchangeFailure{cause: err}
	}
	for _, candidate := range candidates {
		if err := r.index.recordExchanged(ctx, key, candidate.DocKey, candidate.ContentHash,
			exchangeOriginLocal, target, now); err != nil {
			return false, err
		}
	}
	finished, err := r.index.finishedClaims(ctx, key)
	if err != nil {
		return false, err
	}
	if len(finished) > 0 {
		keys := make([]storage.VectorKey, len(finished))
		for i, claim := range finished {
			keys[i] = storage.VectorKey{ReviewUUID: claim.DocKey, ContentSHA256: claim.ContentHash}
		}
		if err := exchange.Release(ctx, key, keys); err != nil {
			return false, &exchangeFailure{cause: err}
		}
		for _, claim := range finished {
			if err := r.index.clearClaim(ctx, key, claim.DocKey, claim.ContentHash); err != nil {
				return false, err
			}
		}
	}
	return len(candidates) == exchangeLookupBatch, nil
}

func (r *Reconciler) addRejected() {
	r.mu.Lock()
	r.health.Rejected++
	r.mu.Unlock()
}

func (r *Reconciler) refreshExchangeHealth(ctx context.Context, status, key string, now, cutoff time.Time) error {
	counts, err := r.index.ExchangeCounts(ctx, key, now, cutoff)
	if err != nil {
		return err
	}
	r.setExchangeHealth(status, counts)
	return nil
}

func (r *Reconciler) setExchangeHealth(status string, counts ExchangeCounts) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health.SourceStatus = status
	r.health.Imported = counts.Imported
	r.health.Published = counts.Published
	r.health.AwaitingPeer = counts.AwaitingPeer
	r.health.ClaimsHeld = counts.ClaimsHeld
}

func (r *Reconciler) scheduleExchange(ctx context.Context, key string, now time.Time) error {
	due, err := r.index.nextExchangeDue(ctx, key, r.config.LocalFallbackAfter)
	if err != nil {
		return err
	}
	// Never poll faster than the lookup backoff, even while the exchange is
	// unreachable and every shared document is technically due.
	if earliest := now.Add(r.config.LookupMinBackoff); !due.IsZero() && due.Before(earliest) {
		due = earliest
	}
	r.mu.Lock()
	r.exchangeDue = due
	r.mu.Unlock()
	return nil
}

// idleDelay is how long Run sleeps without a wake: the safety sweep, or
// sooner when a shared document's next lookup or fallback is due.
func (r *Reconciler) idleDelay() time.Duration {
	r.mu.Lock()
	due := r.exchangeDue
	r.mu.Unlock()
	delay := r.config.SweepInterval
	if due.IsZero() {
		return delay
	}
	if until := due.Sub(r.config.Now()); until < delay {
		delay = max(until, r.config.MinBackoff)
	}
	return delay
}

// localFillStore narrows kit Fill to documents this daemon may embed itself.
type localFillStore struct {
	vector.Store[string, string]
	index        *Index
	now          time.Time
	cutoff       time.Time
	allowClaimed bool
}

func (s *localFillStore) PendingForGeneration(ctx context.Context, key string, limit int) ([]vector.Pending[string], error) {
	ordinal, err := s.index.generationOrdinal(ctx, key)
	if err != nil {
		return nil, err
	}
	rows, err := s.index.db.QueryContext(ctx, `
		SELECT m.doc_key, m.content, m.content_hash`+sharedPendingFrom+`
		 WHERE `+notCoveredSQL+` AND `+localFillSQL+`
		 ORDER BY CASE WHEN COALESCE(x.claim_expires_at, 0) > ? THEN 0 ELSE 1 END, m.doc_key
		 LIMIT ?`,
		ordinal, key, s.cutoff.Unix(), s.allowClaimed, s.now.Unix(), s.now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var pending []vector.Pending[string]
	for rows.Next() {
		var doc, content, hash string
		if err := rows.Scan(&doc, &content, &hash); err != nil {
			return nil, err
		}
		pending = append(pending, vector.Pending[string]{Doc: doc, Content: content, Revision: hash})
	}
	return pending, rows.Err()
}
