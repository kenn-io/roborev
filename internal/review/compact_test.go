package review

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func compactFixture(t *testing.T) (string, CompactManifest) {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "guard.go"), []byte("package fixture\n// access is unchecked\n"), 0o600))
	return repo, CompactManifest{Version: 1, Sources: []CompactSource{{
		JobID: 10, ReviewUUID: uuid.New(), GitRef: "HEAD", SourceLabels: []string{"original"},
		Findings: []structuredreview.Finding{
			{Severity: "high", Problem: "Cross-tenant access", Fix: "Reject foreign tenants and preserve own access", File: "guard.go", StartLine: 2, EndLine: 2, Sources: []int{1}},
			{Severity: "low", Problem: "Missing cancellation", Fix: "Honor caller cancellation", File: "guard.go", StartLine: 2, EndLine: 2},
		},
	}}}
}

func compactEvidence() []CompactEvidence {
	return []CompactEvidence{{File: "guard.go", StartLine: 2, EndLine: 2, Quote: "// access is unchecked"}}
}

func TestCompactMissingDecisionsPreserveOriginals(t *testing.T) {
	repo, manifest := compactFixture(t)
	got, eligible, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1})
	require.NoError(t, err)
	want := append([]structuredreview.Finding(nil), manifest.Sources[0].Findings...)
	want[0].Sources = nil // attribution belongs to the historical audit
	assert.Equal(t, want, got.Structured.Findings)
	assert.Empty(t, eligible)
	assert.Contains(t, got.Structured.Summary, manifest.Sources[0].ReviewUUID.String())
}

func TestCompactZeroFindingSourceIsEligible(t *testing.T) {
	source := CompactSource{JobID: 17, ReviewUUID: uuid.New(), GitRef: "HEAD", Findings: []structuredreview.Finding{}}
	manifest := CompactManifest{Version: 1, Sources: []CompactSource{source}}
	prompt, err := CompactManifestPrompt(manifest)
	require.NoError(t, err)
	require.NoError(t, ValidateCompactManifest(prompt, manifest))
	got, eligible, err := reconcileCompact(t.Context(), "", manifest, compactResponse{Version: 1})
	require.NoError(t, err)
	assert.Equal(t, []CompactSource{source}, eligible)
	assert.NotNil(t, got.Structured.Findings)
	assert.Empty(t, got.Structured.Findings)
}

func TestCompactEvidenceDispositions(t *testing.T) {
	for _, status := range []string{"valid", "fixed", "false_positive", "unresolved", "invented"} {
		t.Run(status, func(t *testing.T) {
			repo, manifest := compactFixture(t)
			manifest.Sources[0].Findings = manifest.Sources[0].Findings[:1]
			got, eligible, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{{ID: "F1", Status: status, Reason: "The unchecked access permits the original cross-tenant request", Evidence: compactEvidence()}}})
			require.NoError(t, err)
			if status == "false_positive" {
				assert.Empty(t, got.Structured.Findings)
			} else {
				require.Len(t, got.Structured.Findings, 1)
				assert.Equal(t, manifest.Sources[0].Findings[0].Problem, got.Structured.Findings[0].Problem)
			}
			if status == "valid" || status == "false_positive" {
				assert.Equal(t, []CompactSource{manifest.Sources[0]}, eligible)
			} else {
				assert.Empty(t, eligible)
			}
		})
	}
}

func TestCompactSourceWithOneUnresolvedFindingIsIneligible(t *testing.T) {
	repo, manifest := compactFixture(t)
	got, eligible, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{{
		ID: "F1", Status: "valid", Reason: "The unchecked access remains reachable", Evidence: compactEvidence(),
	}}})
	require.NoError(t, err)
	require.Len(t, got.Structured.Findings, 2)
	assert.Equal(t, manifest.Sources[0].Findings[0].Problem, got.Structured.Findings[0].Problem)
	assert.Equal(t, manifest.Sources[0].Findings[1].Problem, got.Structured.Findings[1].Problem)
	assert.Empty(t, eligible)
}

