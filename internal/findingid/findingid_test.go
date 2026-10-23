package findingid

import (
	"encoding/json/jsontext"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/structuredreview"
)

const (
	hashA = "3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835420b6b9942dd4f1b"
	hashB = "4a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835420b6b9942dd4f1b"
)

func reviewUUID(t testing.TB) uuid.UUID {
	t.Helper()
	return uuid.MustParse("0b9f1c2e-4d5a-4e6f-8a7b-1c2d3e4f5a6b")
}

func TestSelectorIsDeterministicAndOpaque(t *testing.T) {
	id, err := New(reviewUUID(t), 3, hashA)
	require.NoError(t, err)
	again, err := New(reviewUUID(t), 3, hashA)
	require.NoError(t, err)

	assert := assert.New(t)
	assert.Equal(id.String(), again.String())
	assert.True(strings.HasPrefix(id.String(), "f:"))
	assert.Equal(id.String(), strings.ToLower(id.String()))
	assert.NotContains(id.String(), reviewUUID(t).String(), "the review UUID is encoded, not shown")

	other, err := New(reviewUUID(t), 4, hashA)
	require.NoError(t, err)
	assert.NotEqual(id.String(), other.String(), "each position in a review has its own selector")
	changed, err := New(reviewUUID(t), 3, hashB)
	require.NoError(t, err)
	assert.NotEqual(id.String(), changed.String(), "a changed document changes the selector")
}

func TestParseRecoversTheParts(t *testing.T) {
	want, err := New(reviewUUID(t), 130, hashA)
	require.NoError(t, err, "an index above 127 needs a longer encoding")
	got, err := Parse(want.String())
	require.NoError(t, err)

	assert.Equal(t, want, got)
	assert.Equal(t, reviewUUID(t), got.ReviewUUID)
	assert.Equal(t, 130, got.Index)
	assert.True(t, got.MatchesHash(hashA))
	assert.False(t, got.MatchesHash(hashB), "a stale document hash does not match")
	assert.False(t, got.MatchesHash("not hex"))
}

