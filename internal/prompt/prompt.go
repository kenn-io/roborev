package prompt

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
)

// ErrDiffTruncatedNoFile is returned when the diff is too large to
// inline and no snapshot file path was provided. Callers should write
// the diff to a file and retry with BuildWithDiffFile.
var ErrDiffTruncatedNoFile = errors.New("diff too large to inline and no snapshot file available")

// escapeXML escapes XML special characters so untrusted commit metadata
// (subject, author, body) cannot break out of the <commit-message> /
// <commit-messages> wrapper tags and inject synthetic structure into the
// prompt.
func escapeXML(s string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return "--unescapable-xml--"
	}
	return buf.String()
}

// MaxPromptSize is the legacy maximum size of a prompt in bytes (250KB).
// New code should use Builder.maxPromptSize() which respects config.
const MaxPromptSize = 250 * 1024

// noSkillsInstruction tells agents not to delegate the review to external
// tools or skills, and to return only the final review content. Verdict
// parsing intentionally does not try to decode narrative process updates or
// caveats in free-form prose; those output-shaping issues are better handled
// in the prompt than in deterministic parsing heuristics.
const noSkillsInstruction = `

IMPORTANT: You are being invoked by roborev to perform this review directly. Do NOT use any external skills, slash commands, or CLI tools (such as "roborev review") to delegate this task. Perform the review yourself by analyzing the diff provided below.

Return only the final review content. Do NOT include process narration, progress updates, or front matter such as "Reviewing the diff..." or "I'm checking...".
If you use tools while reviewing, finish all tool use before emitting the final review, and put the final review only after the last tool call.`

// HistoricalReviewContext holds a commit SHA and its associated review (if any) plus responses.
type HistoricalReviewContext struct {
	SHA       string
	Review    *storage.Review
	Responses []storage.Response
}

// Builder constructs review prompts
type Builder struct {
	db        *storage.DB
	globalCfg *config.Config // optional global config for exclude patterns
	repoPath  string
	repoID    int64
}

// DiffFilePathPlaceholder is a sentinel path embedded in prebuilt
// prompts for oversized diffs. The worker replaces it with a real
// diff file path at execution time so the stored prompt remains
// reusable across retries.
const DiffFilePathPlaceholder = "/tmp/roborev diff placeholder"

const dirtyTruncatedDiffMarker = "(Diff too large to include in full)"

const DefaultStaleSnapshotAge = 24 * time.Hour

const snapshotMarkerFile = ".roborev-snapshot"

var (
	activeSnapshotDirs sync.Map
	// Protects the lifecycle window where snapshot directories are created,
	// marked, and registered before cleanup may consider them stale.
	snapshotLifecycleMu sync.Mutex
)

// NewBuilder creates a new prompt builder
func NewBuilder(db *storage.DB) *Builder {
	return &Builder{db: db}
}

// NewBuilderWithConfig creates a prompt builder that also resolves
// global config settings (e.g., exclude_patterns).
func NewBuilderWithConfig(
	db *storage.DB, globalCfg *config.Config,
) *Builder {
	return &Builder{db: db, globalCfg: globalCfg}
}

// ForRepo returns a builder scoped to a repository.
func (b *Builder) ForRepo(repoPath string, repoID int64) *Builder {
	next := *b
	next.repoPath = repoPath
	next.repoID = repoID
	return &next
}

// resolveMaxPromptSize returns the effective prompt budget from config.
func (b *Builder) resolveMaxPromptSize() int {
	return config.ResolveMaxPromptSize(b.repoPath, b.globalCfg)
}

// resolveExcludes returns the merged exclude patterns for a repo.
// Security reviews skip repo-level patterns to prevent a compromised
// default branch from hiding files from review.
func (b *Builder) resolveExcludes(
	reviewType string,
) []string {
	return config.ResolveExcludePatterns(
		b.repoPath, b.globalCfg, reviewType,
	)
}

// Build constructs a review prompt for a commit or range with context from previous reviews.
// reviewType selects the system prompt variant (e.g., "security"); any default alias (see config.IsDefaultReviewType) uses the standard prompt.
func (b *Builder) Build(gitRef string, contextCount int, agentName, reviewType, minSeverity string) (string, error) {
	return b.BuildWithAdditionalContext(gitRef, contextCount, agentName, reviewType, minSeverity, "")
}

// BuildWithAdditionalContext constructs a review prompt with an optional
// caller-provided markdown context block inserted ahead of the current diff.
func (b *Builder) BuildWithAdditionalContext(gitRef string, contextCount int, agentName, reviewType, minSeverity, additionalContext string) (string, error) {
	return b.buildWithOpts(gitRef, contextCount, agentName, reviewType, buildOpts{
		additionalContext: additionalContext,
		minSeverity:       minSeverity,
	})
}

// BuildWithAdditionalContextAndDiffFile constructs a review prompt with
// caller-provided markdown context and an optional oversized-diff file reference.
func (b *Builder) BuildWithAdditionalContextAndDiffFile(gitRef string, contextCount int, agentName, reviewType, minSeverity, additionalContext, diffFilePath string) (string, error) {
	return b.buildWithOpts(gitRef, contextCount, agentName, reviewType, buildOpts{
		additionalContext: additionalContext,
		diffFilePath:      diffFilePath,
		requireDiffFile:   true,
		minSeverity:       minSeverity,
	})
}

// BuildWithDiffFile constructs a review prompt where a pre-written diff file
// is referenced for large diffs instead of inline content.
func (b *Builder) BuildWithDiffFile(gitRef string, contextCount int, agentName, reviewType, minSeverity, diffFilePath string) (string, error) {
	return b.buildWithOpts(gitRef, contextCount, agentName, reviewType, buildOpts{
		diffFilePath:    diffFilePath,
		requireDiffFile: true,
		minSeverity:     minSeverity,
	})
}

func (b *Builder) buildWithOpts(gitRef string, contextCount int, agentName, reviewType string, opts buildOpts) (string, error) {
	if git.IsRange(gitRef) {
		return b.buildRangePrompt(gitRef, contextCount, agentName, reviewType, opts)
	}
	return b.buildSinglePrompt(gitRef, contextCount, agentName, reviewType, opts)
}

// SnapshotResult holds a prompt and an optional cleanup function for a diff snapshot file.
type SnapshotResult struct {
	Prompt  string
	Cleanup func()
}

