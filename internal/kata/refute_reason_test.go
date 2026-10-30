package kata

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefuteReasonCodec(t *testing.T) {
	evidence := "  " + strings.Repeat("界", 40) + "\n"
	for _, reason := range []string{"false-positive", "spurious", "risk-accepted"} {
		t.Run(reason, func(t *testing.T) {
			body, err := EncodeRefuteReason(CommentRefute, evidence, &reason)
			require.NoError(t, err)
			assert.Equal(t, "<!-- roborev-refute:v1 reason="+reason+" -->\n"+evidence, body)
			view := DecodeCommentBody(CommentRefute, body)
			assert.Equal(t, CommentBody{Reason: reason, ReasonState: "explicit", Evidence: evidence, EvidenceValid: true}, view)
			require.NoError(t, ValidateEvidence(CommentRefute, body))
		})
	}
	body, err := EncodeRefuteReason(CommentRefute, evidence, nil)
	require.NoError(t, err)
	assert.Equal(t, evidence, body)
	assert.Equal(t, CommentBody{Reason: "unspecified", ReasonState: "absent", Evidence: evidence, EvidenceValid: true}, DecodeCommentBody(CommentRefute, body))
	for _, reason := range []string{"", "unknown", "unspecified", "Spurious"} {
		_, err := EncodeRefuteReason(CommentRefute, evidence, &reason)
		require.Error(t, err)
	}
	for _, kind := range []CommentKind{CommentReply, CommentConfirm, CommentSupersede, ""} {
		reason := "spurious"
		_, err := EncodeRefuteReason(kind, evidence, &reason)
		require.Error(t, err)
	}
}

func TestRefuteReasonInvalidAndInsufficientEvidence(t *testing.T) {
	marker := "<!-- roborev-refute:v1 reason=spurious -->\n"
	for _, body := range []string{
		"<!-- roborev-refute:v2 reason=spurious -->\n", "<!-- roborev-refute:v1 reason=unknown -->\n",
		"<!-- roborev-refute:v1 reason=spurious -->\r\n", "<!-- roborev-refute:", marker + marker + strings.Repeat("界", 40),
	} {
		view := DecodeCommentBody(CommentRefute, body)
		assert.Equal(t, "invalid", view.ReasonState)
		assert.Equal(t, "unspecified", view.Reason)
		assert.False(t, view.EvidenceValid)
		require.Error(t, ValidateEvidence(CommentRefute, body))
	}
	for _, body := range []string{marker, marker + strings.Repeat("界", 39), marker + strings.Repeat(" \t", 40)} {
		view := DecodeCommentBody(CommentRefute, body)
		assert.Equal(t, "explicit", view.ReasonState)
		assert.False(t, view.EvidenceValid)
		require.ErrorIs(t, ValidateEvidence(CommentRefute, body), ErrEvidenceTooShort)
	}
	for _, kind := range []CommentKind{CommentReply, CommentConfirm, CommentSupersede, ""} {
		view := DecodeCommentBody(kind, marker+strings.Repeat("界", 40))
		assert.Equal(t, "invalid", view.ReasonState)
		assert.False(t, view.EvidenceValid)
		require.Error(t, ValidateEvidence(kind, marker+strings.Repeat("界", 40)))
	}
	_, err := EncodeRefuteReason(CommentRefute, marker+strings.Repeat("界", 40), nil)
	require.Error(t, err, "caller evidence cannot supply the reserved opening")
	quoted := "Quoted evidence:\n" + marker + strings.Repeat("界", 40)
	assert.Equal(t, "absent", DecodeCommentBody(CommentRefute, quoted).ReasonState)
	assert.Equal(t, "absent", DecodeCommentBody(CommentRefute, " "+marker).ReasonState)
	assert.False(t, DecodeCommentBody(CommentSupersede, " \t\n").EvidenceValid)
}

