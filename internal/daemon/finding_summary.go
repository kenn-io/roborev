package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/storage"
)

// ErrFindingKataAccessDenied is an explicit access failure, never an outage
// that can silently degrade to local measurements.
var ErrFindingKataAccessDenied = errors.New("finding context access denied")

// A future parent adapter supplies decoded, validated facts. The default is
// nil because the live mapper/complete-reader contract is not available yet.
type findingKataReader func(context.Context, storage.FindingKataSource) (storage.FindingKataReview, error)

func (s *Server) findingStats(ctx context.Context, opts storage.SummaryOptions, group string) (*storage.FindingStats, error) {
	stats, err := s.db.GetFindingStats(opts, group)
	if err != nil {
		return nil, err
	}
	if err = s.enrichFindingStats(ctx, stats); err != nil {
		return nil, err
	}
	return stats, nil
}

func (s *Server) enrichFindingStats(ctx context.Context, stats *storage.FindingStats) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if stats == nil || s.findingKataReader == nil {
		return nil
	}
	sources := stats.KataSources()
	if len(sources) == 0 {
		return nil
	}
	results, err := readFindingKata(ctx, sources, s.findingKataReader)
	if err != nil {
		return err
	}
	// The original transaction has ended. Reselect before applying remote facts
	// so a concurrent rerun cannot attach a root to the same positional index.
	current, err := s.db.GetFindingStats(storage.SummaryOptions{Since: stats.Since, AsOf: stats.AsOf, RepoPath: stats.RepoPath, Branch: stats.Branch, MinSamples: stats.MinSamples}, stats.GroupBy)
	if err != nil {
		return err
	}
	valid := map[storage.FindingKataSource]bool{}
	for _, source := range current.KataSources() {
		valid[source] = true
	}
	for i := range results {
		if !results[i].Unavailable && !valid[results[i].Source] {
			results[i].Stale = true
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stats.ApplyKata(results)
	return nil
}

// The two-second budget and two in-flight reads apply to the whole invocation,
// per the aggregate read contract. Readers must honor context cancellation;
// joining both workers avoids orphan requests on timeout or explicit failure.
func readFindingKata(ctx context.Context, sources []storage.FindingKataSource, reader findingKataReader) ([]storage.FindingKataReview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	budget, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	seen := map[string]bool{}
	var unique []storage.FindingKataSource
	for _, source := range sources {
		if !seen[source.ReviewUUID] {
			seen[source.ReviewUUID] = true
			unique = append(unique, source)
		}
	}
	results := make([]storage.FindingKataReview, len(unique))
	jobs := make(chan int, len(unique))
	for i, source := range unique {
		results[i] = storage.FindingKataReview{Source: source, Unavailable: true}
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	var accessErr error
	var mu sync.Mutex
	for range min(2, len(unique)) {
		wg.Go(func() {
			for i := range jobs {
				if budget.Err() != nil {
					return
				}
				result, err := reader(budget, unique[i])
				if errors.Is(err, ErrFindingKataAccessDenied) {
					mu.Lock()
					accessErr = err
					mu.Unlock()
					cancel()
					return
				}
				if err == nil && budget.Err() == nil {
					if result.Source != unique[i] {
						result.Source = unique[i]
						result.Stale = true
					}
					results[i] = result
				}
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accessErr != nil {
		return nil, accessErr
	}
	return results, nil
}