func TestNewRejectsWhatItCannotEncode(t *testing.T) {
	for name, tc := range map[string]struct {
		id    uuid.UUID
		index int
		hash  string
	}{
		"negative index": {reviewUUID(t), -1, hashA},
		"nil uuid":       {uuid.Nil(), 0, hashA},
		"short hash":     {reviewUUID(t), 0, "3a7b"},
		"not hex":        {reviewUUID(t), 0, strings.Repeat("z", 64)},
		"empty hash":     {reviewUUID(t), 0, ""},
	} {
		_, err := New(tc.id, tc.index, tc.hash)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestParseRejectsMalformedSelectors(t *testing.T) {
	good, err := New(reviewUUID(t), 0, hashA)
	require.NoError(t, err)
	body := strings.TrimPrefix(good.String(), "f:")

	for name, in := range map[string]string{
		"empty":            "",
		"no prefix":        body,
		"wrong prefix":     "g:" + body,
		"bare prefix":      "f:",
		"truncated":        good.String()[:len(good.String())-3],
		"trailing data":    good.String() + "aa",
		"bad alphabet":     "f:" + strings.Repeat("1", len(body)),
		"padding":          good.String() + "=",
		"too long":         "f:" + strings.Repeat("a", 60),
		"whitespace":       " " + good.String(),
		"embedded newline": good.String() + "\n",
	} {
		_, err := Parse(in)
		require.ErrorIs(t, err, ErrInvalid, "%s: %q", name, in)
	}
}

func TestParseRejectsASecondSpellingOfOneSelector(t *testing.T) {
	// Index 130 encodes in 26 bytes, which leaves two spare bits in the last
	// character. Setting them must not produce another valid spelling.
	good, err := New(reviewUUID(t), 130, hashA)
	require.NoError(t, err)
	last := good.String()[len(good.String())-1]
	i := strings.IndexByte(alphabet, last)
	require.GreaterOrEqual(t, i, 0)
	twin := good.String()[:len(good.String())-1] + string(alphabet[i|1])
	require.NotEqual(t, good.String(), twin)

	_, err = Parse(twin)
	require.ErrorIs(t, err, ErrInvalid)
}

// Found by FuzzParseIsCanonical: the varint decoder accepts an index written
// with redundant bytes, which String would print differently.
func TestParseRejectsAnIndexWrittenWithRedundantBytes(t *testing.T) {
	const shrunk = "f:222222222222222222222222222AA222222222222A"
	_, err := Parse(shrunk)
	require.ErrorIs(t, err, ErrInvalid, "the shrunk fuzz counterexample")

	hash := make([]byte, hashPrefixLen)
	id := reviewUUID(t)
	padded := Prefix + encoding.EncodeToString(slices.Concat(id[:], []byte{0x80, 0x00}, hash))
	_, err = Parse(padded)
	require.ErrorIs(t, err, ErrInvalid, "index 0 spelled as 0x80 0x00")
}

func TestParseRejectsPartsNewWouldNotAccept(t *testing.T) {
	hash := make([]byte, hashPrefixLen)
	raw := func(parts ...[]byte) string {
		return Prefix + encoding.EncodeToString(slices.Concat(parts...))
	}
	id := reviewUUID(t)
	overflow := slices.Repeat([]byte{0xff}, 10)

	for name, in := range map[string]string{
		"nil uuid":        raw(make([]byte, 16), []byte{0}, hash),
		"index overflows": raw(id[:], append(overflow, 0x7f), hash),
		"index runs off":  raw(id[:], []byte{0x80, 0x80}),
		"short hash":      raw(id[:], []byte{1}, hash[:hashPrefixLen-1]),
		"long hash":       raw(id[:], []byte{1}, hash, []byte{0}),
		"uuid only":       raw(id[:]),
		"index above int": raw(id[:], append(slices.Repeat([]byte{0xff}, 9), 0x01), hash),
	} {
		_, err := Parse(in)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestParseAcceptsUppercase(t *testing.T) {
	good, err := New(reviewUUID(t), 2, hashA)
	require.NoError(t, err)
	got, err := Parse(strings.ToUpper(good.String()))
	require.NoError(t, err, "a selector read aloud or retyped in capitals still resolves")
	assert.Equal(t, good, got)
}

// A selector must survive encode and parse for every position and hash, and
// anything Parse accepts must be the one canonical spelling of its parts.
func FuzzSelectorRoundTrip(f *testing.F) {
	f.Add([]byte("0123456789abcdef"), 0, []byte("0123456789abcdef0123456789abcdef"))
	f.Add([]byte("fedcba9876543210"), 127, []byte("fedcba9876543210fedcba9876543210"))
	f.Add([]byte("fedcba9876543210"), 128, []byte("fedcba9876543210fedcba9876543210"))
	f.Add([]byte("fedcba9876543210"), 1<<20, []byte("fedcba9876543210fedcba9876543210"))
	f.Fuzz(func(t *testing.T, rawUUID []byte, index int, rawHash []byte) {
		if len(rawUUID) < 16 || len(rawHash) < 32 || index < 0 {
			t.Skip()
		}
		id := uuid.UUID(rawUUID[:16])
		if id == uuid.Nil() {
			t.Skip()
		}
		hash := fmt.Sprintf("%x", rawHash[:32])
		selector, err := New(id, index, hash)
		require.NoError(t, err)

		parsed, err := Parse(selector.String())
		require.NoError(t, err)
		assert.Equal(t, selector, parsed)
		assert.Equal(t, id, parsed.ReviewUUID)
		assert.Equal(t, index, parsed.Index)
		assert.True(t, parsed.MatchesHash(hash))
		assert.Equal(t, selector.String(), parsed.String(), "the canonical spelling is stable")
	})
}

// Whatever Parse accepts must print back as the same selector, so two spellings
// of one finding cannot both be stored.
func FuzzParseIsCanonical(f *testing.F) {
	good, err := New(reviewUUID(f), 5, hashA)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good.String())
	f.Add(strings.ToUpper(good.String()))
	f.Add("f:")
	f.Add("f:aaaa")
	f.Fuzz(func(t *testing.T, in string) {
		id, err := Parse(in)
		if err != nil {
			require.ErrorIs(t, err, ErrInvalid)
			return
		}
		assert.Equal(t, strings.ToLower(in), id.String())
	})
}

const baseDocument = `{
  "schema_version": 2,
  "summary": "Two problems.",
  "verdict": "fail",
  "findings": [
    {"severity": "high", "problem": "Nil dereference.", "fix": "Check first.", "location": "a.go:10"},
    {"severity": "low", "problem": "Typo.", "fix": "Spell it.", "location": null}
  ]
}`

func TestDocumentHashIgnoresFormatting(t *testing.T) {
	want, err := DocumentHash(jsontext.Value(baseDocument))
	require.NoError(t, err)
	require.Regexp(t, `^[0-9a-f]{64}$`, want)

	for name, doc := range map[string]string{
		"compact": `{"schema_version":2,"summary":"Two problems.","verdict":"fail","findings":[{"severity":"high","problem":"Nil dereference.","fix":"Check first.","location":"a.go:10"},{"severity":"low","problem":"Typo.","fix":"Spell it.","location":null}]}`,
		"keys reordered": `{"findings":[{"location":"a.go:10","fix":"Check first.","problem":"Nil dereference.","severity":"high"},` +
			`{"location":null,"fix":"Spell it.","problem":"Typo.","severity":"low"}],"verdict":"fail","summary":"Two problems.","schema_version":2}`,
		"normalized text":       strings.NewReplacer(`"high"`, `" HIGH "`, `"fail"`, `"Fail"`, `"Typo."`, `"  Typo.\n"`).Replace(baseDocument),
		"empty sources omitted": strings.Replace(baseDocument, `"location": null}`, `"location": null, "sources": []}`, 1),
	} {
		got, err := DocumentHash(jsontext.Value(doc))
		require.NoError(t, err, name)
		assert.Equal(t, want, got, name)
	}
}

func TestDocumentHashSeparatesDifferentDocuments(t *testing.T) {
	want, err := DocumentHash(jsontext.Value(baseDocument))
	require.NoError(t, err)
	ordered, err := structuredreview.Decode(jsontext.Value(baseDocument))
	require.NoError(t, err)
	reordered := ordered
	reordered.Findings = slices.Clone(ordered.Findings)
	slices.Reverse(reordered.Findings)
	reorderedHash, err := HashDocument(reordered)
	require.NoError(t, err)
	assert.NotEqual(t, want, reorderedHash, "reversing the same complete findings changes the hash")
	for name, doc := range map[string]string{
		"finding text":     strings.Replace(baseDocument, "Typo.", "Typos.", 1),
		"location":         strings.Replace(baseDocument, `"location": null`, `"location": "b.go"`, 1),
		"summary":          strings.Replace(baseDocument, "Two problems.", "Two issues.", 1),
		"severity":         strings.Replace(baseDocument, `"low"`, `"medium"`, 1),
		"sourced document": strings.Replace(baseDocument, `"location": null}`, `"location": null, "sources": [1]}`, 1),
		"source labels":    strings.Replace(baseDocument, `"verdict": "fail",`, `"verdict": "fail", "source_labels": ["bug"],`, 1),
	} {
		got, err := DocumentHash(jsontext.Value(doc))
		require.NoError(t, err, name)
		assert.NotEqual(t, want, got, name)
	}
}

func TestDocumentHashRejectsWhatIsNotAReviewDocument(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":           "",
		"not json":        "## Summary",
		"unknown member":  strings.Replace(baseDocument, `"summary"`, `"surprise": 1, "summary"`, 1),
		"trailing":        baseDocument + "{}",
		"wrong version":   strings.Replace(baseDocument, `"schema_version": 2`, `"schema_version": 9`, 1),
		"missing summary": strings.Replace(baseDocument, `"summary": "Two problems.",`, "", 1),
	} {
		_, err := DocumentHash(jsontext.Value(doc))
		require.Error(t, err, name)
	}
}

// shuffledDocument renders the base document with its object members in a
// random order and random insignificant whitespace. Every rendering is the same
// review, so every rendering must hash the same.
func shuffledDocument(r *rand.Rand) string {
	gap := func() string { return strings.Repeat(" ", r.IntN(3)) + []string{"", "\n", "\t"}[r.IntN(3)] }
	object := func(members []string) string {
		r.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
		return "{" + gap() + strings.Join(members, gap()+","+gap()) + gap() + "}"
	}
	finding := func(severity, problem, fix, location string) string {
		return object([]string{
			`"severity":` + gap() + severity, `"problem":` + gap() + problem,
			`"fix":` + gap() + fix, `"location":` + gap() + location,
		})
	}
	findings := "[" + gap() + finding(`"high"`, `"Nil dereference."`, `"Check first."`, `"a.go:10"`) + gap() + "," + gap() +
		finding(`"low"`, `"Typo."`, `"Spell it."`, `null`) + gap() + "]"
	return object([]string{
		`"schema_version":` + gap() + `2`, `"summary":` + gap() + `"Two problems."`,
		`"verdict":` + gap() + `"fail"`, `"findings":` + gap() + findings,
	})
}

func FuzzDocumentHashIgnoresMemberOrderAndSpacing(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Add(uint64(0), uint64(0))
	f.Add(uint64(7), uint64(99))
	want, err := DocumentHash(jsontext.Value(baseDocument))
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, seedA, seedB uint64) {
		doc := shuffledDocument(rand.New(rand.NewPCG(seedA, seedB)))
		got, err := DocumentHash(jsontext.Value(doc))
		require.NoError(t, err, doc)
		assert.Equal(t, want, got, doc)
	})
}