func TestCompactBadAccountingNeverDropsOriginals(t *testing.T) {
	for _, decisions := range [][]CompactDecision{
		{{ID: "F1", Status: "false_positive", Reason: "reason", Evidence: compactEvidence()}, {ID: "F1", Status: "valid", Reason: "reason", Evidence: compactEvidence()}},
		{{ID: "unknown", Status: "false_positive", Reason: "reason", Evidence: compactEvidence()}},
		{{ID: "F1", Status: "duplicate", Target: "F1", Reason: "same"}},
		{{ID: "F1", Status: "duplicate", Target: "F2", Reason: "same"}, {ID: "F2", Status: "duplicate", Target: "F1", Reason: "same"}},
		{{ID: "F1", Status: "duplicate", Target: "F2", Reason: "same location"}, {ID: "F2", Status: "valid", Reason: "still unchecked", Evidence: compactEvidence()}},
	} {
		repo, manifest := compactFixture(t)
		got, eligible, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1, Decisions: decisions})
		require.NoError(t, err)
		require.Len(t, got.Structured.Findings, 2)
		assert.Equal(t, "Cross-tenant access", got.Structured.Findings[0].Problem)
		assert.Equal(t, "Missing cancellation", got.Structured.Findings[1].Problem)
		assert.Empty(t, eligible)
	}
}

func TestCompactDuplicatePromotesSeverity(t *testing.T) {
	repo, manifest := compactFixture(t)
	manifest.Sources[0].Findings[1] = manifest.Sources[0].Findings[0]
	manifest.Sources[0].Findings[1].Severity = "low"
	got, eligible, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{
		{ID: "F1", Status: "duplicate", Target: "F2", Reason: "Identical behavior and preservation obligations"},
		{ID: "F2", Status: "valid", Reason: "Unchecked access still permits foreign requests", Evidence: compactEvidence()},
	}})
	require.NoError(t, err)
	require.Len(t, got.Structured.Findings, 1)
	assert.Equal(t, "high", got.Structured.Findings[0].Severity)
	assert.Len(t, eligible, 1)
}

func TestCompactUntrustedEvidenceRetainsFinding(t *testing.T) {
	for _, evidence := range []CompactEvidence{
		{File: "../guard.go", StartLine: 1, EndLine: 1, Quote: "package fixture"},
		{File: "/guard.go", StartLine: 1, EndLine: 1, Quote: "package fixture"},
		{File: "guard.go", StartLine: 0, EndLine: 1, Quote: "package fixture"},
		{File: "guard.go", StartLine: 2, EndLine: 9, Quote: "unchecked"},
		{File: "guard.go", StartLine: 1, EndLine: 1, Quote: "not actually present"},
	} {
		repo, manifest := compactFixture(t)
		got, eligible, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{{ID: "F1", Status: "false_positive", Reason: "claimed disproof", Evidence: []CompactEvidence{evidence}}}})
		require.NoError(t, err)
		assert.Len(t, got.Structured.Findings, 2)
		assert.Empty(t, eligible)
	}
	repo, manifest := compactFixture(t)
	evidence := append(compactEvidence(), CompactEvidence{File: "guard.go", StartLine: 2, EndLine: 2, Quote: "not present"})
	got, eligible, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{{ID: "F1", Status: "false_positive", Reason: "claimed disproof", Evidence: evidence}}})
	require.NoError(t, err)
	require.Len(t, got.Structured.Findings, 2)
	assert.Empty(t, eligible)
}

