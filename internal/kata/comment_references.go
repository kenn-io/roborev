package kata

import "strings"

// CommentReference is a full UID or a suffix with optional frozen destination qualifiers.
type CommentReference struct{ UID, Project, Issue, Suffix string }

// ParseCommentReference follows Kata v0.19.0 internal/commentref/ref.go: UIDs and optional
// project/issue-qualified Crockford suffixes. Its minimum comment suffix is six
// characters; internal/shortid/shortid.go caps suffixes at the 26-character UID.
func ParseCommentReference(input string) (CommentReference, bool) {
	if validCommentUID(input) {
		return CommentReference{UID: input}, true
	}
	left, suffix, ok := strings.CutLast(input, ":")
	if !ok {
		return CommentReference{}, false
	}
	suffix = strings.ToLower(suffix)
	validSuffix := func(value string, minimum int) bool {
		return len(value) >= minimum && len(value) <= 26 && strings.IndexFunc(value, func(r rune) bool {
			return !strings.ContainsRune("0123456789abcdefghjkmnpqrstvwxyz", r)
		}) < 0
	}
	if !validSuffix(suffix, 6) {
		return CommentReference{}, false
	}
	if strings.EqualFold(left, "c") {
		return CommentReference{Suffix: suffix}, true
	}
	project, issue := "", left
	if prefix, rest, found := strings.CutLast(left, "#"); found {
		project, issue = prefix, rest
		if project == "" {
			return CommentReference{}, false
		}
	}
	issue = strings.ToLower(issue)
	if !validSuffix(issue, 4) {
		return CommentReference{}, false
	}
	return CommentReference{Project: project, Issue: issue, Suffix: suffix}, true
}

// ValidCommentReference checks syntax without a provider or binding lookup.
func ValidCommentReference(input string) bool {
	_, ok := ParseCommentReference(input)
	return ok
}

// ResolveCommentReference selects a unique comment in a complete frozen issue
// read. It never uses the caller's current workspace or default project.
func ResolveCommentReference(issue Issue, input string) (string, error) {
	ref, ok := ParseCommentReference(input)
	if !ok {
		return "", ErrRequestRejected
	}
	if issue.CommentsTruncated {
		return "", ErrInvalidResponse
	}
	project := ""
	if hash := strings.LastIndexByte(issue.QualifiedID, '#'); hash >= 0 {
		project = issue.QualifiedID[:hash]
	}
	if ref.Project != "" && ref.Project != project || ref.Issue != "" && !strings.EqualFold(ref.Issue, issue.ShortID) {
		return "", ErrCommentNotFound
	}
	found := ""
	for _, comment := range issue.Comments {
		matches := ref.UID != "" && strings.EqualFold(comment.UID, ref.UID) || ref.Suffix != "" && strings.HasSuffix(strings.ToLower(comment.UID), ref.Suffix)
		if !matches || comment.IssueUID != issue.UID {
			continue
		}
		if found != "" {
			return "", ErrInvalidResponse
		}
		found = comment.UID
	}
	if found == "" {
		return "", ErrCommentNotFound
	}
	return found, nil
}
