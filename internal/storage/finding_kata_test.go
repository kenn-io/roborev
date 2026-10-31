package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func kataFixture(t *testing.T) (*DB, *FindingStats, FindingKataReview) {
	t.Helper()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/outcome-repo")
	findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{{Severity: "high", Problem: "optional operation can fail", Fix: "retry"}})
	stats, err := db.GetFindingStats(SummaryOptions{}, "severity")
	require.NoError(t, err)
	sources := stats.KataSources()
	require.Len(t, sources, 1)
	return db, stats, FindingKataReview{Source: sources[0], Complete: true, ReadAt: time.Now(), Findings: []FindingKataFinding{{Index: 0, RootUID: "root-a", VersionValid: true, Replies: []FindingKataReply{kataReply("reply-a", "refute", "risk-accepted")}}}}
}

func kataReply(uid, kind, reason string) FindingKataReply {
	state := "explicit"
	if kind != "refute" {
		state = "absent"
	}
	return FindingKataReply{UID: uid, TargetUID: "root-a", Author: "reviewer", Teammate: "worker", Kind: kind, CreatedAt: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), Reason: reason, ReasonState: state, Evidence: strings.Repeat("界", 40), EvidenceValid: true}
}

func TestFindingKataReasons(t *testing.T) {
	for _, reason := range []string{"false-positive", "spurious", "risk-accepted", "unspecified"} {
		t.Run(reason, func(t *testing.T) {
			_, stats, input := kataFixture(t)
			input.Findings[0].Replies[0].Reason = reason
			if reason == "unspecified" {
				input.Findings[0].Replies[0].ReasonState = "absent"
			}
			stats.ApplyKata([]FindingKataReview{input})
			a := assert.New(t)
			a.Equal(1, stats.Total.Negative)
			want := RefuteReasonCounts{}
			switch reason {
			case "false-positive":
				want.FalsePositive = 1
			case "spurious":
				want.Spurious = 1
			case "risk-accepted":
				want.RiskAccepted = 1
			default:
				want.Unspecified = 1
				want.Absent = 1
			}
			a.Equal(want, stats.Total.RefuteReasons)
			a.Equal(1, stats.Total.RefuteReasons.Total())
			a.Equal(reason, stats.Evidence[0].Reason)
			a.Equal("K", stats.Evidence[0].Tier)
			a.Equal("reviewer", stats.Evidence[0].KataReplies[0].Author)
			a.Equal(1, stats.Kata.Labeled)
			a.Equal(1, stats.TierCoverage["K"].Negative)
			a.Equal(stats.Total.RefuteReasons, stats.Rows[0].RefuteReasons)
		})
	}
}

func TestFindingKataCurrentAssertions(t *testing.T) {
	for _, tc := range []struct {
		name               string
		change             func(*FindingKataReview)
		outcome, state     string
		invalid, conflicts int
	}{
		{"39 unicode evidence", func(r *FindingKataReview) { r.Findings[0].Replies[0].Evidence = strings.Repeat("界", 39) }, "unlabeled", "invalid", 1, 0},
		{"marker only invalid", func(r *FindingKataReview) {
			r.Findings[0].Replies[0].Evidence = ""
			r.Findings[0].Replies[0].EvidenceValid = false
		}, "unlabeled", "invalid", 1, 0},
		{"edited newest invalid", func(r *FindingKataReview) {
			n := kataReply("reply-z", "refute", "spurious")
			n.Evidence = "short"
			n.Edited = true
			r.Findings[0].Replies = append(r.Findings[0].Replies, n)
		}, "unlabeled", "invalid", 1, 0},
		{"nested confirmation ignored", func(r *FindingKataReview) {
			n := kataReply("nested", "confirm", "")
			n.TargetUID = "reply-a"
			r.Findings[0].Replies = append(r.Findings[0].Replies, n)
		}, "refuted", "explicit", 0, 0},
		{"ordinary reply ignored", func(r *FindingKataReview) {
			n := kataReply("reply-z", "reply", "")
			r.Findings[0].Replies = append(r.Findings[0].Replies, n)
		}, "refuted", "explicit", 0, 0},
		{"mixed kinds", func(r *FindingKataReview) {
			n := kataReply("reply-b", "confirm", "")
			n.Author = "another"
			r.Findings[0].Replies = append(r.Findings[0].Replies, n)
		}, "unlabeled", "conflicting", 0, 1},
		{"mixed reasons", func(r *FindingKataReview) {
			n := kataReply("reply-b", "refute", "spurious")
			n.Author = "another"
			r.Findings[0].Replies = append(r.Findings[0].Replies, n)
		}, "refuted", "conflicting", 0, 1},
		{"missing reason", func(r *FindingKataReview) {
			n := kataReply("reply-b", "refute", "unspecified")
			n.Author = "another"
			n.ReasonState = "absent"
			r.Findings[0].Replies = append(r.Findings[0].Replies, n)
		}, "refuted", "conflicting", 0, 1},
		{"invalid reason", func(r *FindingKataReview) { r.Findings[0].Replies[0].ReasonState = "invalid" }, "unlabeled", "invalid", 1, 0},
		{"supersede", func(r *FindingKataReview) {
			r.Findings[0].Replies[0].Kind = "supersede"
			r.Findings[0].Replies[0].Reason = ""
			r.Findings[0].Replies[0].ReasonState = "absent"
			r.Findings[0].Replies[0].Evidence = "replaced"
		}, "superseded", "absent", 0, 0},
		{"duplicate replay", func(r *FindingKataReview) {
			r.Findings[0].Replies = append(r.Findings[0].Replies, r.Findings[0].Replies[0])
		}, "refuted", "explicit", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stats, input := kataFixture(t)
			tc.change(&input)
			stats.ApplyKata([]FindingKataReview{input})
			a := assert.New(t)
			a.Equal(tc.outcome, stats.Evidence[0].Outcome)
			a.Equal(tc.state, stats.Evidence[0].ReasonState)
			a.Equal(tc.invalid, stats.Kata.Invalid)
			a.Equal(tc.conflicts, stats.Kata.Conflicting)
			a.Equal(tc.invalid, stats.Total.RefuteReasons.Invalid)
			a.Equal(tc.conflicts, stats.Total.RefuteReasons.Conflicting)
			require.Len(t, stats.Rows, 1)
			a.Equal(stats.Total.RefuteReasons, stats.Rows[0].RefuteReasons)
			a.Equal(stats.Total.RefuteReasons, stats.TierCoverage["K"].RefuteReasons)
			a.Equal(stats.Total.Negative, stats.Total.RefuteReasons.Total())
		})
	}
}

