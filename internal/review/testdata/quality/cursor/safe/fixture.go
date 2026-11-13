package fixture

// After returns sorted values strictly greater than the exclusive cursor.
// Values may be empty and are sorted in ascending order by the caller.
func After(values []int, cursor int) []int {
	start := 0
	for start < len(values) && values[start] <= cursor {
		start++
	}
	return values[start:]
}
