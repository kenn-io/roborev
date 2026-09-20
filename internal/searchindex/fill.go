package searchindex

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/embedding"
)

func encodeDocuments(embedder Embedder) vector.EncodeFunc {
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		return embedder.Embed(ctx, embedding.InputDocument, texts)
	}
}

func encodeQueries(embedder Embedder) vector.EncodeFunc {
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		return embedder.Embed(ctx, embedding.InputQuery, texts)
	}
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
	batchSize int,
	onDocument func(bool),
) (vector.FillStats, error) {
	if store == nil {
		store = index.vectors
	}
	split := vector.SplitOptions{MaxRunes: searchChunkRunes, Overlap: searchChunkOverlap}
	batchOptions := []vector.BatchOption{vector.WithBatchSize(batchSize)}
	progress := progressStore{Store: store, onDocument: onDocument}
	return vector.Fill(ctx, progress, key, enc,
		vector.WithFillSplit[string](split),
		vector.WithFillBatch[string](batchOptions...),
		vector.WithFillBatchErrorIsolation[string](isEmbeddingBadRequest),
		vector.WithFillEncodeError[string](func(doc string, err error) bool {
			if !isEmbeddingBadRequest(err) {
				return false
			}
			return index.contentSpecific400(ctx, doc, enc, split, batchOptions)
		}),
	)
}

func isEmbeddingBadRequest(err error) bool {
	var apiErr *embedding.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest
}

// contentSpecific400 reports whether a 400 is provably the document's content.
// It rebuilds the document's request shape with benign text; success means the
// endpoint accepts that shape, so poison-skip is safe. Any replay failure keeps
// the document pending by aborting the fill.
func (index *Index) contentSpecific400(
	ctx context.Context, doc string, enc vector.EncodeFunc, split vector.SplitOptions, batchOptions []vector.BatchOption,
) bool {
	content, err := index.mirrorContent(ctx, doc)
	if err != nil {
		return false
	}
	chunks := vector.Split(content, split)
	for i := range chunks {
		chunks[i].Text = benignChunkText(chunks[i].Text)
	}
	_, err = vector.EncodeBatched(ctx, enc, chunks, batchOptions...)
	return err == nil
}

func (index *Index) mirrorContent(ctx context.Context, doc string) (string, error) {
	var content string
	if err := index.db.QueryRowContext(ctx,
		`SELECT content FROM review_mirror WHERE doc_key = ?`, doc).Scan(&content); err != nil {
		return "", err
	}
	return content, nil
}

func benignChunkText(text string) string {
	return strings.Repeat("a", utf8.RuneCountInString(text))
}