// Every incomplete decision set must preserve each canonical original's
// reviewable fields and multiplicity. Attribution remains in the historical
// audit and is intentionally removed from active findings.
func TestCompactPreservationProperty(t *testing.T) {
	repo, fixture := compactFixture(t)
	ctx := t.Context()
	property := func(problem, fix string) bool {
		raw, err := json.Marshal(structuredreview.Document{
			SchemaVersion: 3,
			Summary:       "Preserve the original finding exactly",
			Verdict:       "fail",
			SourceLabels:  []string{"source review"},
			Findings: []structuredreview.Finding{{
				Severity: "high",
				Problem:  "Original behavior: {" + problem + "}",
				Fix:      "Expected behavior: {" + fix + "}",
				Sources:  []int{1},
			}},
		})
		require.NoError(t, err)
		canonical, err := structuredreview.Decode(raw)
		require.NoError(t, err)
		finding := canonical.Findings[0]
		firstSource := fixture.Sources[0]
		firstSource.SourceLabels = canonical.SourceLabels
		firstSource.Findings = []structuredreview.Finding{finding}
		secondSource := firstSource
		secondSource.JobID++
		secondSource.ReviewUUID = uuid.New()
		secondSource.SourceLabels = []string{"independent source review"}
		secondFinding := finding
		secondFinding.Sources = []int{1}
		secondSource.Findings = []structuredreview.Finding{secondFinding}
		manifest := CompactManifest{Version: 1, Sources: []CompactSource{firstSource, secondSource}}
		got, eligible, err := reconcileCompact(ctx, repo, manifest, compactResponse{Version: 1})
		require.NoError(t, err)
		firstActive := finding
		firstActive.Sources = nil
		secondActive := secondFinding
		secondActive.Sources = nil
		wantActive := []structuredreview.Finding{firstActive, secondActive}
		assert.Equal(t, wantActive, got.Structured.Findings)
		assert.Empty(t, eligible)
		decoded, err := structuredreview.Decode(got.StructuredOutput)
		require.NoError(t, err)
		assert.Equal(t, strings.TrimSpace(got.Structured.Summary), decoded.Summary)
		assert.Equal(t, got.Structured.Verdict, decoded.Verdict)
		assert.Equal(t, wantActive, decoded.Findings)
		for i, want := range []struct {
			source  CompactSource
			finding structuredreview.Finding
		}{{firstSource, finding}, {secondSource, secondFinding}} {
			marker := "### F" + strconv.Itoa(i+1) + " — unresolved\n\n```json\n"
			_, auditTail, ok := strings.Cut(got.Structured.Summary, marker)
			require.True(t, ok)
			auditJSON, _, ok := strings.Cut(auditTail, "\n```\n")
			require.True(t, ok)
			var audit compactOriginal
			require.NoError(t, json.Unmarshal([]byte(auditJSON), &audit))
			want.source.Findings = []structuredreview.Finding{}
			assert.Equal(t, want.source, audit.Source)
			assert.Equal(t, "F"+strconv.Itoa(i+1), audit.ID)
			assert.Equal(t, 1, audit.Ordinal)
			assert.Equal(t, want.finding, audit.Finding)
			assert.Equal(t, "unresolved", audit.Decision.Status)
		}
		return true
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

// Eligibility is determined independently for each source, while valid and
// unresolved findings both remain in the active result.
func TestCompactEligibilityIsPerSourceProperty(t *testing.T) {
	repo, fixture := compactFixture(t)
	first := fixture.Sources[0]
	first.Findings = []structuredreview.Finding{fixture.Sources[0].Findings[0]}
	second := first
	second.JobID++
	second.ReviewUUID = uuid.New()
	second.SourceLabels = []string{"another source"}
	manifest := CompactManifest{Version: 1, Sources: []CompactSource{first, second}}
	property := func(reason string) bool {
		got, eligible, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{{
			ID: "F1", Status: "valid", Reason: "The behavior remains reachable: " + reason, Evidence: compactEvidence(),
		}}})
		require.NoError(t, err)
		require.Len(t, got.Structured.Findings, 2)
		assert.Equal(t, first.Findings[0].Problem, got.Structured.Findings[0].Problem)
		assert.Equal(t, first.Findings[0].Problem, got.Structured.Findings[1].Problem)
		require.Len(t, eligible, 1)
		assert.Equal(t, first.JobID, eligible[0].JobID)
		return true
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

func TestCompactChainedDuplicatesRetainEveryOriginal(t *testing.T) {
	repo, manifest := compactFixture(t)
	f := manifest.Sources[0].Findings[0]
	manifest.Sources[0].Findings = []structuredreview.Finding{f, f, f}
	got, eligible, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{
		{ID: "F1", Status: "duplicate", Target: "F2", Reason: "identical"},
		{ID: "F2", Status: "duplicate", Target: "F3", Reason: "identical"},
		{ID: "F3", Status: "unresolved", Reason: "not enough evidence"},
	}})
	require.NoError(t, err)
	// F1 cannot target the duplicate F2. F2 may directly target F3, but F1
	// stays present even when F2 becomes a survivor in a later pass.
	require.Len(t, got.Structured.Findings, 2)
	assert.Empty(t, eligible)
}

