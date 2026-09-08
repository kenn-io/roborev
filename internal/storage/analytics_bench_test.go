package storage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func BenchmarkAggregateAnalyticsTimeSeries(b *testing.B) {
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		bucketCount int
		reviewCount int
	}{
		{name: "720-hourly-1-review", bucketCount: 720, reviewCount: 1},
		{name: "8760-hourly-1-review", bucketCount: 8760, reviewCount: 1},
		{name: "43824-hourly-1-review", bucketCount: 43824, reviewCount: 1},
		{name: "720-hourly-1000-reviews", bucketCount: 720, reviewCount: 1000},
		{name: "8760-hourly-10000-reviews", bucketCount: 8760, reviewCount: 10000},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			rows := make([]analyticsRow, tc.reviewCount)
			for i := range rows {
				rows[i] = analyticsRow{
					project: "synthetic-project", source: "synthetic-source",
					agent: "synthetic-agent", model: "synthetic-model",
					jobType: JobTypeReview, status: JobStatusDone,
					finishedAt:     base.Add(time.Duration(i%tc.bucketCount) * time.Hour),
					reviewDuration: 1.5, attemptDuration: 1,
					verdict: sql.NullInt64{Int64: 1, Valid: true}, eligible: true,
				}
			}
			opts := AnalyticsOptions{
				Since: base, Until: base.Add(time.Duration(tc.bucketCount) * time.Hour),
				Bucket: AnalyticsBucketHour,
			}

			b.ReportAllocs()
			var result *AnalyticsSnapshot
			for b.Loop() {
				var err error
				result, err = aggregateAnalytics(rows, opts)
				require.NoError(b, err)
			}
			require.NotNil(b, result)
		})
	}
}
