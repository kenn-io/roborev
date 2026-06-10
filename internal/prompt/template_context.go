package prompt

import "slices"

type TemplateContext struct {
	Meta    PromptMeta
	Review  *ReviewTemplateContext
	Address *AddressTemplateContext
	System  *SystemTemplateContext
}

func (c TemplateContext) Clone() TemplateContext {
	cloned := c
	if c.Review != nil {
		review := c.Review.Clone()
		cloned.Review = &review
	}
	if c.Address != nil {
		address := c.Address.Clone()
		cloned.Address = &address
	}
	if c.System != nil {
		system := *c.System
		cloned.System = &system
	}
	return cloned
}

type PromptMeta struct {
	AgentName  string
	PromptType string
	ReviewType string
}

type ReviewTemplateContext struct {
	Kind     ReviewKind
	Optional ReviewOptionalContext
	Subject  SubjectContext
	Diff     DiffContext
	Fallback FallbackContext
}

func (c ReviewTemplateContext) Clone() ReviewTemplateContext {
	cloned := c
	cloned.Optional = c.Optional.Clone()
	cloned.Subject = c.Subject.Clone()
	cloned.Diff = c.Diff.Clone()
	cloned.Fallback = c.Fallback.Clone()
	return cloned
}

type ReviewKind string

const (
	ReviewKindSingle ReviewKind = "single"
	ReviewKindRange  ReviewKind = "range"
	ReviewKindDirty  ReviewKind = "dirty"
)

type ReviewOptionalContext struct {
	ProjectGuidelines  *MarkdownSection
	KataContext        *MarkdownSection
	AdditionalContext  string
	DependencyMetadata string
	PreviousReviews    []PreviousReviewTemplateContext
	InRangeReviews     []InRangeReviewTemplateContext
	PreviousAttempts   []ReviewAttemptTemplateContext
}

func (o ReviewOptionalContext) Clone() ReviewOptionalContext {
	cloned := o
	if o.ProjectGuidelines != nil {
		section := *o.ProjectGuidelines
		cloned.ProjectGuidelines = &section
	}
	if o.KataContext != nil {
		section := *o.KataContext
		cloned.KataContext = &section
	}
	cloned.PreviousReviews = slices.Clone(o.PreviousReviews)
	for i := range cloned.PreviousReviews {
		cloned.PreviousReviews[i].Comments = slices.Clone(cloned.PreviousReviews[i].Comments)
	}
	cloned.InRangeReviews = slices.Clone(o.InRangeReviews)
	for i := range cloned.InRangeReviews {
		cloned.InRangeReviews[i].Comments = slices.Clone(cloned.InRangeReviews[i].Comments)
	}
	cloned.PreviousAttempts = slices.Clone(o.PreviousAttempts)
	for i := range cloned.PreviousAttempts {
		cloned.PreviousAttempts[i].Comments = slices.Clone(cloned.PreviousAttempts[i].Comments)
	}
	return cloned
}

func (o ReviewOptionalContext) IsEmpty() bool {
	return o.ProjectGuidelines == nil &&
		o.KataContext == nil &&
		o.AdditionalContext == "" &&
		o.DependencyMetadata == "" &&
		len(o.PreviousReviews) == 0 &&
		len(o.InRangeReviews) == 0 &&
		len(o.PreviousAttempts) == 0
}

func (o ReviewOptionalContext) ProjectGuidelinesBody() string {
	if o.ProjectGuidelines == nil {
		return ""
	}
	return o.ProjectGuidelines.Body
}

func (o *ReviewOptionalContext) TrimNext() bool {
	if o == nil {
		return false
	}
	switch {
	case len(o.PreviousAttempts) > 0:
		o.PreviousAttempts = nil
	case len(o.InRangeReviews) > 0:
		o.InRangeReviews = nil
	case len(o.PreviousReviews) > 0:
		o.PreviousReviews = nil
	case o.DependencyMetadata != "":
		o.DependencyMetadata = ""
	case o.AdditionalContext != "":
		o.AdditionalContext = ""
	case o.ProjectGuidelines != nil:
		o.ProjectGuidelines = nil
	case o.KataContext != nil:
		o.KataContext = nil
	default:
		return false
	}
	return true
}

type SubjectContext struct {
	Single *SingleSubjectContext
	Range  *RangeSubjectContext
	Dirty  *DirtySubjectContext
}

func (s SubjectContext) Clone() SubjectContext {
	cloned := s
	if s.Single != nil {
		single := *s.Single
		cloned.Single = &single
	}
	if s.Range != nil {
		rangeCtx := *s.Range
		rangeCtx.Entries = slices.Clone(s.Range.Entries)
		cloned.Range = &rangeCtx
	}
	if s.Dirty != nil {
		dirty := *s.Dirty
		cloned.Dirty = &dirty
	}
	return cloned
}

func (s *SubjectContext) TrimSingleMessage() bool {
	if s == nil || s.Single == nil || s.Single.Message == "" {
		return false
	}
	s.Single.Message = ""
	return true
}

func (s *SubjectContext) TrimSingleAuthor() bool {
	if s == nil || s.Single == nil || s.Single.Author == "" {
		return false
	}
	s.Single.Author = ""
	return true
}