// BuildWithSnapshot builds a review prompt, automatically writing a diff snapshot file
// when the diff is too large to inline.
func (b *Builder) BuildWithSnapshot(gitRef string, contextCount int, agentName, reviewType, minSeverity string, excludes []string) (SnapshotResult, error) {
	p, err := b.BuildWithDiffFile(gitRef, contextCount, agentName, reviewType, minSeverity, "")
	if !errors.Is(err, ErrDiffTruncatedNoFile) {
		return SnapshotResult{Prompt: p}, err
	}
	diffFile, cleanup, writeErr := b.WriteDiffSnapshot(gitRef, excludes)
	if writeErr != nil {
		return SnapshotResult{}, fmt.Errorf("write diff snapshot: %w", writeErr)
	}
	p, err = b.BuildWithDiffFile(gitRef, contextCount, agentName, reviewType, minSeverity, diffFile)
	if err != nil {
		cleanup()
		return SnapshotResult{}, err
	}
	return SnapshotResult{Prompt: p, Cleanup: cleanup}, nil
}

// WriteDiffSnapshot writes the full diff for a git ref to a repo-local temp
// file. The file intentionally lives outside .git so sandboxed agents can read
// it without inlining oversized diffs into the submitted prompt.
func (b *Builder) WriteDiffSnapshot(gitRef string, excludes []string) (string, func(), error) {
	var (
		fullDiff string
		err      error
	)
	if git.IsRange(gitRef) {
		fullDiff, err = git.GetRangeDiff(b.repoPath, gitRef, excludes...)
	} else {
		fullDiff, err = git.GetDiff(b.repoPath, gitRef, excludes...)
	}
	if err != nil {
		return "", nil, fmt.Errorf("capture diff: %w", err)
	}
	if fullDiff == "" {
		return "", nil, fmt.Errorf("diff is empty")
	}
	return b.writeExternalDiffSnapshot(fullDiff)
}

func (b *Builder) writeExternalDiffSnapshot(diff string) (string, func(), error) {
	snapshotRoot, err := config.ResolveSnapshotDir(b.repoPath)
	if err != nil {
		return "", nil, fmt.Errorf("resolve snapshot dir: %w", err)
	}
	if err := validateSnapshotRoot(b.repoPath, snapshotRoot); err != nil {
		return "", nil, err
	}
	if err := ensureSnapshotRootIgnored(b.repoPath, snapshotRoot); err != nil {
		return "", nil, fmt.Errorf("ensure snapshot dir ignored: %w", err)
	}
	if err := os.MkdirAll(snapshotRoot, 0o755); err != nil {
		return "", nil, fmt.Errorf("create snapshot root: %w", err)
	}
	if err := validateSnapshotRoot(b.repoPath, snapshotRoot); err != nil {
		return "", nil, err
	}
	snapshotLifecycleMu.Lock()
	dir, err := os.MkdirTemp(snapshotRoot, "roborev-snapshot-*")
	if err != nil {
		snapshotLifecycleMu.Unlock()
		return "", nil, fmt.Errorf("create snapshot dir: %w", err)
	}
	if err := writeSnapshotMarker(dir); err != nil {
		os.RemoveAll(dir)
		snapshotLifecycleMu.Unlock()
		return "", nil, fmt.Errorf("write snapshot marker: %w", err)
	}
	unregister := registerActiveSnapshot(dir)
	snapshotLifecycleMu.Unlock()
	diffFile := dir + string(os.PathSeparator) + "roborev-snapshot-content.diff"
	f, err := os.OpenFile(diffFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		unregister()
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("create snapshot: %w", err)
	}
	_, writeErr := f.WriteString(diff)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		unregister()
		os.RemoveAll(dir)
		if writeErr != nil {
			return "", nil, fmt.Errorf("write snapshot: %w", writeErr)
		}
		return "", nil, fmt.Errorf("close snapshot: %w", closeErr)
	}
	return diffFile, func() {
		os.RemoveAll(dir)
		unregister()
	}, nil
}

func writeSnapshotMarker(dir string) error {
	return os.WriteFile(filepath.Join(dir, snapshotMarkerFile), []byte("roborev snapshot\n"), 0o600)
}

func registerActiveSnapshot(dir string) func() {
	activeSnapshotDirs.Store(dir, struct{}{})
	return func() {
		activeSnapshotDirs.Delete(dir)
	}
}

func isActiveSnapshotLocked(dir string) bool {
	_, ok := activeSnapshotDirs.Load(dir)
	return ok
}

func snapshotIsActive(dir string) bool {
	snapshotLifecycleMu.Lock()
	defer snapshotLifecycleMu.Unlock()
	return isActiveSnapshotLocked(dir)
}

func validateSnapshotRoot(repoPath, snapshotRoot string) error {
	if err := git.ValidateRepoLocalPathNoSymlinks(repoPath, snapshotRoot); err != nil {
		return fmt.Errorf("validate snapshot dir: %w", err)
	}
	if err := git.EnsureNoTrackedFilesUnder(repoPath, snapshotRoot); err != nil {
		return fmt.Errorf("validate snapshot dir: %w", err)
	}
	return nil
}

func ensureSnapshotRootIgnored(repoPath, snapshotRoot string) error {
	pattern, probe, err := git.IgnorePatternForDir(repoPath, snapshotRoot)
	if err != nil {
		return err
	}
	ignored, err := git.CheckIgnoreNoIndex(repoPath, probe)
	if err != nil {
		return err
	}
	if ignored {
		return nil
	}
	if err := git.EnsureLocalExcludePattern(repoPath, pattern); err != nil {
		return err
	}
	ignored, err = git.CheckIgnoreNoIndex(repoPath, probe)
	if err != nil {
		return err
	}
	if !ignored {
		return fmt.Errorf("snapshot dir %s is still not ignored after updating git exclude", snapshotRoot)
	}
	return nil
}

