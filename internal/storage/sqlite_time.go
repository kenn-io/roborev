package storage

import "time"

const sqliteTimestampLayout = "2006-01-02 15:04:05"

// canonicalSyncTimestamp uses the finest timestamp precision shared by
// SQLite and PostgreSQL. PostgreSQL stores timestamptz values at microsecond
// precision, so generation and same-generation update comparisons must ignore
// sub-microsecond differences introduced before a sync round trip.
func canonicalSyncTimestamp(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

func formatSyncTimestamp(t time.Time) string {
	return canonicalSyncTimestamp(t).Format(time.RFC3339Nano)
}

// sqliteNormalizedTimestampExpr returns a SQLite expression that treats
// RFC3339/RFC3339-with-offset strings and bare SQLite datetime strings as
// comparable UTC instants.
func sqliteNormalizedTimestampExpr(expr string) string {
	return "datetime(CASE WHEN " + expr + " GLOB '*[+-][0-9][0-9]:[0-9][0-9]' OR " + expr + " LIKE '%Z' THEN " + expr + " ELSE " + expr + " || 'Z' END)"
}
