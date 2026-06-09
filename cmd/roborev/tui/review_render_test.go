package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

// setupRenderModel creates a standardized model for rendering tests
func setupRenderModel(
	view viewKind,
	review *storage.Review,
	opts ...testModelOption,
) model {
	base := []testModelOption{
		withCurrentView(view),
		withReview(review),
		withDimensions(100, 30),
	}
	return initTestModel(append(base, opts...)...)
}

func assertOutputContains(t *testing.T, got, want string) {
	t.Helper()
	assert.Contains(t, got, want)
}

func assertAbsent(t *testing.T, got, want string) {
	t.Helper()
	assert.NotContains(t, got, want)
}

func TestTUIRenderViews(t *testing.T) {
	verdictPass := "P"

	tests := []struct {
		name                      string
		view                      viewKind
		branch                    string
		review                    *storage.Review
		wantContains              []string
		wantAbsent                []string
		checkContentStartsOnLine3 bool
		checkContentStartsOnLine4 bool
		checkNoVerdictOnLine3     bool
	}{
		{
			name:   "review view with branch and closed",
			view:   viewReview,
			branch: "feature/test",
			review: &storage.Review{
				ID:     10,
				Output: "Some review output",
				Closed: true,
				Job: &storage.ReviewJob{
					ID:       1,
					GitRef:   "abc1234",
					RepoName: "myrepo",
					Agent:    "codex",
				},
			},
			wantContains: []string{"on feature/test", "[CLOSED]", "myrepo", "abc1234"},
			wantAbsent:   []string{"Daemon:"},
		},
		{
			name: "review view with model",
			view: viewReview,
			review: &storage.Review{
				ID:     10,
				Agent:  "codex",
				Output: "Some review output",
				Job: &storage.ReviewJob{
					ID:       1,
					GitRef:   "abc1234",
					RepoName: "myrepo",
					Agent:    "codex",
					Model:    "o3",
				},
			},
			wantContains: []string{"(codex: o3)"},
			wantAbsent:   []string{"Daemon:"},
		},
		{
			name: "review view without model",
			view: viewReview,
			review: &storage.Review{
				ID:     10,
				Agent:  "codex",
				Output: "Some review output",
				Job: &storage.ReviewJob{
					ID:       1,
					GitRef:   "abc1234",
					RepoName: "myrepo",
					Agent:    "codex",
					Model:    "",
				},
			},
			wantContains: []string{"(codex)"},
			wantAbsent:   []string{"(codex:", "Daemon:"},
		},
		{
			name: "prompt view with model",
			view: viewKindPrompt,
			review: &storage.Review{
				ID:     10,
				Agent:  "codex",
				Prompt: "Review this code",
				Job: &storage.ReviewJob{
					ID:     1,
					GitRef: "abc1234",
					Agent:  "codex",
					Model:  "o3",
				},
			},
			wantContains: []string{"#1", "(codex: o3)"},
		},
		{
			name: "prompt view without model",
			view: viewKindPrompt,
			review: &storage.Review{
				ID:     10,
				Agent:  "codex",
				Prompt: "Review this code",
				Job: &storage.ReviewJob{
					ID:     1,
					GitRef: "abc1234",
					Agent:  "codex",
					Model:  "",
				},
			},
			wantContains: []string{"(codex)"},
			wantAbsent:   []string{"(codex:"},
		},
		{
			name:   "review view no branch for range",
			view:   viewReview,
			branch: "",
			review: &storage.Review{
				ID:     10,
				Output: "Some review output",
				Job: &storage.ReviewJob{
					ID:       1,
					GitRef:   "abc123..def456",
					RepoName: "myrepo",
					Agent:    "codex",
				},
			},
			wantContains: []string{"abc123..def456"},
			wantAbsent:   []string{" on ", "Daemon:"},
		},
		{
			name: "review view no blank line without verdict",
			view: viewReview,
			review: &storage.Review{
				ID:     10,
				Output: "Line 1\nLine 2\nLine 3",
				Job: &storage.ReviewJob{
					ID:       1,
					GitRef:   "abc1234",
					RepoName: "myrepo",
					Agent:    "codex",
					Verdict:  nil,
				},
			},
			wantContains:              []string{"Review", "abc1234"},
			wantAbsent:                []string{"Daemon:"},
			checkContentStartsOnLine3: true,
		},
		{
			name: "review view verdict on line 2",
			view: viewReview,
			review: &storage.Review{
				ID:     10,
				Output: "Line 1\nLine 2\nLine 3",
				Job: &storage.ReviewJob{
					ID:       1,
					GitRef:   "abc1234",
					RepoName: "myrepo",
					Agent:    "codex",
					Verdict:  &verdictPass,
				},
			},
			wantContains:              []string{"Review", "abc1234", "Verdict"},
			wantAbsent:                []string{"Daemon:"},
			checkContentStartsOnLine4: true,
		},
		{
			name: "review view closed without verdict",
			view: viewReview,
			review: &storage.Review{
				ID:     10,
				Output: "Line 1\n\nLine 2\n\nLine 3",
				Closed: true,
				Job: &storage.ReviewJob{
					ID:       1,
					GitRef:   "abc1234",
					RepoName: "myrepo",
					Agent:    "codex",
					Verdict:  nil,
				},
			},
			wantContains:              []string{"Review", "abc1234", "[CLOSED]"},
			wantAbsent:                []string{"Daemon:"},
			checkContentStartsOnLine4: true,
			checkNoVerdictOnLine3:     true,
		},
		{
			name:   "failed job no branch shown",
			view:   viewReview,
			branch: "",
			review: &storage.Review{
				Agent:  "codex",
				Output: "Job failed:\n\nsome error",
				Job: &storage.ReviewJob{
					ID:       1,
					GitRef:   "abc1234",
					RepoName: "myrepo",
					Agent:    "codex",
					Status:   storage.JobStatusFailed,
				},
			},
			wantAbsent: []string{" on ", "Daemon:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := setupRenderModel(tt.view, tt.review, withBranchName(tt.branch))
			output := m.View().Content

			for _, want := range tt.wantContains {
				assertOutputContains(t, output, want)
			}
			for _, absent := range tt.wantAbsent {
				assertAbsent(t, output, absent)
			}

			// Specific check for layout tests (Lines 0, 1, 2)
			if tt.checkContentStartsOnLine3 {
				lines := strings.Split(output, "\n")
				foundContent := false
				for _, line := range lines[2:] {
					if strings.Contains(stripANSI(line), "Line 1") {
						foundContent = true
						break
					}
				}
				assert.True(t, foundContent, "Content should contain 'Line 1' after header, output:\n%s", output)
			}
			if tt.checkContentStartsOnLine4 {
				lines := strings.Split(output, "\n")
				foundContent := false
				for _, line := range lines[3:] {
					if strings.Contains(stripANSI(line), "Line 1") {
						foundContent = true
						break
					}
				}
				assert.True(t, foundContent, "Content should contain 'Line 1' after closed/verdict line, output:\n%s", output)
			}
			if tt.checkNoVerdictOnLine3 {
				lines := strings.Split(output, "\n")
				line3 := ""
				if len(lines) > 2 {
					line3 = lines[2]
				}
				assert.False(t, len(lines) > 2 && strings.Contains(line3, "Verdict"),
					"Line 2 should not contain 'Verdict' when no verdict is set, got: %s", line3)
			}
		})
	}
}

