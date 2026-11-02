package storage

import "time"

const sqliteTimestampLayout = "2006-01-02 15:04:05"

// formatSQLiteTimestamp matches SQLite's datetime('now') representation used
// by timestamp column defaults. Keeping explicitly updated timestamps in the
// same format avoids relying on lexical comparisons between SQL and RFC3339.
func formatSQLiteTimestamp(value time.Time) string {
	return value.UTC().Format(sqliteTimestampLayout)
}

// sqliteNormalizedTimestampExpr returns a SQLite expression that treats
// RFC3339/RFC3339-with-offset strings and bare SQLite datetime strings as
// comparable UTC instants.
func sqliteNormalizedTimestampExpr(expr string) string {
	return "datetime(" + sqliteZonedTimestampExpr(expr) + ")"
}

// sqlitePreciseJulianExpr is sqliteNormalizedTimestampExpr as a Julian day
// number. It keeps the fractional seconds that datetime() drops, to the
// millisecond SQLite resolves.
func sqlitePreciseJulianExpr(expr string) string {
	return "julianday(" + sqliteZonedTimestampExpr(expr) + ")"
}

// sqliteZonedTimestampExpr appends Z to a bare SQLite datetime string, which
// is UTC, so SQLite date functions read every stored format the same way.
func sqliteZonedTimestampExpr(expr string) string {
	return "CASE WHEN " + expr + " GLOB '*[+-][0-9][0-9]:[0-9][0-9]' OR " + expr + " LIKE '%Z' THEN " + expr + " ELSE " + expr + " || 'Z' END"
}