// Duplicate classification requires every documented obligation field to
// match. Severity remains outside that comparison and is promoted separately.
func TestCompactDuplicateEqualityRequiresMatchingObligationsProperty(t *testing.T) {
	property := func(text string) bool {
		original := structuredreview.Finding{
			Severity: "high", Problem: "Problem: {" + text + "}", Fix: "Expected behavior: {" + text + "}",
			Kind: structuredreview.KindDefect, Topic: structuredreview.TopicSecurity,
			File: "guard.go", StartLine: 2, EndLine: 3, Location: "guard.go lines 2-3",
		}
		assert.True(t, compactSameObligations(original, original))
		for _, tc := range []struct {
			name   string
			mutate func(*structuredreview.Finding)
		}{
			{"problem", func(f *structuredreview.Finding) { f.Problem += " changed" }},
			{"fix", func(f *structuredreview.Finding) { f.Fix += " changed" }},
			{"kind", func(f *structuredreview.Finding) { f.Kind = structuredreview.KindImprovement }},
			{"topic", func(f *structuredreview.Finding) { f.Topic = structuredreview.TopicDocumentation }},
			{"file", func(f *structuredreview.Finding) { f.File = "other.go" }},
			{"start_line", func(f *structuredreview.Finding) { f.StartLine++ }},
			{"end_line", func(f *structuredreview.Finding) { f.EndLine++ }},
			{"location", func(f *structuredreview.Finding) { f.Location += " changed" }},
		} {
			candidate := original
			tc.mutate(&candidate)
			assert.False(t, compactSameObligations(original, candidate), tc.name)
		}
		return true
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

func TestCompactSymlinkEvidenceCannotEscape(t *testing.T) {
	repo, manifest := compactFixture(t)
	external := filepath.Join(t.TempDir(), "outside.go")
	require.NoError(t, os.WriteFile(external, []byte("disproving guard\n"), 0o600))
	require.NoError(t, os.Symlink(external, filepath.Join(repo, "link.go")))
	got, eligible, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{{ID: "F1", Status: "false_positive", Reason: "external guard", Evidence: []CompactEvidence{{File: "link.go", StartLine: 1, EndLine: 1, Quote: "disproving guard"}}}}})
	require.NoError(t, err)
	assert.Len(t, got.Structured.Findings, 2)
	assert.Empty(t, eligible)
}

func TestCompactBindingProperty(t *testing.T) {
	_, fixture := compactFixture(t)
	property := func(ref string) bool {
		manifest := fixture
		manifest.Sources = append([]CompactSource(nil), fixture.Sources...)
		manifest.Sources[0].GitRef = ref
		prompt, err := CompactManifestPrompt(manifest)
		require.NoError(t, err)
		require.NoError(t, ValidateCompactManifest(prompt, manifest))
		manifest.Sources[0].GitRef = ref + " changed"
		require.Error(t, ValidateCompactManifest(prompt, manifest))
		return true
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

func TestCompactAuditRetainsRejectedDecisionEvidence(t *testing.T) {
	repo, manifest := compactFixture(t)
	got, _, err := reconcileCompact(t.Context(), repo, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{
		{ID: "F1", Status: "fixed", Reason: "first attempted explanation", Evidence: compactEvidence()},
		{ID: "F1", Status: "false_positive", Reason: "conflicting explanation", Evidence: compactEvidence()},
		{ID: "F999", Status: "valid", Reason: "unknown original", Evidence: compactEvidence()},
	}})
	require.NoError(t, err)
	assert.Contains(t, got.Structured.Summary, "first attempted explanation")
	assert.Contains(t, got.Structured.Summary, "conflicting explanation")
	assert.Contains(t, got.Structured.Summary, "F999")
}

func TestCompactFixedRequiresSourceToChangeHistory(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	original := repo.CommitFile("guard.go", "package fixture\n// unchecked\n", "feat: vulnerable fixture")
	repaired := repo.CommitFile("guard.go", "package fixture\n// tenant checked\n", "fix: check tenant")
	_, manifest := compactFixture(t)
	manifest.Sources[0].GitRef = original
	manifest.Sources[0].Findings = manifest.Sources[0].Findings[:1]
	d := CompactDecision{ID: "F1", Status: "fixed", Reason: "The tenant check rejects other owners while retaining own access", Change: repaired, Evidence: []CompactEvidence{{File: "guard.go", StartLine: 2, EndLine: 2, Quote: "// tenant checked"}}}
	got, eligible, err := reconcileCompact(t.Context(), repo.Root, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{d}})
	require.NoError(t, err)
	assert.Empty(t, got.Structured.Findings)
	assert.Len(t, eligible, 1)
	manifest.Sources[0].GitRef = repaired
	got, eligible, err = reconcileCompact(t.Context(), repo.Root, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{d}})
	require.NoError(t, err)
	assert.Len(t, got.Structured.Findings, 1)
	assert.Empty(t, eligible)
}

