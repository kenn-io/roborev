package kata

import (
	"regexp"
	"strings"
)

// ParseRefs extracts kata issue references from a commit message and returns
// bare short_ids. Two qualified forms are recognized: the literal marker
// "kata#<id>" (a convention, not a project name) and "<projectName>#<id>" for
// the bound project. Both denote a kata in this workspace, so both normalize to
// just "<id>" (e.g. "kata#abc4" and "roborev#abc4" in a roborev-bound repo both
// yield "abc4"). Matches are deduplicated and returned in first-seen order.
// Bare unqualified ids are not scraped, and foreign "<other>#<id>" refs are not
// matched. Ids use Crockford base32 (kata's ULID-suffix alphabet, excluding
// i/l/o/u), so prose like "kata#look" does not trigger a bogus lookup.
func ParseRefs(commitMsg, projectName string) []string {
	prefixes := []string{"kata"}
	if projectName != "" && projectName != "kata" {
		prefixes = append(prefixes, projectName)
	}
	quoted := make([]string, len(prefixes))
	for i, p := range prefixes {
		quoted[i] = regexp.QuoteMeta(p)
	}
	re := regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)#([0-9a-hjkmnp-tv-z]{4,26})\b`)

	var out []string
	seen := make(map[string]bool)
	for _, m := range re.FindAllStringSubmatch(commitMsg, -1) {
		id := strings.ToLower(m[1])
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