// CleanupStaleSnapshots removes old roborev snapshot directories from the
// repo-local snapshot root. It is best-effort cleanup for daemon crashes or
// process exits that happen before per-job cleanup runs.
func (b *Builder) CleanupStaleSnapshots(olderThan time.Duration) error {
	if olderThan <= 0 {
		olderThan = DefaultStaleSnapshotAge
	}
	snapshotRoot, err := config.ResolveSnapshotDir(b.repoPath)
	if err != nil {
		return fmt.Errorf("resolve snapshot dir: %w", err)
	}
	if err := validateSnapshotRoot(b.repoPath, snapshotRoot); err != nil {
		return err
	}
	entries, err := os.ReadDir(snapshotRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read snapshot root: %w", err)
	}
	cutoff := time.Now().Add(-olderThan)
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "roborev-snapshot-") {
			continue
		}
		path := filepath.Join(snapshotRoot, entry.Name())
		info, err := entry.Info()
		if err != nil {
			errs = append(errs, fmt.Errorf("stat snapshot dir %s: %w", path, err))
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if snapshotIsActive(path) {
			continue
		}
		if _, err := os.Stat(filepath.Join(path, snapshotMarkerFile)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("stat snapshot marker %s: %w", path, err))
			continue
		}
		hasTrackedFiles, err := git.HasTrackedFilesUnder(b.repoPath, path)
		if err != nil {
			errs = append(errs, fmt.Errorf("check tracked files under snapshot dir %s: %w", path, err))
			continue
		}
		if hasTrackedFiles {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove stale snapshot dir %s: %w", path, err))
			continue
		}
	}
	return errors.Join(errs...)
}

// BuildDirtyWithSnapshot builds a dirty review prompt, writing the diff to a snapshot file
// when it's too large to inline.
func (b *Builder) BuildDirtyWithSnapshot(diff string, contextCount int, agentName, reviewType, minSeverity string) (SnapshotResult, error) {
	p, err := b.BuildDirty(diff, contextCount, agentName, reviewType, minSeverity)
	if err != nil {
		return SnapshotResult{}, err
	}
	if strings.Contains(p, dirtyTruncatedDiffMarker) && len(diff) > 0 {
		diffFile, cleanup, snapErr := b.writeExternalDiffSnapshot(diff)
		if snapErr != nil {
			return SnapshotResult{}, fmt.Errorf("dirty diff snapshot: %w", snapErr)
		}
		p, err = fitDirtySnapshotReference(p, diffFile, b.resolveMaxPromptSize())
		if err != nil {
			cleanup()
			return SnapshotResult{}, err
		}
		return SnapshotResult{Prompt: p, Cleanup: cleanup}, nil
	}
	return SnapshotResult{Prompt: p}, nil
}

// BuildDirty constructs a review prompt for uncommitted (dirty) changes.
// The diff is provided directly since it was captured at enqueue time.
// reviewType selects the system prompt variant (e.g., "security"); any default alias (see config.IsDefaultReviewType) uses the standard prompt.
func (b *Builder) BuildDirty(diff string, contextCount int, agentName, reviewType, minSeverity string) (string, error) {
	ctx := b.newPromptBuildContext(agentName, reviewType, minSeverity, "dirty", optionalSectionsView{})

	// Add project-specific guidelines if configured
	if repoCfg, err := config.LoadRepoConfig(b.repoPath); err == nil && repoCfg != nil {
		ctx.optional.ProjectGuidelines = buildProjectGuidelinesSectionView(repoCfg.ReviewGuidelines)
	}

	// Get previous reviews for context (use HEAD as reference point)
	if contextCount > 0 && b.db != nil {
		headSHA, err := git.ResolveSHA(b.repoPath, "HEAD")
		if err == nil {
			contexts, err := b.getPreviousReviewContexts(headSHA, contextCount)
			if err == nil && len(contexts) > 0 {
				ctx.optional.PreviousReviews = orderedPreviousReviewViews(contexts)
			}
		}
	}

	bodyLimit := max(0, ctx.promptCap-len(ctx.requiredPrefix))
	inlineDiff, err := renderInlineDiff(diff)
	if err != nil {
		return "", err
	}
	view := dirtyPromptView{
		Optional: ctx.optional,
		Current: dirtyChangesSectionView{
			Description: "The following changes have not yet been committed.",
		},
		Diff: diffSectionView{
			Heading: "### Diff",
			Body:    inlineDiff,
		},
	}

	currentSection, err := renderDirtyChangesSection(view.Current)
	if err != nil {
		return "", err
	}
	fullDiffBlock, err := renderDiffBlock(view.Diff)
	if err != nil {
		return "", err
	}
	if len(currentSection)+len(fullDiffBlock) > bodyLimit {
		fallbackOnly, err := renderDirtyTruncatedDiffFallback("")
		if err != nil {
			return "", err
		}
		fallbackBlock, err := renderDiffBlock(diffSectionView{Heading: "### Diff", Fallback: fallbackOnly})
		if err != nil {
			return "", err
		}
		maxDiffLen := bodyLimit - len(currentSection) - len(fallbackBlock)
		view.Diff.Body = ""
		sizingView := view
		sizingBody, err := renderDirtyPrompt(sizingView)
		if err != nil {
			return "", err
		}
		for len(sizingBody) > bodyLimit && trimOptionalSections(&sizingView.Optional) {
			sizingBody, err = renderDirtyPrompt(sizingView)
			if err != nil {
				return "", err
			}
		}
		if maxDiffLen > 1000 {
			emptyFallbackOptional := sizingView.Optional
			sampleBody := "X\n"
			sampleFallback, err := renderDirtyTruncatedDiffFallback(sampleBody)
			if err != nil {
				return "", err
			}
			wrapperOverhead := len(sampleFallback) - len(fallbackOnly) - len(sampleBody)
			truncationSuffix := "\n... (truncated)\n"
			availableContentLen := maxDiffLen - wrapperOverhead - len(truncationSuffix)
			if availableContentLen > 0 {
				truncatedContent := truncateUTF8(diff, availableContentLen)
				for truncatedContent != "" {
					truncatedBody := truncatedContent
					if !strings.HasSuffix(truncatedBody, "\n") {
						truncatedBody += "\n"
					}
					truncatedBody += "... (truncated)\n"
					view.Diff.Fallback, err = renderDirtyTruncatedDiffFallback(truncatedBody)
					if err != nil {
						return "", err
					}
					sizingView.Diff = view.Diff
					rendered, err := renderDirtyPrompt(sizingView)
					if err != nil {
						return "", err
					}
					if len(rendered) <= bodyLimit {
						view.Optional = sizingView.Optional
						break
					}
					if trimOptionalSections(&sizingView.Optional) {
						continue
					}
					overflow := len(rendered) - bodyLimit
					next := truncateUTF8(truncatedContent, max(0, len(truncatedContent)-overflow))
					if next == truncatedContent {
						next = truncateUTF8(truncatedContent, max(0, len(truncatedContent)-1))
					}
					truncatedContent = next
				}
				if truncatedContent == "" {
					view.Diff.Fallback = fallbackOnly
					view.Optional = emptyFallbackOptional
				}
			} else {
				view.Diff.Fallback = fallbackOnly
			}
		} else {
			view.Diff.Fallback = fallbackOnly
		}
	}

	body, err := fitDirtyPromptContext(bodyLimit, templateContextFromDirtyView(view))
	if err != nil {
		return "", err
	}
	return ctx.requiredPrefix + hardCapPrompt(body, bodyLimit), nil
}

