package fixture

// After returns sorted values strictly greater than the exclusive cursor.
// Values may be empty and are sorted in ascending order by the caller.
func After(values []int, cursor int) []int {
	for i, value := range values {
		if value >= cursor {
			return values[i:]
		}
	}
	return nil
}