func TestTUIVisibleLinesCalculationTable(t *testing.T) {
	verdictPass := "P"
	verdictFail := "F"

	tests := []struct {
		name                     string
		width                    int
		height                   int
		branch                   string
		reviewAgent              string
		jobRef                   string
		jobRepoName              string
		jobAgent                 string
		jobVerdict               *string
		closed                   bool
		wantVisibleLines         int
		wantContains             []string
		checkVisibleContentCount bool
	}{
		{
			name:                     "no verdict",
			width:                    120,
			height:                   10,
			jobRef:                   "abc1234",
			jobAgent:                 "codex",
			jobVerdict:               nil,
			wantVisibleLines:         5, // height 10 - 5 non-content = 5
			checkVisibleContentCount: true,
		},
		{
			name:             "with verdict",
			width:            120,
			height:           10,
			jobRef:           "abc1234",
			jobAgent:         "codex",
			jobVerdict:       &verdictPass,
			wantVisibleLines: 4, // height 10 - 6 non-content = 4
		},
		{
			name:             "narrow terminal",
			width:            50,
			height:           10,
			jobRef:           "abc1234",
			jobAgent:         "codex",
			jobVerdict:       nil,
			wantVisibleLines: 4, // height 10 - 6 non-content = 4
		},
		{
			name:             "narrow terminal with verdict",
			width:            50,
			height:           10,
			jobRef:           "abc1234",
			jobAgent:         "codex",
			jobVerdict:       &verdictFail,
			wantVisibleLines: 3, // height 10 - 7 non-content = 3
			wantContains:     []string{"Verdict"},
		},
		{
			name:             "long title wraps",
			width:            50,
			height:           12,
			branch:           "feature/very-long-branch-name",
			reviewAgent:      "claude-code",
			jobRef:           "abc1234567890..def5678901234",
			jobRepoName:      "very-long-repository-name-here",
			jobAgent:         "claude-code",
			jobVerdict:       nil,
			closed:           true,
			wantVisibleLines: 3, // height 12 - 9 non-content = 3
			wantContains:     []string{"very-long-repository-name-here", "feature/very-long-branch-name", "[CLOSED]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			review := &storage.Review{
				ID:     10,
				Output: "L1\nL2\nL3\nL4\nL5\nL6\nL7\nL8\nL9\nL10\nL11\nL12\nL13\nL14\nL15\nL16\nL17\nL18\nL19\nL20",
				Closed: tt.closed,
				Agent:  tt.reviewAgent,
				Job: &storage.ReviewJob{
					ID:       1,
					GitRef:   tt.jobRef,
					RepoName: tt.jobRepoName,
					Agent:    tt.jobAgent,
					Verdict:  tt.jobVerdict,
				},
			}

			m := setupRenderModel(viewReview, review, withBranchName(tt.branch))
			m.width = tt.width
			m.height = tt.height

			output := m.View().Content

			expectedIndicator := fmt.Sprintf("[1-%d of %d lines]", tt.wantVisibleLines, 21)
			assert.Contains(t, output, expectedIndicator)

			if tt.checkVisibleContentCount {
				contentCount := 0
				for line := range strings.SplitSeq(output, "\n") {
					trimmed := strings.TrimSpace(stripANSI(line))
					if len(trimmed) >= 2 && trimmed[0] == 'L' && trimmed[1] >= '0' && trimmed[1] <= '9' {
						contentCount++
					}
				}
				assert.NotZero(t, contentCount, "Expected at least some content lines visible")
			}

			for _, want := range tt.wantContains {
				assert.Contains(t, output, want)
			}
		})
	}
}

