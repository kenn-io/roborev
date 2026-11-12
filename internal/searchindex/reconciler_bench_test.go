package searchindex

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

// Measure a full scan on 8,000 reviews, 4,000 comments, and 500 MiB of prompts.
func BenchmarkReconcilerReviewEvent(b *testing.B) {
	b.Run("full", func(b *testing.B) {
		db, r := seedReconcilerHistory(b, 8000, 65536)
		benchmarkReconcilerReviewEvent(b, db, r, false)
	})
}

func BenchmarkReconcilerReviewEventHistory(b *testing.B) {
	for _, reviews := range []int{1000, 4000, 8000, 16000} {
		b.Run(fmt.Sprintf("reviews=%d", reviews), func(b *testing.B) {
			db, r := seedReconcilerHistory(b, reviews, 0)
			benchmarkReconcilerReviewEvent(b, db, r, true)
		})
	}
}

func benchmarkReconcilerReviewEvent(b *testing.B, db *storage.DB, r *Reconciler, job bool) {
	b.Helper()
	for b.Loop() {
		_, err := db.ExecContext(b.Context(), `UPDATE reviews SET closed = NOT closed WHERE job_id = 1`)
		require.NoError(b, err)
		if job {
			r.WakeJob(1)
		} else {
			r.Wake()
		}
		for more := true; more; {
			more, err = r.reconcileTurn(b.Context())
			require.NoError(b, err)
		}
	}
}

func seedReconcilerHistory(b testing.TB, reviews, promptSize int) (*storage.DB, *Reconciler) {
	b.Helper()
	db := seedCanonicalReconcilerHistory(b, reviews, promptSize)
	index, err := Open(b.Context(), filepath.Join(b.TempDir(), "search.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, index.Close()) })
	r := NewReconciler(db, index, nil, ReconcilerConfig{})
	for more := true; more; {
		more, err = r.reconcileTurn(b.Context())
		require.NoError(b, err)
	}
	return db, r
}

// BenchmarkReconcilerIdleSweep measures lexical-only sweeps with no changes.
func BenchmarkReconcilerIdleSweep(b *testing.B) {
	for _, count := range []int{1000, 4000, 16000} {
		b.Run(fmt.Sprintf("reviews=%d", count), func(b *testing.B) {
			_, r := seedReconcilerHistory(b, count, 0)
			for b.Loop() {
				r.nextSweep = r.config.Now().Add(-r.config.SweepInterval)
				for more := true; more; {
					var err error
					more, err = r.reconcileTurn(b.Context())
					require.NoError(b, err)
				}
			}
		})
	}
}