func TestCompactFixedRejectsExcludedChangePath(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	source := repo.CommitFile("guard.go", "package fixture\n// unchecked\n", "feat: source")
	change := repo.CommitFile("guard.go", "package fixture\n// tenant checked\n", "fix: tenant guard")
	_, manifest := compactFixture(t)
	manifest.Sources[0].GitRef = source
	manifest.Sources[0].Findings = manifest.Sources[0].Findings[:1]
	decision := CompactDecision{ID: "F1", Status: "fixed", Reason: "The tenant check rejects foreign requests and preserves own access", Change: change, Evidence: []CompactEvidence{{File: "guard.go", StartLine: 2, EndLine: 2, Quote: "// tenant checked"}}}
	got, eligible, err := reconcileCompact(t.Context(), repo.Root, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{decision}}, "guard.go")
	require.NoError(t, err)
	require.Len(t, got.Structured.Findings, 1)
	assert.Equal(t, "Cross-tenant access", got.Structured.Findings[0].Problem)
	assert.Empty(t, eligible)
}

func TestCompactFixedRecognizesFirstParentMergeRepair(t *testing.T) {
	repo, source, _, merge := compactMergedRepairRepo(t)
	_, manifest := compactFixture(t)
	manifest.Sources[0].GitRef = source
	manifest.Sources[0].Findings = manifest.Sources[0].Findings[:1]
	decision := CompactDecision{ID: "F1", Status: "fixed", Reason: "The merged tenant check rejects foreign requests and preserves own access", Change: merge, Evidence: []CompactEvidence{{File: "guard.go", StartLine: 2, EndLine: 2, Quote: "// tenant checked"}}}
	got, eligible, err := reconcileCompact(t.Context(), repo.Root, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{decision}})
	require.NoError(t, err)
	assert.Empty(t, got.Structured.Findings)
	assert.Len(t, eligible, 1)
}

func TestCompactFixedRecognizesRepairMergedFromBranchBeforeSource(t *testing.T) {
	repo, source, repair, _ := compactMergedRepairRepo(t)
	_, manifest := compactFixture(t)
	manifest.Sources[0].GitRef = source
	manifest.Sources[0].Findings = manifest.Sources[0].Findings[:1]
	decision := CompactDecision{ID: "F1", Status: "fixed", Reason: "The merged tenant check rejects foreign requests and preserves own access", Change: repair, Evidence: []CompactEvidence{{File: "guard.go", StartLine: 2, EndLine: 2, Quote: "// tenant checked"}}}
	got, eligible, err := reconcileCompact(t.Context(), repo.Root, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{decision}})
	require.NoError(t, err)
	assert.Empty(t, got.Structured.Findings)
	assert.Len(t, eligible, 1)
}

func TestCompactFixedRejectsChangeFromDivergentReviewedEndpoint(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("guard.go", "package fixture\n// unchecked\n", "feat: vulnerable base")
	mainBranch := repo.Run("branch", "--show-current")
	repo.Run("checkout", "-b", "reviewed-source")
	source := repo.CommitFile("reviewed.go", "package fixture\n// reviewed source\n", "review: source change")
	repo.Run("checkout", mainBranch)
	change := repo.CommitFile("guard.go", "package fixture\n// tenant checked\n", "fix: tenant guard on another line")
	_, manifest := compactFixture(t)
	manifest.Sources[0].GitRef = source
	manifest.Sources[0].Findings = manifest.Sources[0].Findings[:1]
	decision := CompactDecision{ID: "F1", Status: "fixed", Reason: "The tenant check rejects foreign requests and preserves own access", Change: change, Evidence: []CompactEvidence{{File: "guard.go", StartLine: 2, EndLine: 2, Quote: "// tenant checked"}}}
	got, eligible, err := reconcileCompact(t.Context(), repo.Root, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{decision}})
	require.NoError(t, err)
	require.Len(t, got.Structured.Findings, 1)
	assert.Equal(t, "Cross-tenant access", got.Structured.Findings[0].Problem)
	assert.Empty(t, eligible)
}