// goldenDocument is a fixed review exercising every part of the encoding:
// a null location, a finding that cites several reviews, one that cites one,
// one that cites none, and source labels.
func goldenDocument() structuredreview.Document {
	return structuredreview.Document{
		SchemaVersion: structuredreview.SchemaVersion,
		Summary:       "Three problems.",
		Verdict:       structuredreview.VerdictFail,
		Findings: []structuredreview.Finding{
			{Severity: "high", Problem: "Nil dereference.", Fix: "Check first.", Location: "a.go:10", Sources: []int{1, 2}},
			{Severity: "low", Problem: "Typo.", Fix: "Spell it."},
			{Severity: "medium", Problem: "Missing \"quote\" & <tag>.", Fix: "Escape it.", Location: "b/c.go:3-9", Sources: []int{2}},
		},
		SourceLabels: []string{"bug", "security"},
	}
}

// The selectors a review's findings were given are stored elsewhere, so they
// must never change for the same review. This pins the canonical encoding, its
// hash, and the selector derived from them. If it fails, the encoding of
// structuredreview.Document changed, and every selector already handed out
// would name nothing. That is a migration to design, not a golden to update.
func TestSelectorsAreAStoredContract(t *testing.T) {
	const (
		wantCanonical = `{"schema_version":2,"summary":"Three problems.","verdict":"fail","findings":[` +
			`{"severity":"high","problem":"Nil dereference.","fix":"Check first.","location":"a.go:10","sources":[1,2]},` +
			`{"severity":"low","problem":"Typo.","fix":"Spell it.","location":null},` +
			`{"severity":"medium","problem":"Missing \"quote\" & <tag>.","fix":"Escape it.","location":"b/c.go:3-9","sources":[2]}],` +
			`"source_labels":["bug","security"]}`
		wantHash     = "f46e59d4489e70b9d29a2e5ff8dbf01d40c82d6300078e863f31357bb182c9af"
		wantSelector = "f:boprylsnljhg7ct3dqwt4t22nmbpi3sz2rej44fz"
	)
	doc := goldenDocument()

	canonical, err := canonicalJSON(doc)
	require.NoError(t, err)
	// The order of members is part of the contract, which JSONEq would ignore.
	assert.Equal(t, wantCanonical, string(canonical)) //nolint:testifylint // exact bytes, not JSON equivalence

	hash, err := HashDocument(doc)
	require.NoError(t, err)
	assert.Equal(t, wantHash, hash)

	id, err := New(uuid.MustParse("0b9f1c2e-4d5a-4e6f-8a7b-1c2d3e4f5a6b"), 2, hash)
	require.NoError(t, err)
	assert.Equal(t, wantSelector, id.String())

	// The same review as an agent or an export would write it.
	fromWire, err := DocumentHash(jsontext.Value(`{
	  "findings": [
	    {"location": "a.go:10", "fix": "Check first.", "sources": [1, 2], "problem": "Nil dereference.", "severity": "High"},
	    {"location": null, "fix": "Spell it.", "problem": " Typo. ", "severity": "low"},
	    {"sources": [2], "location": "b/c.go:3-9", "fix": "Escape it.", "problem": "Missing \"quote\" & <tag>.", "severity": "medium"}
	  ],
	  "source_labels": ["bug", "security"], "verdict": "Fail", "summary": "Three problems.", "schema_version": 2
	}`))
	require.NoError(t, err)
	assert.Equal(t, wantHash, fromWire)
}