func TestFindingKataFallback(t *testing.T) {
	for _, mode := range []string{"unavailable", "incomplete", "stale", "unmapped", "no assertions"} {
		t.Run(mode, func(t *testing.T) {
			db, stats, input := kataFixture(t)
			parent := stats.Evidence[0]
			statsLabel(t, db, parent.JobID, parent.ReviewUUID, "optional operation can fail", "refuted", "2026-10-20 12:00:00")
			var err error
			stats, err = db.GetFindingStats(SummaryOptions{}, "severity")
			require.NoError(t, err)
			switch mode {
			case "unavailable":
				input.Unavailable = true
			case "incomplete":
				input.Complete = false
			case "stale":
				input.Findings[0].VersionValid = false
			case "unmapped":
				input.Findings = nil
			case "no assertions":
				input.Findings[0].Replies = nil
			}
			stats.ApplyKata([]FindingKataReview{input})
			a := assert.New(t)
			a.Equal(1, stats.Total.Negative)
			a.Equal(1, stats.Total.RefuteReasons.Unspecified)
			a.Equal("R", stats.Evidence[0].Tier)
			a.Equal("unspecified", stats.Evidence[0].Reason)
			if mode == "stale" {
				a.Equal("unavailable", stats.Kata.State)
				a.Equal(1, stats.Kata.Unavailable)
			}
		})
	}
}

func FuzzFindingKataPermutation(f *testing.F) {
	f.Add(uint8(0), uint8(1), true)
	f.Add(uint8(31), uint8(255), false)
	f.Add(uint8(64), uint8(128), true)
	f.Fuzz(func(t *testing.T, a, b uint8, reverse bool) {
		reasons := []string{"false-positive", "spurious", "risk-accepted", "unspecified"}
		replies := []FindingKataReply{kataReply("reply-a", "refute", reasons[int(a)%4]), kataReply("reply-b", "refute", reasons[int(b)%4])}
		replies[1].Author = "another"
		for i, value := range []uint8{a, b} {
			replies[i].Kind = []string{"refute", "confirm", "supersede"}[(value>>2)%3]
			replies[i].CreatedAt = replies[i].CreatedAt.Add(time.Duration(value>>4) * time.Hour)
			if replies[i].Reason == "unspecified" || replies[i].Kind != "refute" {
				replies[i].ReasonState = "absent"
			}
			if value&64 != 0 {
				replies[i].Evidence = strings.Repeat("界", 39)
			}
			if value&128 != 0 {
				replies[i].Author = "reviewer"
			}
		}
		left := reduceKataFinding(FindingKataFinding{RootUID: "root-a", VersionValid: true, Replies: replies})
		if reverse {
			replies[0], replies[1] = replies[1], replies[0]
		}
		right := reduceKataFinding(FindingKataFinding{RootUID: "root-a", VersionValid: true, Replies: replies})
		assert.Equal(t, left, right)
		counts := FindingOutcomeCounts{}
		addFindingEvidence(&counts, left)
		assert.Equal(t, counts.Negative, counts.RefuteReasons.Total())
	})
}