func FuzzRefuteReasonCodec(f *testing.F) {
	f.Add("Evidence with whitespace.\n", uint8(0))
	f.Add("<!-- roborev-refute:v2 reason=unknown -->\n", uint8(2))
	f.Add(strings.Repeat("界", 40), uint8(1))
	f.Add("short\n\n[roborev:0123456789abcdef01234567]", uint8(1))
	f.Fuzz(func(t *testing.T, evidence string, choice uint8) {
		if !utf8.ValidString(evidence) {
			t.Skip()
		}
		reason := []string{"false-positive", "spurious", "risk-accepted"}[choice%3]
		encoded, err := EncodeRefuteReason(CommentRefute, evidence, &reason)
		if strings.HasPrefix(evidence, "<!-- roborev-refute:") {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		assert.Equal(t, "<!-- roborev-refute:v1 reason="+reason+" -->\n"+evidence, encoded)
		view := DecodeCommentBody(CommentRefute, encoded)
		// Recovery metadata is excluded from the evidence view; the raw encoding
		// above still preserves every input byte. Its qualification has fixed cases.
		if !strings.Contains(evidence, "[roborev:") {
			assert.Equal(t, evidence, view.Evidence)
			assert.Equal(t, EvidenceRunes(evidence) >= 40, view.EvidenceValid)
		}
		assert.Equal(t, reason, view.Reason)
		assert.Equal(t, "explicit", view.ReasonState)
		assert.Equal(t, "invalid", DecodeCommentBody(CommentConfirm, encoded).ReasonState)
		unknownVersion := strings.Replace(encoded, "roborev-refute:v1", "roborev-refute:v2", 1)
		assert.Equal(t, "invalid", DecodeCommentBody(CommentRefute, unknownVersion).ReasonState)
		assert.Equal(t, "invalid", DecodeCommentBody(CommentRefute, "<!-- roborev-refute:v1 reason=spurious -->\n"+encoded).ReasonState)
	})
}

func TestRefuteReasonRecoveryMetadataIsNotEvidence(t *testing.T) {
	const recovery = "\n\n[roborev:0123456789abcdef01234567]"
	for _, kind := range []CommentKind{CommentConfirm, CommentRefute} {
		for _, evidence := range []string{"", "short", strings.Repeat("界", 39), strings.Repeat("界", 40), "  " + strings.Repeat("界", 40) + "\n"} {
			prefixes := []string{""}
			if kind == CommentRefute {
				prefixes = append(prefixes, "<!-- roborev-refute:v1 reason=spurious -->\n")
			}
			for _, prefix := range prefixes {
				raw := prefix + evidence + recovery
				view := DecodeCommentBody(kind, raw)
				assert.Equal(t, evidence, view.Evidence)
				assert.Equal(t, EvidenceRunes(evidence) >= 40, view.EvidenceValid)
				if EvidenceRunes(evidence) < 40 {
					require.ErrorIs(t, ValidateEvidence(kind, raw), ErrEvidenceTooShort)
				} else {
					require.NoError(t, ValidateEvidence(kind, raw))
				}
			}
		}
	}
	// Repeated terminal metadata also cannot turn an empty body into evidence.
	view := DecodeCommentBody(CommentRefute, "<!-- roborev-refute:v1 reason=spurious -->\n"+recovery+recovery)
	assert.Empty(t, view.Evidence)
	assert.False(t, view.EvidenceValid)
	// A quoted marker remains ordinary prose, not an appended recovery frame.
	prose := "The literal [roborev:0123456789abcdef01234567] occurs inside this explanation."
	assert.Equal(t, prose, DecodeCommentBody(CommentRefute, prose).Evidence)
}

func TestRefuteReasonRecoveryEditsPreserveEvidenceWhitespace(t *testing.T) {
	const token = "[roborev:0123456789abcdef01234567]"
	const prefix = "<!-- roborev-refute:v1 reason=spurious -->\n"
	for _, kind := range []CommentKind{CommentConfirm, CommentRefute} {
		header := ""
		if kind == CommentRefute {
			header = prefix
		}
		for _, separator := range []string{" ", "\r\n\r\n"} {
			for _, tail := range []string{"", "\n\t"} {
				body := header + "  short" + separator + token + tail
				view := DecodeCommentBody(kind, body)
				assert.Equal(t, "  short"+separator+tail, view.Evidence)
				assert.False(t, view.EvidenceValid)
				require.ErrorIs(t, ValidateEvidence(kind, body), ErrEvidenceTooShort)
			}
		}
	}
}

func TestRefuteReasonRecoveryBeforeAppendedEvidence(t *testing.T) {
	const token = "[roborev:0123456789abcdef01234567]"
	for _, kind := range []CommentKind{CommentConfirm, CommentRefute} {
		prefixes := []string{""}
		if kind == CommentRefute {
			for _, reason := range []string{"false-positive", "spurious", "risk-accepted"} {
				prefixes = append(prefixes, "<!-- roborev-refute:v1 reason="+reason+" -->\n")
			}
		}
		for _, prefix := range prefixes {
			for _, after := range []string{"more", strings.Repeat("界", 33), strings.Repeat("界", 34)} {
				want := "short\n" + after
				view := DecodeCommentBody(kind, prefix+"short\n\n"+token+"\n"+after)
				assert.Equal(t, want, view.Evidence)
				assert.Equal(t, EvidenceRunes(want) >= 40, view.EvidenceValid)
			}
		}
	}
	quoted := "The literal " + token + " occurs in this explanation."
	assert.Equal(t, quoted, DecodeCommentBody(CommentRefute, quoted).Evidence)
}

func FuzzCommentRecoveryFrameEvidence(f *testing.F) {
	f.Add("short", "more", true)
	f.Add(strings.Repeat("界", 19), strings.Repeat("界", 19), false)
	f.Add(strings.Repeat("界", 19), strings.Repeat("界", 20), true)
	f.Fuzz(func(t *testing.T, before, after string, marked bool) {
		if !utf8.ValidString(before) || !utf8.ValidString(after) || strings.Contains(before, "[roborev:") || strings.Contains(after, "[roborev:") || strings.HasPrefix(before, "<!-- roborev-refute:") {
			t.Skip()
		}
		body := FrameCommentBody(before, CommentRecoveryMarker("0123456789abcdef01234567")) + "\n" + after
		kind := CommentConfirm
		if marked {
			kind = CommentRefute
			body = "<!-- roborev-refute:v1 reason=spurious -->\n" + body
		}
		view := DecodeCommentBody(kind, body)
		want := before + "\n" + after
		assert.Equal(t, want, view.Evidence)
		assert.Equal(t, EvidenceRunes(want) >= 40, view.EvidenceValid)
	})
}