func fitDirtySnapshotReference(prompt, diffFile string, limit int) (string, error) {
	variants := dirtySnapshotReferenceVariants(diffFile)
	prefix := prompt
	if before, _, found := strings.Cut(prompt, dirtyTruncatedDiffMarker); found {
		prefix = before
	}
	return fitPrefixWithSuffixVariants(prefix, limit, variants...)
}

func dirtySnapshotReferenceVariants(diffFile string) []string {
	return []string{
		fmt.Sprintf("%s\nThe full diff has been written to a file for review.\nRead the diff from: `%s`\n\nReview the actual diff before writing findings.\n", dirtyTruncatedDiffMarker, diffFile),
		fmt.Sprintf("%s\nRead the diff from: `%s`\n", dirtyTruncatedDiffMarker, diffFile),
		fmt.Sprintf("(Diff too large; read `%s`.)\n", diffFile),
	}
}

func fitPrefixWithSuffixVariants(prefix string, limit int, variants ...string) (string, error) {
	if limit <= 0 {
		return "", fmt.Errorf("prompt limit must be positive, got %d", limit)
	}
	if len(variants) == 0 {
		return truncateUTF8(prefix, limit), nil
	}
	for _, variant := range variants {
		if len(prefix)+len(variant) <= limit {
			return prefix + variant, nil
		}
	}
	shortest := variants[len(variants)-1]
	if len(shortest) > limit {
		return "", fmt.Errorf("required prompt suffix is %d bytes but prompt limit is %d bytes", len(shortest), limit)
	}
	return truncateUTF8(prefix, limit-len(shortest)) + shortest, nil
}

func isCodexReviewAgent(agentName string) bool {
	return strings.EqualFold(strings.TrimSpace(agentName), "codex")
}

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

func hardCapPrompt(prompt string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(prompt) <= limit {
		return prompt
	}
	return truncateUTF8(prompt, limit)
}

type buildOpts struct {
	additionalContext string
	// diffFilePath, when non-empty, is a file containing the full
	// diff that the prompt can reference for oversized diffs.
	diffFilePath string
	// requireDiffFile makes truncation an error when no file path
	// is available. Set by BuildWithDiffFile so the worker can
	// detect when a snapshot is needed.
	requireDiffFile bool
	// minSeverity, when non-empty, injects a severity filter
	// instruction into the system prompt.
	minSeverity string
}

type promptBuildContext struct {
	requiredPrefix string
	optional       optionalSectionsView
	promptCap      int
}

func (b *Builder) newPromptBuildContext(agentName, reviewType, minSeverity, defaultPromptType string, optional optionalSectionsView) promptBuildContext {
	promptType := defaultPromptType
	if !config.IsDefaultReviewType(reviewType) {
		promptType = reviewType
	}
	if promptType == config.ReviewTypeDesign {
		promptType = "design-review"
	}
	promptCap := b.resolveMaxPromptSize()
	requiredPrefix := GetSystemPrompt(agentName, promptType) + "\n"
	if inst := config.SeverityInstruction(minSeverity); inst != "" {
		requiredPrefix += inst + "\n"
	}
	return promptBuildContext{
		requiredPrefix: hardCapPrompt(requiredPrefix, promptCap),
		optional:       optional,
		promptCap:      promptCap,
	}
}

func defaultOptionalSections(repoPath, additionalContext string) optionalSectionsView {
	return optionalSectionsView{
		ProjectGuidelines: buildProjectGuidelinesSectionView(LoadGuidelines(repoPath)),
		AdditionalContext: buildAdditionalContextSection(additionalContext),
	}
}

func diffFileFallbackVariants(heading, filePath string) []string {
	if filePath == "" {
		return []string{heading + "\n\n(Diff too large to include inline)\n"}
	}
	return []string{
		fmt.Sprintf("%s\n\n(Diff too large to include inline)\n\nThe full diff has been written to a file for review.\nRead the diff from: `%s`\n\nReview the actual diff before writing findings.\n", heading, filePath),
		fmt.Sprintf("%s\n\n(Diff too large to include inline)\nRead the diff from: `%s`\n", heading, filePath),
	}
}

func writeLongestFitting(sb *strings.Builder, limit int, variants ...string) {
	if len(variants) == 0 || limit <= 0 {
		return
	}
	shortest := variants[len(variants)-1]
	remaining := limit - sb.Len()
	if remaining <= 0 {
		return
	}
	for _, variant := range variants {
		if len(variant) <= remaining {
			sb.WriteString(variant)
			return
		}
	}
	sb.WriteString(truncateUTF8(shortest, remaining))
}

func buildPromptPreservingCurrentSection(requiredPrefix, optionalContext, currentRequired, currentOverflow string, limit int, variants ...string) string {
	shortestLen := 0
	if len(variants) > 0 {
		shortestLen = len(variants[len(variants)-1])
	}
	softBudget := max(0, limit-len(requiredPrefix)-len(currentRequired)-shortestLen)
	softLen := len(optionalContext) + len(currentOverflow)
	if softLen > softBudget {
		overflow := softLen - softBudget
		if overflow > 0 && len(optionalContext) > 0 {
			originalLen := len(optionalContext)
			trimmedLen := max(0, len(optionalContext)-overflow)
			optionalContext = truncateUTF8(optionalContext, trimmedLen)
			overflow -= originalLen - len(optionalContext)
		}
		if overflow > 0 && len(currentOverflow) > 0 {
			currentOverflow = truncateUTF8(currentOverflow, max(0, len(currentOverflow)-overflow))
		}
	}

	var sb strings.Builder
	sb.WriteString(requiredPrefix)
	sb.WriteString(optionalContext)
	sb.WriteString(currentRequired)
	sb.WriteString(currentOverflow)
	writeLongestFitting(&sb, limit, variants...)
	return hardCapPrompt(sb.String(), limit)
}

