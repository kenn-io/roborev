package kata

import (
	"strings"
	"unicode"
)

const (
	commentRecoveryOpening     = "[roborev:"
	commentRecoveryHexChars    = 24 // The publisher's historical SHA-256 prefix width.
	commentRecoverySeparator   = "\n\n"
	commentRecoveryMarkerBytes = len(commentRecoveryOpening) + commentRecoveryHexChars + 1
)

// CommentRecoveryMarker formats the publisher's existing lowercase digest.
// Digest generation and operation identity remain owned by the publisher.
func CommentRecoveryMarker(digest string) string {
	return commentRecoveryOpening + digest + "]"
}

// FrameCommentBody preserves the publisher's exact recovery wire format.
func FrameCommentBody(body, marker string) string {
	return body + commentRecoverySeparator + marker
}

// evidenceWithoutRecoveryMetadata removes emitted recovery frames and terminal
// tokens from the evidence view, including native edits. Only the exact
// inherited separator is removed; other evidence whitespace stays byte-exact.
func evidenceWithoutRecoveryMetadata(body string) string {
	body = evidenceWithoutEmittedRecoveryFrames(body)
	cut := len(body)
	var tails []string
	for {
		end := len(strings.TrimRightFunc(body[:cut], unicode.IsSpace))
		start := end - commentRecoveryMarkerBytes
		if start < 0 || !validCommentRecoveryMarker(body[start:end]) {
			break
		}
		if end < cut {
			tails = append(tails, body[end:cut])
		}
		cut = start
		if strings.HasSuffix(body[:cut], commentRecoverySeparator) {
			cut -= len(commentRecoverySeparator)
		}
	}
	if len(tails) == 0 {
		return body[:cut]
	}
	// Keep repeated tokens linear in body size and preserve whitespace order.
	var evidence strings.Builder
	evidence.WriteString(body[:cut])
	for i := len(tails) - 1; i >= 0; i-- {
		evidence.WriteString(tails[i])
	}
	return evidence.String()
}

func validCommentRecoveryMarker(marker string) bool {
	if len(marker) != commentRecoveryMarkerBytes || !strings.HasPrefix(marker, commentRecoveryOpening) || marker[len(marker)-1] != ']' {
		return false
	}
	for _, c := range marker[len(commentRecoveryOpening) : len(marker)-1] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Native edits can append prose below an existing emitted frame. Recognize
// that exact blank-line/standalone framing without interpreting Markdown or
// treating inline quoted marker mentions as transport metadata.
func evidenceWithoutEmittedRecoveryFrames(body string) string {
	const opening = commentRecoverySeparator + commentRecoveryOpening
	remaining, search := body, 0
	var evidence strings.Builder
	changed := false
	for {
		frame := strings.Index(remaining[search:], opening)
		if frame < 0 {
			break
		}
		frame += search
		start := frame + len(commentRecoverySeparator)
		end := start + commentRecoveryMarkerBytes
		if end > len(remaining) || !validCommentRecoveryMarker(remaining[start:end]) || end < len(remaining) && remaining[end] != '\n' && !strings.HasPrefix(remaining[end:], "\r\n") {
			search = start + len(commentRecoveryOpening)
			continue
		}
		evidence.WriteString(remaining[:frame])
		remaining = remaining[end:]
		search = 0
		changed = true
	}
	if !changed {
		return body
	}
	evidence.WriteString(remaining)
	return evidence.String()
}
