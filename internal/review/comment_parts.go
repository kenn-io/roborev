package review

import (
	"fmt"
	"strings"
)

// CommentPartMarker identifies the primary comment or one of its continuations.
// Keeping continuation markers distinct lets existing primary-comment lookups
// keep selecting the beginning of the review.
func CommentPartMarker(marker string, part int) string {
	if part == 0 {
		return marker
	}
	return strings.TrimSuffix(marker, " -->") + fmt.Sprintf(" part=%d -->", part+1)
}

// CommentParts preserves a complete review across provider-sized comments.
// prepare applies provider formatting before measuring each candidate, so its
// marker and escaping count toward the existing comment limit.
func CommentParts(body string, prepare func(string, int) string) []string {
	var parts []string
	for {
		part := len(parts)
		end := min(len(body), MaxCommentLen)
		for {
			end = len(TrimPartialRune(body[:end]))
			prepared := prepare(body[:end], part)
			overflow := len(prepared) - MaxCommentLen
			if overflow <= 0 {
				break
			}
			end -= overflow
		}
		// Prefer a line boundary when a comment needs more than one part.
		if end < len(body) {
			if newline := strings.LastIndexByte(body[:end], '\n'); newline >= 0 {
				end = newline + 1
			}
		}
		parts = append(parts, prepare(body[:end], part))
		body = body[end:]
		if body == "" {
			return parts
		}
	}
}