func TestPanelReviewHeaderSummarizesMembers(t *testing.T) {
	job := makeJob(10, withSynthesis("R", storage.PanelSummary{MembersTotal: 2}))
	members := []storage.ReviewJob{
		makeJob(11, withPanelMember("R", "default", 0), withVerdict("P")),
		makeJob(12, withPanelMember("R", "security", 1), withVerdict("F")),
	}
	header := panelReviewHeader(job, members)
	assert.Contains(t, header, "2 reviewers")
	assert.Contains(t, header, "default P")
	assert.Contains(t, header, "security F")
}

func TestPanelReviewHeaderFallsBackToSummary(t *testing.T) {
	// Opening a parent that was never expanded (members not cached) must still
	// render a header — from PanelSummary — never dropped.
	job := makeJob(10, withSynthesis("R", storage.PanelSummary{MembersTotal: 3, MembersSucceeded: 2, MembersFailed: 1}))
	header := panelReviewHeader(job, nil)
	assert.Contains(t, header, "3 reviewers")
	assert.Contains(t, header, "2 ok")
	assert.Contains(t, header, "1 failed")
}

func TestRenderReviewPrefixesPanelHeader(t *testing.T) {
	job := makeJob(10, withRef("syn"), withStatus(storage.JobStatusDone),
		withSynthesis("R", storage.PanelSummary{MembersTotal: 2, MembersSucceeded: 2}))
	review := makeReview(1, &job, withReviewOutput("Synthesized findings"))
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width, m.height = 120, 30
	m.currentView = viewReview
	m.currentReview = review
	m.panelMembers = map[string][]storage.ReviewJob{"R": {
		makeJob(11, withPanelMember("R", "default", 0), withVerdict("P")),
		makeJob(12, withPanelMember("R", "security", 1), withVerdict("F")),
	}}
	out := stripANSI(m.renderReviewView())
	assert.Contains(t, out, "2 reviewers")
	assert.Contains(t, out, "default P")
	assert.Contains(t, out, "Synthesized findings")
}
