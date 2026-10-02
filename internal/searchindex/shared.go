package searchindex

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"

	"go.kenn.io/roborev/internal/storage"
)

// sharedLookupPage is how many pending documents one shared-vector lookup
// covers. Imports are cheap next to provider calls, so a fill turn looks up
// more documents than it would embed.
const sharedLookupPage = 256

// SharedVectors is the review-vector cache in the sync database. Daemons
// that sync the same reviews publish the vectors they embed there and import
// each other's instead of embedding the same text again.
type SharedVectors interface {
	LookupSearchVectors(
		ctx context.Context, space string, keys []storage.SearchVectorKey,
	) (map[storage.SearchVectorKey][]storage.SearchVectorChunk, error)
	PublishSearchVectors(
		ctx context.Context, space string, key storage.SearchVectorKey, chunks []storage.SearchVectorChunk,
	) (bool, error)
}

// ShareVectors enables importing and publishing vectors through shared.
func (r *Reconciler) ShareVectors(shared SharedVectors) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shared = shared
}

// ReviewsPushed publishes already embedded vectors for reviews that sync has
// just pushed. A review is usually embedded before sync pushes it, when the
// sync database cannot accept its vectors yet.
func (r *Reconciler) ReviewsPushed(reviewUUIDs []string) {
	r.mu.Lock()
	if r.shared == nil || r.embedder == nil {
		r.mu.Unlock()
		return
	}
	if r.pendingPublish == nil {
		r.pendingPublish = make(map[string]struct{})
	}
	for _, reviewUUID := range reviewUUIDs {
		r.pendingPublish[reviewUUID] = struct{}{}
	}
	r.mu.Unlock()
	r.Wake()
}

func (r *Reconciler) sharedVectors() SharedVectors {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shared
}

func (r *Reconciler) takePendingPublish() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	reviewUUIDs := make([]string, 0, len(r.pendingPublish))
	for reviewUUID := range r.pendingPublish {
		reviewUUIDs = append(reviewUUIDs, reviewUUID)
	}
	r.pendingPublish = nil
	slices.Sort(reviewUUIDs)
	return reviewUUIDs
}

// sharedSpace names the vector space shared rows belong to. It adds the
// chunk windows to the model's generation, because imported vectors replace
// a local split of the same text.
func sharedSpace(space embedmodel.Descriptor) (string, error) {
	generation, err := space.Generation()
	if err != nil {
		return "", fmt.Errorf("describe shared search vector space: %w", err)
	}
	generation.Params["chunk_runes"] = strconv.Itoa(searchSplit.MaxRunes)
	generation.Params["chunk_overlap"] = strconv.Itoa(searchSplit.Overlap)
	return generation.Fingerprint(), nil
}

// sharedKey keys shared rows by the mirror's content_hash, the SHA-256 of the
// exact text that was embedded.
func sharedKey(reviewUUID, contentHash string) storage.SearchVectorKey {
	return storage.SearchVectorKey{ReviewUUID: reviewUUID, ContentSHA256: contentHash}
}

// pendingContentHash returns the content_hash a pending document was read
// with. The vector store reads it as the document's revision.
func pendingContentHash(doc vector.Pending[string]) string {
	switch revision := doc.Revision.(type) {
	case string:
		return revision
	case []byte:
		return string(revision)
	default:
		return ""
	}
}