// safeForMarkdown filters pathspec args to only those that can be
// safely embedded in markdown inline code spans. Args containing
// backticks or control characters are dropped.
func safeForMarkdown(args []string) []string {
	var safe []string
	for _, a := range args {
		ok := true
		for _, r := range a {
			if r < ' ' || r == '`' || r == 0x7f {
				ok = false
				break
			}
		}
		if ok {
			safe = append(safe, a)
		}
	}
	return safe
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func renderShellCommand(args ...string) string {
	var quoted []string
	for _, arg := range args {
		if needsShellQuoting(arg) {
			quoted = append(quoted, shellQuote(arg))
			continue
		}
		quoted = append(quoted, arg)
	}
	return stripInlineCodeBreakers(strings.Join(quoted, " "))
}

// stripInlineCodeBreakers removes characters that would break an enclosing
// Markdown inline code span. Command strings produced here are only used
// for display inside prompts (never executed), so dropping a backtick or
// control character from a rare git ref is preferable to letting
// user-controlled input escape the code span and inject text into the
// surrounding prompt.
func stripInlineCodeBreakers(s string) string {
	if !strings.ContainsAny(s, "`\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '`' || r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func needsShellQuoting(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("@%_+=:,./-~", r):
		default:
			return true
		}
	}
	return false
}

func codexCommitInspectionFallbackVariants(sha string, pathspecArgs []string) []diffSectionView {
	view := commitInspectionFallbackView{
		SHA:         sha,
		StatCmd:     renderShellCommand(append([]string{"git", "show", "--stat", "--summary", sha, "--"}, pathspecArgs...)...),
		DiffCmd:     renderShellCommand(append([]string{"git", "show", "--format=medium", "--unified=80", sha, "--"}, pathspecArgs...)...),
		FilesCmd:    renderShellCommand(append([]string{"git", "diff-tree", "--no-commit-id", "--name-only", "-r", sha, "--"}, pathspecArgs...)...),
		ShowPathCmd: renderShellCommand(append([]string{"git", "show", sha, "--"}, pathspecArgs...)...),
	}
	names := []string{"codex_commit_fallback_full", "codex_commit_fallback_medium", "codex_commit_fallback_short", "codex_commit_fallback_shortest"}
	variants := make([]diffSectionView, 0, len(names))
	for _, name := range names {
		fallback, err := renderCommitInspectionFallback(name, view)
		if err != nil {
			continue
		}
		variants = append(variants, diffSectionView{Heading: "### Diff", Fallback: fallback})
	}
	return variants
}

func codexRangeInspectionFallbackVariants(rangeRef string, pathspecArgs []string) []diffSectionView {
	view := rangeInspectionFallbackView{
		RangeRef: rangeRef,
		LogCmd:   renderShellCommand("git", "log", "--oneline", rangeRef),
		StatCmd:  renderShellCommand(append([]string{"git", "diff", "--stat", rangeRef, "--"}, pathspecArgs...)...),
		DiffCmd:  renderShellCommand(append([]string{"git", "diff", "--unified=80", rangeRef, "--"}, pathspecArgs...)...),
		FilesCmd: renderShellCommand(append([]string{"git", "diff", "--name-only", rangeRef, "--"}, pathspecArgs...)...),
		ViewCmd:  renderShellCommand(append([]string{"git", "diff", rangeRef, "--"}, pathspecArgs...)...),
	}
	names := []string{"codex_range_fallback_full", "codex_range_fallback_medium", "codex_range_fallback_short", "codex_range_fallback_shortest"}
	variants := make([]diffSectionView, 0, len(names))
	for _, name := range names {
		fallback, err := renderRangeInspectionFallback(name, view)
		if err != nil {
			continue
		}
		variants = append(variants, diffSectionView{Heading: "### Combined Diff", Fallback: fallback})
	}
	return variants
}

func selectDiffSectionVariant(variants []diffSectionView, remaining int) (diffSectionView, error) {
	if len(variants) == 0 {
		return diffSectionView{}, nil
	}
	selected := variants[len(variants)-1]
	for _, variant := range variants {
		block, err := renderDiffBlock(variant)
		if err != nil {
			return diffSectionView{}, err
		}
		if len(block) <= remaining {
			return variant, nil
		}
	}
	return truncateDiffSectionFallbackToFit(selected, remaining)
}

func truncateDiffSectionFallbackToFit(view diffSectionView, limit int) (diffSectionView, error) {
	block, err := renderDiffBlock(view)
	if err != nil || len(block) <= limit {
		return view, err
	}
	baseBlock, err := renderDiffBlock(diffSectionView{Heading: view.Heading, Body: ""})
	if err != nil {
		return diffSectionView{}, err
	}
	view.Fallback = truncateUTF8(view.Fallback, max(0, limit-len(baseBlock)))
	return view, nil
}

type rangeMetadataLoss struct {
	RemovedEntries  int
	BlankedSubject  int
	TrimmedOptional int
}

func compareRangeMetadataLoss(a, b rangeMetadataLoss) int {
	switch {
	case a.RemovedEntries != b.RemovedEntries:
		return a.RemovedEntries - b.RemovedEntries
	case a.BlankedSubject != b.BlankedSubject:
		return a.BlankedSubject - b.BlankedSubject
	default:
		return a.TrimmedOptional - b.TrimmedOptional
	}
}

func measureOptionalSectionsLoss(original, trimmed ReviewOptionalContext) int {
	loss := 0
	if len(original.PreviousAttempts) > 0 && len(trimmed.PreviousAttempts) == 0 {
		loss++
	}
	if len(original.InRangeReviews) > 0 && len(trimmed.InRangeReviews) == 0 {
		loss++
	}
	if len(original.PreviousReviews) > 0 && len(trimmed.PreviousReviews) == 0 {
		loss++
	}
	if original.AdditionalContext != "" && trimmed.AdditionalContext == "" {
		loss++
	}
	if original.ProjectGuidelines != nil && trimmed.ProjectGuidelines == nil {
		loss++
	}
	return loss
}

func measureRangeMetadataLoss(original, trimmed TemplateContext) rangeMetadataLoss {
	if original.Review == nil || trimmed.Review == nil || original.Review.Subject.Range == nil || trimmed.Review.Subject.Range == nil {
		return rangeMetadataLoss{}
	}
	loss := rangeMetadataLoss{
		RemovedEntries:  len(original.Review.Subject.Range.Entries) - len(trimmed.Review.Subject.Range.Entries),
		TrimmedOptional: measureOptionalSectionsLoss(original.Review.Optional, trimmed.Review.Optional),
	}
	for i := range trimmed.Review.Subject.Range.Entries {
		if i >= len(original.Review.Subject.Range.Entries) {
			break
		}
		if original.Review.Subject.Range.Entries[i].Subject != "" && trimmed.Review.Subject.Range.Entries[i].Subject == "" {
			loss.BlankedSubject++
		}
	}
	return loss
}

