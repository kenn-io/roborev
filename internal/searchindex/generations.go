package searchindex

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// GenerationInfo is the persisted lifecycle state for one vector space.
type GenerationInfo = sqlitevec.GenerationInfo[string]

// GenerationCounts describes current mirror coverage for a generation.
type GenerationCounts = sqlitevec.Coverage

// ResolveGeneration returns the live generation that space owns, creating a
// building one only when none matches. A generation stored under a legacy
// fingerprint is reused as-is, so upgrading never forces a re-embed.
func (index *Index) ResolveGeneration(ctx context.Context, space embedmodel.Descriptor) (string, error) {
	key, exists, model, err := index.plannedGeneration(ctx, space)
	if err != nil || exists {
		return key, err
	}
	if err := index.vectors.EnsureGeneration(ctx, key, model, sqlitevec.StateBuilding); err != nil {
		return "", fmt.Errorf("ensure search vector generation: %w", err)
	}
	return key, nil
}

// plannedGeneration returns the key ResolveGeneration would use without
// creating anything. model is set only when the generation does not exist.
func (index *Index) plannedGeneration(
	ctx context.Context, space embedmodel.Descriptor,
) (string, bool, vector.Generation, error) {
	existing, found, err := index.matchingGeneration(ctx, space)
	if err != nil {
		return "", false, vector.Generation{}, err
	}
	if found {
		return existing.Key, true, vector.Generation{}, nil
	}
	model, err := space.Generation()
	if err != nil {
		return "", false, vector.Generation{}, fmt.Errorf("describe search vector generation: %w", err)
	}
	key, err := index.unusedGenerationKey(ctx, model.Fingerprint())
	if err != nil {
		return "", false, vector.Generation{}, err
	}
	return key, false, model, nil
}

// matchingGeneration prefers the active generation, then the newest building
// one. Retired generations never serve or fill again.
func (index *Index) matchingGeneration(
	ctx context.Context, space embedmodel.Descriptor,
) (GenerationInfo, bool, error) {
	generations, err := index.vectors.Generations(ctx)
	if err != nil {
		return GenerationInfo{}, false, fmt.Errorf("list search vector generations: %w", err)
	}
	var building *GenerationInfo
	for i := len(generations) - 1; i >= 0; i-- {
		generation := generations[i]
		if generation.State == sqlitevec.StateRetired {
			continue
		}
		matches, err := space.Matches(generation.Fingerprint)
		if err != nil {
			return GenerationInfo{}, false, fmt.Errorf("match search vector generation: %w", err)
		}
		if !matches {
			continue
		}
		if generation.State == sqlitevec.StateActive {
			return generation, true, nil
		}
		if building == nil {
			building = &generations[i]
		}
	}
	if building != nil {
		return *building, true, nil
	}
	return GenerationInfo{}, false, nil
}

// unusedGenerationKey keys a new generation by its fingerprint. A retired
// generation keeps its row, so returning to that vector space later needs a
// fresh key.
func (index *Index) unusedGenerationKey(ctx context.Context, fingerprint string) (string, error) {
	generations, err := index.vectors.Generations(ctx)
	if err != nil {
		return "", fmt.Errorf("list search vector generations: %w", err)
	}
	taken := func(key string) bool {
		return slices.ContainsFunc(generations, func(info GenerationInfo) bool { return info.Key == key })
	}
	key := fingerprint
	for attempt := 2; taken(key); attempt++ {
		key = fingerprint + "-" + strconv.Itoa(attempt)
	}
	return key, nil
}

// ActiveGeneration returns the newest active vector generation.
func (index *Index) ActiveGeneration(ctx context.Context) (GenerationInfo, bool, error) {
	return index.vectors.ActiveGeneration(ctx)
}

// ServingGeneration returns the active generation when it belongs to space.
// Search serves only that generation; a building one never answers queries.
func (index *Index) ServingGeneration(ctx context.Context, space embedmodel.Descriptor) (GenerationInfo, bool, error) {
	active, ok, err := index.vectors.ActiveGeneration(ctx)
	if err != nil || !ok {
		return GenerationInfo{}, false, err
	}
	matches, err := space.Matches(active.Fingerprint)
	if err != nil {
		return GenerationInfo{}, false, fmt.Errorf("match active search vector generation: %w", err)
	}
	if !matches {
		return GenerationInfo{}, false, nil
	}
	return active, true, nil
}

// PendingGeneration returns a stable page of documents awaiting a generation.
func (index *Index) PendingGeneration(ctx context.Context, key string, limit int) ([]vector.Pending[string], error) {
	return index.vectors.PendingForGeneration(ctx, key, limit)
}

// SaveGenerationVectors atomically saves vectors for the revision that was read.
func (index *Index) SaveGenerationVectors(
	ctx context.Context, key string, pending vector.Pending[string], vectors []vector.ChunkVector,
) error {
	return index.vectors.SaveVectors(ctx, key, pending.Doc, pending.Revision, vectors)
}

// GenerationCounts returns fresh embedded, intentionally skipped, and pending counts.
func (index *Index) GenerationCounts(ctx context.Context, key string) (GenerationCounts, error) {
	counts, err := index.vectors.Coverage(ctx, key, "")
	if err != nil {
		return GenerationCounts{}, fmt.Errorf("count search vector generation coverage: %w", err)
	}
	return counts, nil
}

// ActivateGeneration cuts over only after every mirrored document is covered,
// then reclaims every retired generation's vectors.
func (index *Index) ActivateGeneration(ctx context.Context, key string) error {
	if err := index.vectors.Activate(ctx, key); err != nil {
		return fmt.Errorf("activate search vector generation: %w", err)
	}
	generations, err := index.vectors.Generations(ctx)
	if err != nil {
		return fmt.Errorf("list search vector generations: %w", err)
	}
	for _, generation := range generations {
		if generation.State != sqlitevec.StateRetired {
			continue
		}
		if err := index.vectors.Reclaim(ctx, generation.Key); err != nil {
			return fmt.Errorf("reclaim retired search vector generation: %w", err)
		}
	}
	return nil
}

func (index *Index) mirrorCount(ctx context.Context) (int64, error) {
	var count int64
	if err := index.db.QueryRowContext(ctx, `SELECT count(*) FROM review_mirror`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count search mirror: %w", err)
	}
	return count, nil
}