func TestCompactFixedMatchesQuotedUnicodeChangePath(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	source := repo.CommitFile("guard.go", "package fixture\n// unchecked\n", "feat: source")
	repo.Run("config", "core.quotePath", "true")
	repaired := repo.CommitFile("unicøde.go", "package fixture\n// tenant checked\n", "fix: tenant guard")
	_, manifest := compactFixture(t)
	manifest.Sources[0].GitRef = source
	manifest.Sources[0].Findings = manifest.Sources[0].Findings[:1]
	manifest.Sources[0].Findings[0].File = "unicøde.go"
	decision := CompactDecision{ID: "F1", Status: "fixed", Reason: "The tenant check rejects foreign requests and preserves own access", Change: repaired, Evidence: []CompactEvidence{{File: "unicøde.go", StartLine: 2, EndLine: 2, Quote: "// tenant checked"}}}
	got, eligible, err := reconcileCompact(t.Context(), repo.Root, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{decision}})
	require.NoError(t, err)
	assert.Empty(t, got.Structured.Findings)
	assert.Len(t, eligible, 1)
}

func compactMergedRepairRepo(t *testing.T) (*testutil.TestRepo, string, string, string) {
	t.Helper()
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("guard.go", "package fixture\n// unchecked\n", "feat: vulnerable base")
	mainBranch := repo.Run("branch", "--show-current")
	repo.Run("checkout", "-b", "repair-side")
	repair := repo.CommitFile("guard.go", "package fixture\n// tenant checked\n", "fix: tenant guard")
	repo.Run("checkout", mainBranch)
	source := repo.CommitFile("reviewed.go", "package fixture\n// reviewed source\n", "review: source change")
	repo.Run("merge", "--no-ff", "repair-side", "-m", "merge repair")
	return repo, source, repair, repo.HeadSHA()
}

func TestCompactHistorySuppliesPackedGitChanges(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	source := repo.CommitFile("guard.go", "package fixture\n// unchecked\n", "feat: original")
	repaired := repo.CommitFile("guard.go", "package fixture\n// tenant checked\n", "fix: tenant guard")
	_, manifest := compactFixture(t)
	manifest.Sources[0].GitRef = source
	text, err := BuildCompactPrompt(manifest, "", "")
	require.NoError(t, err)
	got, err := AppendCompactHistory(t.Context(), repo.Root, text, manifest)
	require.NoError(t, err)
	assert.Contains(t, got, repaired)
	assert.Contains(t, got, "+// tenant checked")
	assert.Contains(t, got, "-// unchecked")
}

func TestCompactHistorySuppliesFirstParentMergePatch(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	mainBranch := repo.Run("branch", "--show-current")
	repo.Run("checkout", "-b", "history-side")
	repo.CommitFile("side.go", "package fixture\n// side branch patch\n", "side change")
	repo.Run("checkout", mainBranch)
	source := repo.CommitFile("source.go", "package fixture\n// source change\n", "source change")
	repo.Run("merge", "--no-ff", "--no-commit", "history-side")
	repo.WriteFile("merge-only.go", "package fixture\n// merge-only patch\n")
	repo.Run("add", ".")
	repo.Run("commit", "-m", "merge side with additional resolution")
	merge := repo.HeadSHA()
	_, manifest := compactFixture(t)
	manifest.Sources[0].GitRef = source
	text, err := BuildCompactPrompt(manifest, "", "")
	require.NoError(t, err)
	got, err := AppendCompactHistory(t.Context(), repo.Root, text, manifest)
	require.NoError(t, err)
	assert.Contains(t, got, merge)
	assert.Contains(t, got, "merge-only.go")
	assert.Contains(t, got, "+// merge-only patch")
}

func TestCompactFixedAuditRecordsImmutableChange(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	source := repo.CommitFile("guard.go", "package fixture\n// unchecked\n", "feat: original")
	repaired := repo.CommitFile("guard.go", "package fixture\n// tenant checked\n", "fix: tenant guard")
	_, manifest := compactFixture(t)
	manifest.Sources[0].GitRef = source
	manifest.Sources[0].Findings = manifest.Sources[0].Findings[:1]
	got, _, err := reconcileCompact(t.Context(), repo.Root, manifest, compactResponse{Version: 1, Decisions: []CompactDecision{{ID: "F1", Status: "fixed", Reason: "Tenant check blocks the original foreign request", Change: "HEAD", Evidence: []CompactEvidence{{File: "guard.go", StartLine: 2, EndLine: 2, Quote: "// tenant checked"}}}}})
	require.NoError(t, err)
	_, audit, ok := strings.Cut(got.Structured.Summary, "### F1 — fixed\n\n```json\n")
	require.True(t, ok)
	payload, _, ok := strings.Cut(audit, "\n```\n")
	require.True(t, ok)
	var original compactOriginal
	require.NoError(t, json.Unmarshal([]byte(payload), &original))
	assert.Equal(t, repaired, original.Decision.Change)
}