func (s *SubjectContext) TrimSingleSubjectTo(maxBytes int) bool {
	if s == nil || s.Single == nil || s.Single.Subject == "" {
		return false
	}
	next := truncateUTF8(s.Single.Subject, maxBytes)
	if next == s.Single.Subject {
		return false
	}
	s.Single.Subject = next
	return true
}

func (s *SubjectContext) BlankNextRangeSubject() bool {
	if s == nil || s.Range == nil {
		return false
	}
	for i := range slices.Backward(s.Range.Entries) {
		if s.Range.Entries[i].Subject == "" {
			continue
		}
		s.Range.Entries[i].Subject = ""
		return true
	}
	return false
}

func (s *SubjectContext) DropLastRangeEntry() bool {
	if s == nil || s.Range == nil || len(s.Range.Entries) == 0 {
		return false
	}
	s.Range.Entries = s.Range.Entries[:len(s.Range.Entries)-1]
	return true
}

type SingleSubjectContext struct {
	Commit  string
	Subject string
	Author  string
	Message string
}

type RangeSubjectContext struct {
	Count   int
	Entries []RangeEntryContext
}

type RangeEntryContext struct {
	Commit  string
	Subject string
}

type DirtySubjectContext struct {
	Description string
}

type DiffContext struct {
	Heading string
	Body    string
}

func (d DiffContext) Clone() DiffContext {
	return d
}

func (d DiffContext) HasBody() bool {
	return d.Body != ""
}

type FallbackMode string

const (
	FallbackModeNone    FallbackMode = ""
	FallbackModeCommit  FallbackMode = "commit"
	FallbackModeRange   FallbackMode = "range"
	FallbackModeDirty   FallbackMode = "dirty"
	FallbackModeGeneric FallbackMode = "generic"
)

type FallbackContext struct {
	Mode    FallbackMode
	Text    string
	Commit  *CommitFallbackContext
	Range   *RangeFallbackContext
	Dirty   *DirtyFallbackContext
	Generic *GenericFallbackContext
}

func (f FallbackContext) Clone() FallbackContext {
	cloned := f
	if f.Commit != nil {
		commit := *f.Commit
		cloned.Commit = &commit
	}
	if f.Range != nil {
		rangeCtx := *f.Range
		cloned.Range = &rangeCtx
	}
	if f.Dirty != nil {
		dirty := *f.Dirty
		cloned.Dirty = &dirty
	}
	if f.Generic != nil {
		generic := *f.Generic
		cloned.Generic = &generic
	}
	return cloned
}

func (f FallbackContext) IsEmpty() bool {
	return f.Mode == FallbackModeNone && f.Text == "" && f.Commit == nil && f.Range == nil && f.Dirty == nil && f.Generic == nil
}

func (f FallbackContext) HasContent() bool {
	return !f.IsEmpty()
}

func (f FallbackContext) Rendered() string {
	switch {
	case f.Text != "":
		return f.Text
	case f.Dirty != nil:
		return f.Dirty.Body
	default:
		return ""
	}
}

type CommitFallbackContext struct {
	SHA         string
	StatCmd     string
	DiffCmd     string
	FilesCmd    string
	ShowPathCmd string
}

type RangeFallbackContext struct {
	RangeRef string
	LogCmd   string
	StatCmd  string
	DiffCmd  string
	FilesCmd string
	ViewCmd  string
}

type DirtyFallbackContext struct {
	Body string
}

type GenericFallbackContext struct {
	ViewCmd string
}

type AddressTemplateContext struct {
	ProjectGuidelines *MarkdownSection
	ToolAttempts      []AddressAttemptTemplateContext
	UserComments      []AddressAttemptTemplateContext
	SeverityFilter    string
	ReviewFindings    string
	OriginalDiff      string
	JobID             int64
}

func (c AddressTemplateContext) Clone() AddressTemplateContext {
	cloned := c
	if c.ProjectGuidelines != nil {
		section := *c.ProjectGuidelines
		cloned.ProjectGuidelines = &section
	}
	cloned.ToolAttempts = slices.Clone(c.ToolAttempts)
	cloned.UserComments = slices.Clone(c.UserComments)
	return cloned
}

type SystemTemplateContext struct {
	NoSkillsInstruction              string
	ToolchainVerificationInstruction string
	CurrentDate                      string
}

type MarkdownSection struct {
	Heading string
	Body    string
}

type ReviewCommentTemplateContext struct {
	Responder string
	Response  string
}

type PreviousReviewTemplateContext struct {
	Commit    string
	Output    string
	Comments  []ReviewCommentTemplateContext
	Available bool
}

type InRangeReviewTemplateContext struct {
	Commit   string
	Agent    string
	Verdict  string
	Output   string
	Comments []ReviewCommentTemplateContext
}

type ReviewAttemptTemplateContext struct {
	Label    string
	Agent    string
	When     string
	Output   string
	Comments []ReviewCommentTemplateContext
}

type AddressAttemptTemplateContext struct {
	Responder string
	Response  string
	When      string
}
