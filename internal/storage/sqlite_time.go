package storage

// sqliteNormalizedTimestampExpr returns a SQLite expression that treats
// RFC3339/RFC3339-with-offset strings and bare SQLite datetime strings as
// comparable UTC instants.
func sqliteNormalizedTimestampExpr(expr string) string {
	return "datetime(CASE WHEN " + expr + " GLOB '*[+-][0-9][0-9]:[0-9][0-9]' OR " + expr + " LIKE '%Z' THEN " + expr + " ELSE " + expr + " || 'Z' END)"
}
