package searchindex

import (
	"context"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/vector"
)

// searchSplit windows review documents into embedding chunks.
var searchSplit = vector.SplitOptions{MaxRunes: searchChunkRunes, Overlap: searchChunkOverlap}

func encodeDocuments(embedder Embedder) vector.EncodeFunc {
	return embedder.EncodeFunc(embedconfig.RoleDocument)
}

func encodeQueries(embedder Embedder) vector.EncodeFunc {
	return embedder.EncodeFunc(embedconfig.RoleQuery)
}

type progressStore struct {
	vector.Store[string, string]
	onDocument func(bool)
}

func (s progressStore) SaveVectors(ctx context.Context, gen, doc string, revision any, vectors []vector.ChunkVector) error {
	if err := s.Store.SaveVectors(ctx, gen, doc, revision, vectors); err != nil {
		return err
	}
	if s.onDocument != nil {
		s.onDocument(len(vectors) > 0)
	}
	return nil
}

// Fill embeds pending mirror documents into gen using kit's scan-and-fill loop.
// store may wrap the sidecar (for example to bound one reconciler turn); a nil
// store uses the index's sqlitevec store.
func (index *Index) Fill(
	ctx context.Context,
	store vector.Store[string, string],
	key string,
	enc vector.EncodeFunc,
	batch embedconfig.Batch,
	onDocument func(bool),
) (vector.FillStats, error) {
	if store == nil {
		store = index.vectors
	}
	split := searchSplit
	batchOptions := []vector.BatchOption{vector.WithBatchSize(batch.Items)}
	if batch.MaxTokens > 0 {
		batchOptions = append(batchOptions, vector.WithBatchTokenBudget(batch.MaxTokens, batch.InputTokenUpperBound))
	}
	progress := progressStore{Store: store, onDocument: onDocument}
	return vector.Fill(ctx, progress, key, enc,
		vector.WithFillSplit[string](split),
		vector.WithFillBatch[string](batchOptions...),
		vector.WithFillBatchErrorIsolation[string](isEmbeddingInputRejected),
		vector.WithFillEncodeError[string](func(_ string, err error) bool { return isEmbeddingInputRejected(err) }),
	)
}

// isEmbeddingInputRejected reports a provider refusal of one input, such as
// text over the model's context limit or content refused by policy. Fill
// skips and stamps only those documents. Kit classifies the provider's error
// body, so a request-wide 400 (unknown model, unsupported dimensions, or an
// unrecognized message) is not an input rejection and aborts the fill.
func isEmbeddingInputRejected(err error) bool {
	apiErr, ok := embeddingAPIError(err)
	return ok && apiErr.InputRejected()
}