func TestFindingKataPreservesDiscussionAndLocalEvidence(t *testing.T) {
	db, stats, input := kataFixture(t)
	e := stats.Evidence[0]
	statsLabel(t, db, e.JobID, e.ReviewUUID, "optional operation can fail", "confirmed", "2026-10-20 12:00:00")
	var err error
	stats, err = db.GetFindingStats(SummaryOptions{}, "severity")
	require.NoError(t, err)
	nested := kataReply("nested", "confirm", "")
	nested.TargetUID = "reply-a"
	input.Findings[0].Replies = append(input.Findings[0].Replies, nested)
	stats.ApplyKata([]FindingKataReview{input})
	a := assert.New(t)
	a.Equal("confirmed", stats.Evidence[0].ROutcome)
	a.Equal("verified evidence", stats.Evidence[0].RExplanation)
	a.Empty(stats.Evidence[0].Explanation)
	a.Equal("refuted", stats.Evidence[0].Outcome)
	a.NotZero(stats.Evidence[0].AssessmentJobID)
	a.Len(stats.Evidence[0].KataDiscussion, 2)
	a.Len(stats.Evidence[0].KataReplies, 1)
	a.Equal(1, stats.TierCoverage["R"].Positive)
	a.Equal(1, stats.TierCoverage["K"].Negative)
	a.Equal(1, stats.Total.RefuteReasons.RiskAccepted)
	a.Equal(2.0, stats.Cost.TotalUSD)
	input.Findings[0].Replies[0].Evidence = "invalid current edit"
	input.Findings[0].Replies[0].Edited = true
	stats.ApplyKata([]FindingKataReview{input})
	a.Equal("unlabeled", stats.Evidence[0].Outcome, "invalid current K cannot fall back to confirmed R")
	a.Equal("confirmed", stats.Evidence[0].ROutcome)
	a.Equal(1, stats.Kata.Invalid)
	a.Equal(1, stats.Kata.Edited)
	a.Equal(1, stats.Total.RefuteReasons.Invalid)
	a.Equal(1, stats.Rows[0].RefuteReasons.Invalid)
	a.Equal(stats.Total.Negative, stats.Total.RefuteReasons.Total())
	a.Equal("verified evidence", stats.Evidence[0].RExplanation)
	input.Findings[0].Replies[0].Evidence = strings.Repeat("界", 40)
	conflict := kataReply("reply-b", "confirm", "")
	conflict.Author = "another"
	input.Findings[0].Replies = append(input.Findings[0].Replies, conflict)
	stats.ApplyKata([]FindingKataReview{input})
	a.Equal("unlabeled", stats.Evidence[0].Outcome)
	a.Equal("confirmed", stats.Evidence[0].ROutcome)
	a.Equal("verified evidence", stats.Evidence[0].RExplanation)
	a.Equal(1, stats.Total.RefuteReasons.Conflicting)
	a.Equal(1, stats.Rows[0].RefuteReasons.Conflicting)
	a.Equal(stats.Total.Negative, stats.Total.RefuteReasons.Total())
	// A later complete read without a current direct assertion retains both
	// local outcome and discussion, while repeated reduction remains idempotent.
	input.Findings[0].Replies = []FindingKataReply{nested}
	stats.ApplyKata([]FindingKataReview{input})
	a.Equal("confirmed", stats.Evidence[0].Outcome)
	a.Equal("R", stats.Evidence[0].Tier)
	a.Len(stats.Evidence[0].KataDiscussion, 1)
	a.Equal(0, stats.Kata.Labeled)
}

func TestFindingKataActorAndCoverage(t *testing.T) {
	_, stats, input := kataFixture(t)
	other := kataReply("reply-b", "confirm", "")
	other.Teammate = "another-worker"
	input.Findings[0].Replies = append(input.Findings[0].Replies, other)
	stats.ApplyKata([]FindingKataReview{input})
	assert.Equal(t, "unlabeled", stats.Evidence[0].Outcome, "different teammates are distinct actual actors")
	assert.Equal(t, 1, stats.Kata.Conflicting)
	// Creation ordering, then full UID, determines the current candidate.
	other.Teammate = "worker"
	other.CreatedAt = other.CreatedAt.Add(-time.Hour)
	input.Findings[0].Replies[1] = other
	stats.ApplyKata([]FindingKataReview{input})
	assert.Equal(t, "refuted", stats.Evidence[0].Outcome)
	assert.Equal(t, 1, stats.Total.RefuteReasons.RiskAccepted)
	input.Findings[0].Replies[1].CreatedAt = other.CreatedAt.Add(2 * time.Hour)
	stats.ApplyKata([]FindingKataReview{input})
	assert.Equal(t, "confirmed", stats.Evidence[0].Outcome)
}