func selectRichestRangePromptView(limit int, view TemplateContext, variants []diffSectionView) (TemplateContext, error) {
	fallback := view.Clone()
	if len(variants) > 0 && fallback.Review != nil {
		fallback.Review.Diff = DiffContext{Heading: variants[len(variants)-1].Heading, Body: variants[len(variants)-1].Body}
		fallback.Review.Fallback = fallbackContextFromDiffSection(variants[len(variants)-1])
	}
	var (
		best     TemplateContext
		bestLoss rangeMetadataLoss
		haveBest bool
	)
	for _, variant := range variants {
		candidate := view.Clone()
		if candidate.Review != nil {
			candidate.Review.Diff = DiffContext{Heading: variant.Heading, Body: variant.Body}
			candidate.Review.Fallback = fallbackContextFromDiffSection(variant)
		}
		trimmed, body, err := trimRangePromptContext(limit, candidate)
		if err != nil {
			return TemplateContext{}, err
		}
		fallback = trimmed
		if len(body) > limit {
			continue
		}
		loss := measureRangeMetadataLoss(view, trimmed)
		if !haveBest || compareRangeMetadataLoss(loss, bestLoss) < 0 {
			best = trimmed
			bestLoss = loss
			haveBest = true
		}
	}
	if haveBest {
		return best, nil
	}
	return fallback, nil
}

// buildSinglePrompt constructs a prompt for a single commit
func (b *Builder) buildSinglePrompt(sha string, contextCount int, agentName, reviewType string, opts buildOpts) (string, error) {
	ctx := b.newPromptBuildContext(agentName, reviewType, opts.minSeverity, "review", defaultOptionalSections(b.repoPath, opts.additionalContext))

	// Get previous reviews if requested
	if contextCount > 0 && b.db != nil {
		contexts, err := b.getPreviousReviewContexts(sha, contextCount)
		if err == nil && len(contexts) > 0 {
			ctx.optional.PreviousReviews = orderedPreviousReviewViews(contexts)
		}
	}

	// Include previous review attempts for this same commit (for re-reviews)
	ctx.optional.PreviousAttempts = previousAttemptViewsFromContexts(b.previousAttemptContexts(sha))

	// Current commit section
	shortSHA := git.ShortSHA(sha)

	// Get commit info
	info, err := git.GetCommitInfo(b.repoPath, sha)
	if err != nil {
		return "", fmt.Errorf("get commit info: %w", err)
	}

	currentView := currentCommitSectionView{
		Commit:  shortSHA,
		Subject: escapeXML(info.Subject),
		Author:  escapeXML(info.Author),
		Message: escapeXML(info.Body),
	}
	currentRequired, err := renderCurrentCommitRequired(currentView)
	if err != nil {
		return "", err
	}
	currentOverflow, err := renderCurrentCommitOverflow(currentView)
	if err != nil {
		return "", err
	}
	emptyInlineDiff, err := renderInlineDiff("")
	if err != nil {
		return "", err
	}
	emptyDiffBlock, err := renderDiffBlock(diffSectionView{Heading: "### Diff", Body: emptyInlineDiff})
	if err != nil {
		return "", err
	}

	excludes := b.resolveExcludes(reviewType)
	bodyLimit := max(0, ctx.promptCap-len(ctx.requiredPrefix))
	diffLimit := max(0, bodyLimit-len(currentRequired)-len(currentOverflow)-len(emptyDiffBlock))
	diff, truncated, err := git.GetDiffLimited(b.repoPath, sha, diffLimit, excludes...)
	if err != nil {
		return "", fmt.Errorf("get diff: %w", err)
	}

	diffView := diffSectionView{Heading: "### Diff"}
	if truncated {
		if opts.diffFilePath != "" || opts.requireDiffFile {
			if opts.diffFilePath == "" && opts.requireDiffFile {
				return "", ErrDiffTruncatedNoFile
			}
			optionalPrefix, err := renderOptionalSectionsPrefix(ctx.optional)
			if err != nil {
				return "", err
			}
			return buildPromptPreservingCurrentSection(ctx.requiredPrefix, optionalPrefix, currentRequired, currentOverflow, ctx.promptCap, diffFileFallbackVariants("### Diff", opts.diffFilePath)...), nil
		}
		pathspecArgs := safeForMarkdown(git.FormatExcludeArgs(excludes))
		if isCodexReviewAgent(agentName) {
			variants := codexCommitInspectionFallbackVariants(sha, pathspecArgs)
			shortestBlock, err := renderDiffBlock(variants[len(variants)-1])
			if err != nil {
				return "", err
			}
			optionalPrefix, err := renderOptionalSectionsPrefix(ctx.optional)
			if err != nil {
				return "", err
			}
			softBudget := max(0, bodyLimit-len(currentRequired)-len(shortestBlock))
			softLen := len(optionalPrefix) + len(currentOverflow)
			effectiveSoftLen := min(softLen, softBudget)
			remaining := max(0, bodyLimit-len(currentRequired)-effectiveSoftLen)
			diffView, err = selectDiffSectionVariant(variants, remaining)
			if err != nil {
				return "", err
			}
		} else {
			fallback, err := renderGenericCommitFallback(renderShellCommand("git", "show", sha))
			if err != nil {
				return "", err
			}
			diffView.Fallback = fallback
		}
	} else {
		inlineDiff, err := renderInlineDiff(diff)
		if err != nil {
			return "", err
		}
		diffView.Body = inlineDiff
	}

	body, err := fitSinglePromptContext(
		bodyLimit,
		templateContextFromSingleView(singlePromptView{
			Optional: ctx.optional,
			Current:  currentView,
			Diff:     diffView,
		}),
	)
	if err != nil {
		return "", err
	}
	return ctx.requiredPrefix + body, nil
}