// publishPending publishes covered reviews discovered by sync or a mirror scan.
// Matching shared rows are skipped. The next scan retries missing rows.
func (r *Reconciler) publishPending(ctx context.Context, shared SharedVectors, space, generation string) error {
	for batch := range slices.Chunk(r.takePendingPublish(), sharedLookupPage) {
		docs, err := r.index.coveredSharedDocuments(ctx, generation, batch)
		if err != nil {
			return err
		}
		keys := make([]storage.SearchVectorKey, len(docs))
		for i, doc := range docs {
			keys[i] = doc.key
		}
		published, err := shared.LookupSearchVectors(ctx, space, keys)
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		for _, doc := range docs {
			if _, ok := published[doc.key]; ok {
				continue
			}
			if _, err := shared.PublishSearchVectors(ctx, space, doc.key, doc.chunks); err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return nil
}

// sharingStore imports vectors other daemons published before Fill embeds
// anything, and publishes the vectors Fill embeds.
type sharingStore struct {
	vector.Store[string, string]
	index  *Index
	shared SharedVectors
	space  string
	keys   map[string]storage.SearchVectorKey
}

func newSharingStore(store vector.Store[string, string], index *Index, shared SharedVectors, space string) *sharingStore {
	return &sharingStore{
		Store: store, index: index, shared: shared, space: space,
		keys: make(map[string]storage.SearchVectorKey),
	}
}

// PendingForGeneration imports every shared hit in one lookup page and
// returns only documents that still need the encoder. A page of hits returns
// nothing, which ends this Fill; the reconciler continues while backlog remains.
func (s *sharingStore) PendingForGeneration(ctx context.Context, gen string, limit int) ([]vector.Pending[string], error) {
	pending, err := s.Store.PendingForGeneration(ctx, gen, max(limit, sharedLookupPage))
	if err != nil {
		return nil, err
	}
	imported, err := s.importShared(ctx, gen, pending)
	if err != nil {
		return nil, err
	}
	misses := make([]vector.Pending[string], 0, min(limit, len(pending)))
	for _, doc := range pending {
		if _, ok := imported[doc.Doc]; ok {
			continue
		}
		if len(misses) == limit {
			break
		}
		misses = append(misses, doc)
	}
	return misses, nil
}

// SaveVectors publishes after a successful local save. Publishing is best
// effort: the sync database may be unreachable or not hold the review yet.
func (s *sharingStore) SaveVectors(
	ctx context.Context, gen, doc string, revision any, vectors []vector.ChunkVector,
) error {
	if err := s.Store.SaveVectors(ctx, gen, doc, revision, vectors); err != nil {
		return err
	}
	if key, ok := s.keys[doc]; ok && len(vectors) > 0 {
		_, _ = s.shared.PublishSearchVectors(ctx, s.space, key, sharedChunks(vectors))
	}
	return nil
}

// importShared saves shared vectors for pending documents. A failed lookup,
// an unusable row, or a failed save leaves the document to the encoder.
func (s *sharingStore) importShared(
	ctx context.Context, gen string, pending []vector.Pending[string],
) (map[string]struct{}, error) {
	docKeys := make([]string, len(pending))
	for i, doc := range pending {
		docKeys[i] = doc.Doc
	}
	reviewUUIDs, err := s.index.reviewUUIDs(ctx, docKeys)
	if err != nil {
		return nil, err
	}
	keys := make([]storage.SearchVectorKey, 0, len(reviewUUIDs))
	for _, doc := range pending {
		reviewUUID, ok := reviewUUIDs[doc.Doc]
		contentHash := pendingContentHash(doc)
		if !ok || contentHash == "" {
			continue
		}
		key := sharedKey(reviewUUID, contentHash)
		s.keys[doc.Doc] = key
		keys = append(keys, key)
	}
	found, err := s.shared.LookupSearchVectors(ctx, s.space, keys)
	if err != nil {
		return nil, ctx.Err()
	}
	imported := make(map[string]struct{})
	for _, doc := range pending {
		key, ok := s.keys[doc.Doc]
		if !ok {
			continue
		}
		vectors, ok := importableVectors(doc.Content, found[key])
		if !ok {
			continue
		}
		if err := s.Store.SaveVectors(ctx, gen, doc.Doc, doc.Revision, vectors); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		imported[doc.Doc] = struct{}{}
	}
	return imported, nil
}

// importableVectors accepts shared chunks only when they line up with the
// local split of content and every vector can take part in cosine distance.
// The store checks dimensions when saving.
func importableVectors(content string, shared []storage.SearchVectorChunk) ([]vector.ChunkVector, bool) {
	chunks := vector.Split(content, searchSplit)
	if len(chunks) == 0 || len(chunks) != len(shared) {
		return nil, false
	}
	vectors := make([]vector.ChunkVector, len(chunks))
	for i, chunk := range chunks {
		if shared[i].Index != chunk.Index || !usableVector(shared[i].Vector) {
			return nil, false
		}
		vectors[i] = vector.ChunkVector{ChunkIndex: chunk.Index, Vector: shared[i].Vector}
	}
	return vectors, true
}

func usableVector(values []float32) bool {
	var squaredNorm float64
	for _, value := range values {
		f := float64(value)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
		squaredNorm += f * f
	}
	return squaredNorm > 0
}

func sharedChunks(vectors []vector.ChunkVector) []storage.SearchVectorChunk {
	chunks := make([]storage.SearchVectorChunk, len(vectors))
	for i, cv := range vectors {
		chunks[i] = storage.SearchVectorChunk{Index: cv.ChunkIndex, Vector: cv.Vector}
	}
	return chunks
}

// reviewUUIDs maps mirror documents to their review UUIDs. Documents without
// one never sync and are left out.
func (index *Index) reviewUUIDs(ctx context.Context, docKeys []string) (map[string]string, error) {
	result := make(map[string]string, len(docKeys))
	if len(docKeys) == 0 {
		return result, nil
	}
	args := make([]any, len(docKeys))
	for i, key := range docKeys {
		args[i] = key
	}
	rows, err := index.db.QueryContext(ctx, `
		SELECT doc_key, review_uuid FROM review_mirror
		 WHERE review_uuid IS NOT NULL AND review_uuid <> ''
		   AND doc_key IN (`+placeholders(len(docKeys))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read search mirror review UUIDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var docKey, reviewUUID string
		if err := rows.Scan(&docKey, &reviewUUID); err != nil {
			return nil, fmt.Errorf("scan search mirror review UUID: %w", err)
		}
		result[docKey] = reviewUUID
	}
	return result, rows.Err()
}

type sharedDocument struct {
	key    storage.SearchVectorKey
	chunks []storage.SearchVectorChunk
}

// coveredSharedDocuments reads the current vectors of the given reviews in
// generation. Stamp-only and stale documents are left out.
func (index *Index) coveredSharedDocuments(
	ctx context.Context, generation string, reviewUUIDs []string,
) ([]sharedDocument, error) {
	if len(reviewUUIDs) == 0 {
		return nil, nil
	}
	snapshot, err := index.vectors.Snapshot(ctx, generation)
	if err != nil {
		return nil, fmt.Errorf("open search vector snapshot: %w", err)
	}
	defer func() { _ = snapshot.Close() }()
	args := make([]any, len(reviewUUIDs))
	for i, reviewUUID := range reviewUUIDs {
		args[i] = reviewUUID
	}
	rows, err := snapshot.CoveredDocs(ctx, sqlitevec.DocQuery{
		Columns: []string{"review_uuid", "content_hash"},
		Where:   "d.review_uuid IN (" + placeholders(len(reviewUUIDs)) + ")",
		Args:    args,
	})
	if err != nil {
		return nil, err
	}
	type coveredDoc struct{ docKey, reviewUUID, contentHash string }
	var covered []coveredDoc
	for rows.Next() {
		var doc coveredDoc
		if err := rows.Scan(&doc.docKey, &doc.reviewUUID, &doc.contentHash); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan covered search document: %w", err)
		}
		covered = append(covered, doc)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("read covered search documents: %w", err)
	}
	docs := make([]sharedDocument, 0, len(covered))
	for _, doc := range covered {
		vectors, err := snapshot.Chunks(ctx, doc.docKey)
		if err != nil {
			return nil, err
		}
		if len(vectors) == 0 {
			continue
		}
		docs = append(docs, sharedDocument{key: sharedKey(doc.reviewUUID, doc.contentHash), chunks: sharedChunks(vectors)})
	}
	return docs, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
