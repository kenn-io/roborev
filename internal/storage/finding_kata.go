package storage

import (
	"cmp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Kata v0.19.0's typed confirm/refute contract requires 40 normalized
// evidence runes; marker bytes are excluded by the parent codec.
const findingKataMinEvidenceRunes = 40

// RefuteReasonCounts partitions effective negative findings into four reasons.
// Invalid/conflicting state counts also include incoherent unlabeled findings.
type RefuteReasonCounts struct {
	FalsePositive int `json:"false_positive"`
	Spurious      int `json:"spurious"`
	RiskAccepted  int `json:"risk_accepted"`
	Unspecified   int `json:"unspecified"`
	Absent        int `json:"absent"`
	Invalid       int `json:"invalid"`
	Conflicting   int `json:"conflicting"`
}

func (r RefuteReasonCounts) Total() int {
	return r.FalsePositive + r.Spurious + r.RiskAccepted + r.Unspecified
}

// FindingKataSource retains the source version across external I/O. Document is
// opaque: verified root/document hashes are the parent mapper's responsibility.
type FindingKataSource struct {
	JobID      int64
	ReviewUUID string
	Document   string
}

// FindingKataReply is already decoded by the parent codec, never parsed here.
// EvidenceValid attests the parent's normalization/qualification; the consumer
// also rejects evidence shorter than the native minimum without marker bytes.
type FindingKataReply struct {
	UID           string
	TargetUID     string
	Author        string
	Teammate      string
	Kind          string
	CreatedAt     time.Time
	Reason        string
	ReasonState   string
	Evidence      string
	EvidenceValid bool
	Edited        bool
}

// FindingKataFinding carries validated direct-root/version facts from the
// parent mapper. Index alone never establishes identity.
type FindingKataFinding struct {
	Index        int
	RootUID      string
	VersionValid bool
	Replies      []FindingKataReply
}

type FindingKataReview struct {
	Source      FindingKataSource
	Complete    bool
	Unavailable bool
	Stale       bool
	ReadAt      time.Time
	Findings    []FindingKataFinding
}

type FindingKataCoverage struct {
	State           string     `json:"state"`
	Eligible        int        `json:"eligible"`
	Ineligible      int        `json:"ineligible"`
	Mapped          int        `json:"mapped"`
	Readable        int        `json:"readable"`
	Labeled         int        `json:"labeled"`
	Refutes         int        `json:"refutes"`
	ExplicitReasons int        `json:"explicit_reasons"`
	Unmapped        int        `json:"unmapped"`
	Unavailable     int        `json:"unavailable"`
	Incomplete      int        `json:"incomplete"`
	Stale           int        `json:"stale"`
	Invalid         int        `json:"invalid"`
	Conflicting     int        `json:"conflicting"`
	Excluded        int        `json:"excluded"`
	Edited          int        `json:"edited"`
	ReadStartedAt   *time.Time `json:"read_started_at,omitempty"`
	ReadFinishedAt  *time.Time `json:"read_finished_at,omitempty"`
}

func kataEligible(r *findingReview) bool {
	return r.doc != nil && r.failed && r.reviewUUID != "" && r.source != JobSourceCI && r.ciBase == ""
}

func (f *FindingStats) KataSources() []FindingKataSource {
	var sources []FindingKataSource
	for _, r := range f.retained {
		if kataEligible(r) {
			sources = append(sources, FindingKataSource{r.id, r.reviewUUID, r.document.String})
		}
	}
	return sources
}

func (f *FindingStats) initializeKata() {
	f.Kata = FindingKataCoverage{State: "unavailable"}
	for _, r := range f.retained {
		if kataEligible(r) {
			f.Kata.Eligible += len(r.evidence)
		} else {
			f.Kata.Ineligible += len(r.evidence)
		}
	}
	f.Kata.Unavailable = f.Kata.Eligible
}

// ApplyKata is pure aggregation over the retained local snapshot. Callers must
// revalidate source identity after I/O; an absent/unreadable context retains R.
func (f *FindingStats) ApplyKata(inputs []FindingKataReview) {
	f.initializeKata()
	f.UnavailableTiers = []string{"K", "D", "panel R"}
	byUUID := map[string][]FindingKataReview{}
	for _, input := range inputs {
		byUUID[input.Source.ReviewUUID] = append(byUUID[input.Source.ReviewUUID], input)
	}
	for _, r := range f.retained {
		r.evidence = append([]FindingEvidence(nil), r.localEvidence...)
		if !kataEligible(r) {
			continue
		}
		results := byUUID[r.reviewUUID]
		if len(results) != 1 {
			continue
		}
		input := results[0]
		if input.Unavailable {
			continue
		}
		n := len(r.evidence)
		if input.Stale || input.Source != (FindingKataSource{r.id, r.reviewUUID, r.document.String}) {
			f.Kata.Stale += n
			continue
		}
		if !input.Complete {
			f.Kata.Incomplete += n
			continue
		}
		f.Kata.Unavailable -= n
		f.Kata.State = "available"
		if !input.ReadAt.IsZero() {
			if f.Kata.ReadStartedAt == nil || input.ReadAt.Before(*f.Kata.ReadStartedAt) {
				v := input.ReadAt
				f.Kata.ReadStartedAt = &v
			}
			if f.Kata.ReadFinishedAt == nil || input.ReadAt.After(*f.Kata.ReadFinishedAt) {
				v := input.ReadAt
				f.Kata.ReadFinishedAt = &v
			}
		}
		matches := map[int][]FindingKataFinding{}
		roots := map[string]int{}
		for _, found := range input.Findings {
			matches[found.Index] = append(matches[found.Index], found)
			roots[found.RootUID]++
		}
		for i := range r.evidence {
			candidates := matches[i]
			if len(candidates) == 0 {
				f.Kata.Unmapped++
				continue
			}
			if len(candidates) != 1 || !candidates[0].VersionValid || candidates[0].RootUID == "" || roots[candidates[0].RootUID] != 1 {
				f.Kata.Stale++
				f.Kata.Unavailable++
				continue
			}
			f.Kata.Mapped++
			f.Kata.Readable++
			label := reduceKataFinding(candidates[0])
			r.evidence[i].KataRootUID = label.KataRootUID
			r.evidence[i].KataDiscussion = label.KataDiscussion
			for _, reply := range label.KataDiscussion {
				if reply.Edited {
					f.Kata.Edited++
					break
				}
			}
			if label.Tier == "" {
				continue
			}
			old := r.evidence[i]
			label.JobID, label.ReviewUUID, label.Index, label.Finding = old.JobID, old.ReviewUUID, old.Index, old.Finding
			label.AssessmentJobID, label.AssessmentUUID, label.RExplanation = old.AssessmentJobID, old.AssessmentUUID, old.Explanation
			label.ROutcome = old.Outcome
			r.evidence[i] = label
			if label.ReasonState == "invalid" {
				f.Kata.Invalid++
			}
			if label.ReasonState == "conflicting" {
				f.Kata.Conflicting++
			}
			switch label.Outcome {
			case "confirmed":
				f.Kata.Labeled++
			case "refuted":
				f.Kata.Labeled++
				f.Kata.Refutes++
				if label.ReasonState == "explicit" {
					f.Kata.ExplicitReasons++
				}
			case "superseded":
				f.Kata.Excluded++
			}
		}
	}
	if f.Kata.Unavailable > 0 && f.Kata.State == "available" {
		f.Kata.State = "partial"
		if f.Kata.Unavailable == f.Kata.Eligible {
			f.Kata.State = "unavailable"
		}
	}
	if f.Kata.State == "available" {
		f.UnavailableTiers = []string{"D", "panel R"}
	}
	f.aggregateFindings()
}

func reduceKataFinding(input FindingKataFinding) FindingEvidence {
	result := FindingEvidence{Outcome: "unlabeled", Reason: "unspecified", ReasonState: "absent", KataRootUID: input.RootUID, KataDiscussion: append([]FindingKataReply(nil), input.Replies...)}
	slices.SortFunc(result.KataDiscussion, func(a, b FindingKataReply) int { return cmp.Compare(a.UID, b.UID) })
	if !input.VersionValid || input.RootUID == "" {
		return result
	}
	type actor struct{ author, teammate string }
	latest := map[actor]FindingKataReply{}
	for _, r := range input.Replies {
		if r.TargetUID != input.RootUID || (r.Kind != "confirm" && r.Kind != "refute" && r.Kind != "supersede") {
			continue
		}
		key := actor{r.Author, r.Teammate}
		prev, ok := latest[key]
		if !ok || r.CreatedAt.After(prev.CreatedAt) || (r.CreatedAt.Equal(prev.CreatedAt) && r.UID > prev.UID) {
			latest[key] = r
		}
	}
	for _, r := range latest {
		result.KataReplies = append(result.KataReplies, r)
	}
	slices.SortFunc(result.KataReplies, func(a, b FindingKataReply) int { return cmp.Compare(a.UID, b.UID) })
	if len(latest) == 0 {
		return result
	}
	result.Tier = "K"
	kind, reason, state := "", "", ""
	invalid, conflicting, reasonConflict := false, false, false
	for _, r := range result.KataReplies {
		validReason := r.ReasonState == "absent" || (r.Kind == "refute" && r.ReasonState == "explicit" && (r.Reason == "false-positive" || r.Reason == "spurious" || r.Reason == "risk-accepted"))
		validEvidence := r.EvidenceValid && utf8.RuneCountInString(strings.TrimSpace(r.Evidence)) >= findingKataMinEvidenceRunes
		if r.Kind == "supersede" {
			validEvidence = r.EvidenceValid && strings.TrimSpace(r.Evidence) != ""
		}
		if r.UID == "" || r.Author == "" || r.CreatedAt.IsZero() || !validReason || !validEvidence {
			invalid = true
		}
		if kind != "" && kind != r.Kind {
			conflicting = true
		}
		kind = r.Kind
		current := r.Reason
		if r.ReasonState == "absent" {
			current = "unspecified"
		}
		if reason != "" && (reason != current || state != r.ReasonState) {
			reasonConflict = true
		}
		reason, state = current, r.ReasonState
	}
	if invalid {
		result.ReasonState = "invalid"
		return result
	}
	if conflicting {
		result.ReasonState = "conflicting"
		return result
	}
	switch kind {
	case "confirm":
		result.Outcome = "confirmed"
	case "refute":
		result.Outcome = "refuted"
		result.Reason = reason
		result.ReasonState = state
	case "supersede":
		result.Outcome = "superseded"
	}
	if kind == "refute" && reasonConflict {
		result.Reason = "unspecified"
		result.ReasonState = "conflicting"
	}
	return result
}

func addFindingEvidence(c *FindingOutcomeCounts, e FindingEvidence) {
	addFindingOutcome(c, e.Outcome)
	switch e.ReasonState {
	case "invalid":
		c.RefuteReasons.Invalid++
	case "conflicting":
		c.RefuteReasons.Conflicting++
	}
	if e.Outcome != "refuted" {
		return
	}
	switch e.Reason {
	case "false-positive":
		c.RefuteReasons.FalsePositive++
	case "spurious":
		c.RefuteReasons.Spurious++
	case "risk-accepted":
		c.RefuteReasons.RiskAccepted++
	default:
		c.RefuteReasons.Unspecified++
	}
	switch e.ReasonState {
	case "invalid", "conflicting", "explicit":
	default:
		c.RefuteReasons.Absent++
	}
}