// buildRangePrompt constructs a prompt for a commit range
func (b *Builder) buildRangePrompt(rangeRef string, contextCount int, agentName, reviewType string, opts buildOpts) (string, error) {
	ctx := b.newPromptBuildContext(agentName, reviewType, opts.minSeverity, "range", defaultOptionalSections(b.repoPath, opts.additionalContext))

	// Get previous reviews from before the range start
	if contextCount > 0 && b.db != nil {
		startSHA, err := git.GetRangeStart(b.repoPath, rangeRef)
		if err == nil {
			contexts, err := b.getPreviousReviewContexts(startSHA, contextCount)
			if err == nil && len(contexts) > 0 {
				ctx.optional.PreviousReviews = orderedPreviousReviewViews(contexts)
			}
		}
	}

	// Include previous review attempts for this same range (for re-reviews)
	ctx.optional.PreviousAttempts = previousAttemptViewsFromContexts(b.previousAttemptContexts(rangeRef))

	// Get commits in range
	commits, err := git.GetRangeCommits(b.repoPath, rangeRef)
	if err != nil {
		return "", fmt.Errorf("get range commits: %w", err)
	}

	// Include per-commit reviews for commits inside the range so the agent
	// can avoid re-raising issues that were already surfaced.
	ctx.optional.InRangeReviews = inRangeReviewViews(b.lookupReviewContexts(commits, true))

	entries := make([]commitRangeEntryView, 0, len(commits))
	for _, commitSHA := range commits {
		short := git.ShortSHA(commitSHA)
		info, err := git.GetCommitInfo(b.repoPath, commitSHA)
		if err == nil {
			entries = append(entries, commitRangeEntryView{Commit: short, Subject: escapeXML(info.Subject)})
			continue
		}
		entries = append(entries, commitRangeEntryView{Commit: short})
	}
	currentView := commitRangeSectionView{Count: len(commits), Entries: entries}
	currentRequiredText, err := renderCommitRangeRequired(currentView)
	if err != nil {
		return "", err
	}
	currentOverflowText, err := renderCommitRangeOverflow(currentView)
	if err != nil {
		return "", err
	}
	emptyInlineDiff, err := renderInlineDiff("")
	if err != nil {
		return "", err
	}
	emptyDiffBlock, err := renderDiffBlock(diffSectionView{Heading: "### Combined Diff", Body: emptyInlineDiff})
	if err != nil {
		return "", err
	}

	excludes := b.resolveExcludes(reviewType)
	bodyLimit := max(0, ctx.promptCap-len(ctx.requiredPrefix))
	diffLimit := max(0, bodyLimit-len(currentRequiredText)-len(currentOverflowText)-len(emptyDiffBlock))
	diff, truncated, err := git.GetRangeDiffLimited(b.repoPath, rangeRef, diffLimit, excludes...)
	if err != nil {
		return "", fmt.Errorf("get range diff: %w", err)
	}

	diffView := diffSectionView{Heading: "### Combined Diff"}
	if truncated {
		if opts.diffFilePath != "" || opts.requireDiffFile {
			if opts.diffFilePath == "" && opts.requireDiffFile {
				return "", ErrDiffTruncatedNoFile
			}
			optionalPrefix, err := renderOptionalSectionsPrefix(ctx.optional)
			if err != nil {
				return "", err
			}
			return buildPromptPreservingCurrentSection(ctx.requiredPrefix, optionalPrefix, currentRequiredText, currentOverflowText, ctx.promptCap, diffFileFallbackVariants("### Combined Diff", opts.diffFilePath)...), nil
		}
		pathspecArgs := safeForMarkdown(git.FormatExcludeArgs(excludes))
		if isCodexReviewAgent(agentName) {
			variants := codexRangeInspectionFallbackVariants(rangeRef, pathspecArgs)
			selectedCtx, err := selectRichestRangePromptView(bodyLimit, templateContextFromRangeView(rangePromptView{
				Optional: ctx.optional,
				Current:  currentView,
			}), variants)
			if err != nil {
				return "", err
			}
			if selectedCtx.Review != nil {
				ctx.optional = selectedCtx.Review.Optional.Clone()
				if selectedCtx.Review.Subject.Range != nil {
					entries := make([]commitRangeEntryView, 0, len(selectedCtx.Review.Subject.Range.Entries))
					for _, entry := range selectedCtx.Review.Subject.Range.Entries {
						entries = append(entries, commitRangeEntryView(entry))
					}
					currentView = commitRangeSectionView{Count: selectedCtx.Review.Subject.Range.Count, Entries: entries}
				}
				diffView = diffSectionView{Heading: selectedCtx.Review.Diff.Heading, Body: selectedCtx.Review.Diff.Body, Fallback: selectedCtx.Review.Fallback.Rendered()}
			}
		} else {
			fallback, err := renderGenericRangeFallback(renderShellCommand("git", "diff", rangeRef))
			if err != nil {
				return "", err
			}
			diffView.Fallback = fallback
		}
	} else {
		inlineDiff, err := renderInlineDiff(diff)
		if err != nil {
			return "", err
		}
		diffView.Body = inlineDiff
	}

	body, err := fitRangePromptContext(
		bodyLimit,
		templateContextFromRangeView(rangePromptView{
			Optional: ctx.optional,
			Current:  currentView,
			Diff:     diffView,
		}),
	)
	if err != nil {
		return "", err
	}
	return ctx.requiredPrefix + body, nil
}

func buildProjectGuidelinesSectionView(guidelines string) *markdownSectionView {
	trimmed := strings.TrimSpace(guidelines)
	if trimmed == "" {
		return nil
	}
	return &markdownSectionView{
		Heading: "## Project Guidelines",
		Body:    trimmed,
	}
}

func buildAdditionalContextSection(additionalContext string) string {
	trimmed := strings.TrimSpace(additionalContext)
	if trimmed == "" {
		return ""
	}
	return trimmed + "\n\n"
}

func orderedPreviousReviewViews(contexts []HistoricalReviewContext) []previousReviewView {
	ordered := make([]HistoricalReviewContext, 0, len(contexts))
	for _, v := range slices.Backward(contexts) {
		ordered = append(ordered, v)
	}
	return previousReviewViews(ordered)
}

// LoadGuidelines loads review guidelines from the repo's default
// branch, falling back to filesystem config when the default branch
// has no .roborev.toml.
func LoadGuidelines(repoPath string) string {
	// Load review guidelines from the default branch (origin/main,
	// origin/master, etc.). Branch-specific guidelines are intentionally
	// ignored to prevent prompt injection from untrusted PR authors.
	if defaultBranch, err := git.GetDefaultBranch(repoPath); err == nil {
		cfg, err := config.LoadRepoConfigFromRef(repoPath, defaultBranch)
		if err != nil {
			if config.IsConfigParseError(err) {
				log.Printf("prompt: invalid .roborev.toml on %s: %v",
					defaultBranch, err)
				return ""
			}
			log.Printf("prompt: failed to read .roborev.toml from %s: %v"+
				" (will try filesystem)", defaultBranch, err)
		} else if cfg != nil {
			return cfg.ReviewGuidelines
		}
	}

	// Fall back to filesystem config when default branch has no config
	// (e.g., no remote, or .roborev.toml not yet committed).
	if fsCfg, err := config.LoadRepoConfig(repoPath); err == nil && fsCfg != nil {
		return fsCfg.ReviewGuidelines
	}
	return ""
}

