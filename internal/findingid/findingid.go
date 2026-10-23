// Package findingid names the findings of a structured review.
//
// A finding has no identity in the agent's output. Roborev derives one from
// facts the review already has: the review's UUID, the finding's zero-based
// position in the document, and a hash of the canonical document. The result
// is a selector such as "f:…" that stays the same every time it is derived, on
// any machine that holds the same review, and that is opaque to people and
// agents: nothing about the finding can be read from it.
//
// A selector is always given in full. Nothing here matches a shortened one.
//
// The package is pure. Storing snapshots of findings is internal/storage's job.
package findingid

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strings"
	"uuid"

	"go.kenn.io/roborev/pkg/structuredreview"
)

// Prefix starts every selector.
const Prefix = "f:"

// hashPrefixLen is how many leading bytes of the document hash a selector
// carries. The review UUID and the index already name the finding; the hash
// prefix only detects a document that changed underneath them, and 64 bits make
// an accidental match negligible. The full hash is stored beside the snapshot.
const hashPrefixLen = 8

// alphabet is RFC 4648 base32 in lower case, so a selector is easy to read
// aloud and to type, and has no characters a shell or a URL treats specially.
const alphabet = "abcdefghijklmnopqrstuvwxyz234567"

// encoding has no padding. Parse re-encodes what it decodes and compares, so
// every selector has exactly one spelling.
var encoding = base32.NewEncoding(alphabet).WithPadding(base32.NoPadding)

// Errors a caller can test with errors.Is.
var (
	// ErrInvalid means the text is not a selector, or the parts given to New
	// cannot form one.
	ErrInvalid = errors.New("finding selector is not valid")
	// ErrNotFound means no known finding has the selector. Callers return it;
	// this package knows no findings.
	ErrNotFound = errors.New("no finding matches the selector")
)

// ID is the decoded form of a selector.
type ID struct {
	ReviewUUID uuid.UUID
	// Index is the finding's zero-based position in the review document.
	Index      int
	hashPrefix [hashPrefixLen]byte
}

// New builds the ID of the finding at index in the review whose canonical
// document hash is documentHash (a hex SHA-256, as DocumentHash returns).
func New(reviewUUID uuid.UUID, index int, documentHash string) (ID, error) {
	if reviewUUID == uuid.Nil() {
		return ID{}, fmt.Errorf("%w: the review has no UUID", ErrInvalid)
	}
	if index < 0 {
		return ID{}, fmt.Errorf("%w: negative finding index %d", ErrInvalid, index)
	}
	raw, err := hex.DecodeString(documentHash)
	if err != nil || len(raw) != sha256.Size {
		return ID{}, fmt.Errorf("%w: the document hash is not a hex SHA-256", ErrInvalid)
	}
	id := ID{ReviewUUID: reviewUUID, Index: index}
	copy(id.hashPrefix[:], raw)
	return id, nil
}

// MatchesHash reports whether documentHash is the hash this selector was
// derived from. A false result means the document changed since the selector
// was issued.
func (id ID) MatchesHash(documentHash string) bool {
	raw, err := hex.DecodeString(documentHash)
	if err != nil || len(raw) != sha256.Size {
		return false
	}
	return bytes.Equal(raw[:hashPrefixLen], id.hashPrefix[:])
}

// String is the selector: Prefix and the base32 of the review UUID, the index
// as an unsigned varint, and the hash prefix.
func (id ID) String() string {
	raw := make([]byte, 0, len(id.ReviewUUID)+binary.MaxVarintLen64+hashPrefixLen)
	raw = append(raw, id.ReviewUUID[:]...)
	raw = binary.AppendUvarint(raw, uint64(id.Index))
	raw = append(raw, id.hashPrefix[:]...)
	return Prefix + encoding.EncodeToString(raw)
}

// Parse decodes a complete selector. Capitals are accepted and read as lower
// case. Anything else that is not a selector String could have produced is
// ErrInvalid, including a second spelling of a valid one.
func Parse(selector string) (ID, error) {
	body, ok := strings.CutPrefix(strings.ToLower(selector), Prefix)
	if !ok || body == "" {
		return ID{}, fmt.Errorf("%w: it must start with %q", ErrInvalid, Prefix)
	}
	raw, err := encoding.DecodeString(body)
	if err != nil {
		return ID{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	var id ID
	if len(raw) < len(id.ReviewUUID) {
		return ID{}, fmt.Errorf("%w: it is too short", ErrInvalid)
	}
	copy(id.ReviewUUID[:], raw)
	if id.ReviewUUID == uuid.Nil() {
		return ID{}, fmt.Errorf("%w: the review UUID is empty", ErrInvalid)
	}
	raw = raw[len(id.ReviewUUID):]
	index, n := binary.Uvarint(raw)
	if n <= 0 || index > math.MaxInt {
		return ID{}, fmt.Errorf("%w: the finding index is malformed", ErrInvalid)
	}
	id.Index = int(index)
	if len(raw)-n != hashPrefixLen {
		return ID{}, fmt.Errorf("%w: the document hash is malformed", ErrInvalid)
	}
	copy(id.hashPrefix[:], raw[n:])
	// Reject a second spelling of a valid selector: a base32 tail with spare
	// bits set, or an index written with redundant varint bytes.
	if id.String() != Prefix+body {
		return ID{}, fmt.Errorf("%w: it is not spelled canonically", ErrInvalid)
	}
	return id, nil
}

// DocumentHash returns the hex SHA-256 of a review document's canonical form.
// The document is decoded first, which trims and lower-cases the fields it
// normalizes, and then encoded with sorted keys and no insignificant space, so
// two serializations of one review hash alike. A document that
// structuredreview.Decode rejects has no hash.
func DocumentHash(raw jsontext.Value) (string, error) {
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return "", err
	}
	return HashDocument(doc)
}

// HashDocument is DocumentHash for a document that is already decoded.
func HashDocument(doc structuredreview.Document) (string, error) {
	canonical, err := canonicalJSON(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON is the encoding that is hashed. It is a stored contract: every
// selector derives from this hash, so a change to how structuredreview.Document
// encodes (a new field without omitempty, a renamed tag, a different order)
// changes every hash and re-keys every selector ever handed out. The golden test
// in this package fails when it does.
func canonicalJSON(doc structuredreview.Document) ([]byte, error) {
	canonical, err := json.Marshal(doc, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("encode canonical review document: %w", err)
	}
	return canonical, nil
}