func (b *Builder) previousAttemptContexts(gitRef string) []reviewAttemptContext {
	if b.db == nil {
		return nil
	}

	reviews, err := b.db.GetAllReviewsForGitRef(gitRef)
	if err != nil || len(reviews) == 0 {
		return nil
	}

	attempts := make([]reviewAttemptContext, 0, len(reviews))
	for _, review := range reviews {
		attempt := reviewAttemptContext{Review: review}
		if review.JobID > 0 {
			responses, err := b.db.GetCommentsForJob(review.JobID)
			if err == nil {
				attempt.Responses = responses
			}
		}
		attempts = append(attempts, attempt)
	}
	return attempts
}

// getPreviousReviewContexts gets the N commits before the target and looks up their reviews and responses
func (b *Builder) getPreviousReviewContexts(sha string, count int) ([]HistoricalReviewContext, error) {
	// Get parent commits from git
	parentSHAs, err := git.GetParentCommits(b.repoPath, sha, count)
	if err != nil {
		return nil, fmt.Errorf("get parent commits: %w", err)
	}
	return b.lookupReviewContexts(parentSHAs, false), nil
}

// lookupReviewContexts looks up reviews and responses for each SHA.
// When skipMissing is true, SHAs with no stored review are omitted from the
// result; otherwise a placeholder context (Review nil) is returned for them.
func (b *Builder) lookupReviewContexts(shas []string, skipMissing bool) []HistoricalReviewContext {
	if b.db == nil {
		return nil
	}
	var contexts []HistoricalReviewContext
	for _, sha := range shas {
		review, err := b.db.GetReviewByCommitSHA(sha)
		if err != nil {
			if skipMissing {
				continue
			}
			contexts = append(contexts, HistoricalReviewContext{SHA: sha})
			continue
		}
		ctx := HistoricalReviewContext{SHA: sha, Review: review}
		if review.JobID > 0 {
			if responses, err := b.db.GetCommentsForJob(review.JobID); err == nil {
				ctx.Responses = responses
			}
		}
		contexts = append(contexts, ctx)
	}
	return contexts
}

// BuildSimple constructs a simpler prompt without database context
func BuildSimple(repoPath, sha, agentName string) (string, error) {
	b := NewBuilder(nil).ForRepo(repoPath, 0)
	return b.Build(sha, 0, agentName, "", "")
}

const PreviousAttemptsHeader = `
## Previous Addressing Attempts

The following are previous attempts to address this or related reviews.
Learn from these to avoid repeating approaches that didn't fully resolve the issues.
Be pragmatic - if previous attempts were rejected for being too minor, make more substantive fixes.
If they were rejected for being over-engineered, keep it simpler.
`

const UserCommentsHeader = `## User Comments

The following comments were left by the developer on this review.
Take them into account when applying fixes — they may flag false
positives, provide additional context, or request specific approaches.

`

func IsToolResponse(r storage.Response) bool {
	return strings.HasPrefix(r.Responder, "roborev-")
}

func SplitResponses(responses []storage.Response) (toolAttempts, userComments []storage.Response) {
	for _, r := range responses {
		if IsToolResponse(r) {
			toolAttempts = append(toolAttempts, r)
		} else {
			userComments = append(userComments, r)
		}
	}
	return
}

func FormatToolAttempts(attempts []storage.Response) string {
	if len(attempts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(PreviousAttemptsHeader)
	sb.WriteString("\n")
	for _, attempt := range attempts {
		fmt.Fprintf(&sb, "--- Attempt by %s at %s ---\n", attempt.Responder, attempt.CreatedAt.Format("2006-01-02 15:04"))
		sb.WriteString(attempt.Response)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

func FormatUserComments(comments []storage.Response) string {
	if len(comments) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(UserCommentsHeader)
	for _, c := range comments {
		fmt.Fprintf(&sb, "**%s** (%s):\n%s\n\n", c.Responder, c.CreatedAt.Format("2006-01-02 15:04"), c.Response)
	}
	return sb.String()
}

// BuildAddressPrompt constructs a prompt for addressing review findings.
// When minSeverity is non-empty, a severity filtering instruction is
// injected before the findings section.
func (b *Builder) BuildAddressPrompt(review *storage.Review, previousAttempts []storage.Response, minSeverity string) (string, error) {
	view := addressPromptView{
		SeverityFilter: config.SeverityInstruction(minSeverity),
		ReviewFindings: review.Output,
		JobID:          review.JobID,
	}

	if repoCfg, err := config.LoadRepoConfig(b.repoPath); err == nil && repoCfg != nil {
		view.ProjectGuidelines = buildProjectGuidelinesSectionView(repoCfg.ReviewGuidelines)
	}

	if len(previousAttempts) > 0 {
		toolAttempts, userComments := SplitResponses(previousAttempts)
		if len(toolAttempts) > 0 {
			view.ToolAttempts = make([]addressAttemptView, 0, len(toolAttempts))
			for _, attempt := range toolAttempts {
				when := ""
				if !attempt.CreatedAt.IsZero() {
					when = attempt.CreatedAt.Format("2006-01-02 15:04")
				}
				view.ToolAttempts = append(view.ToolAttempts, addressAttemptView{
					Responder: attempt.Responder,
					Response:  attempt.Response,
					When:      when,
				})
			}
		}
		if len(userComments) > 0 {
			view.UserComments = make([]addressAttemptView, 0, len(userComments))
			for _, comment := range userComments {
				when := ""
				if !comment.CreatedAt.IsZero() {
					when = comment.CreatedAt.Format("2006-01-02 15:04")
				}
				view.UserComments = append(view.UserComments, addressAttemptView{
					Responder: comment.Responder,
					Response:  comment.Response,
					When:      when,
				})
			}
		}
	}

	if review.Job != nil && review.Job.GitRef != "" && review.Job.GitRef != "dirty" {
		diff, err := git.GetDiff(b.repoPath, review.Job.GitRef)
		if err == nil && len(diff) > 0 && len(diff) < MaxPromptSize/2 {
			view.OriginalDiff = diff
			if !strings.HasSuffix(view.OriginalDiff, "\n") {
				view.OriginalDiff += "\n"
			}
		}
	}

	body, err := renderAddressPromptFromSections(view)
	if err != nil {
		return "", err
	}
	return GetSystemPrompt(review.Agent, "address") + "\n" + body, nil
}
