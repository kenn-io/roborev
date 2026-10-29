package storage

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/structuredreview"
)

func readMemberStats(t *testing.T, db *DB) MemberStats {
	t.Helper()
	f, err := db.GetFindingStats(SummaryOptions{}, "member")
	require.NoError(t, err)
	require.NotNil(t, f.Members)
	return *f.Members
}

func memberStatsPanel(t *testing.T, db *DB, sources [][]int, labels []string) (int64, []int64) {
	t.Helper()
	repo := createRepo(t, db, "/tmp/member-stats")
	var findings []structuredreview.Finding
	for _, source := range sources {
		findings = append(findings, structuredreview.Finding{Severity: "high", Problem: "issue", Fix: "repair", Sources: source})
	}
	parent := findingStatsReview(t, db, repo.ID, JobTypeSynthesis, findings)
	run := testUUID(fmt.Sprintf("member-stats-run-%d", parent))
	_, err := db.Exec("UPDATE review_jobs SET panel_run_uuid=?,panel_role='synthesis',panel_name='quality' WHERE id=?", run, parent)
	require.NoError(t, err)
	raw, err := json.Marshal(structuredreview.Document{SchemaVersion: 2, Summary: "combined", Verdict: "pass", Findings: findings, SourceLabels: labels})
	require.NoError(t, err)
	_, err = db.Exec("UPDATE reviews SET structured_output=? WHERE job_id=?", string(raw), parent)
	require.NoError(t, err)
	var ids []int64
	// Insertion order intentionally differs from persisted member positions.
	for _, idx := range []int{1, 0, 2} {
		id := findingStatsReview(t, db, repo.ID, JobTypeReview, []structuredreview.Finding{{Severity: "low", Problem: "member issue", Fix: "repair"}})
		_, err = db.Exec("UPDATE review_jobs SET panel_run_uuid=?,panel_role='member',panel_name='quality',panel_member_name=?,panel_member_index=?,agent='test',review_type='default' WHERE id=?", run, []string{"first", "second", "observer"}[idx], idx, id)
		require.NoError(t, err)
		if idx == 2 {
			_, err = db.Exec("UPDATE review_jobs SET non_voting=1 WHERE id=?", id)
			require.NoError(t, err)
		}
		ids = append(ids, id)
	}
	return parent, []int64{ids[1], ids[0], ids[2]}
}

func TestMemberStatsContribution(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/member-stats")
	_, _ = memberStatsPanel(t, db, [][]int{{1, 2}, {1, 1}, {1, 2}}, []string{"test", "test"})
	m := readMemberStats(t, db)
	require.Len(t, m.Rows, 2)
	a := assert.New(t)
	first, second := m.Rows[0], m.Rows[1]
	a.Equal("first", first.MemberName)
	a.Equal("second", second.MemberName)
	a.Equal(repo.RootPath, first.RepoPath)
	a.Equal("quality", first.PanelName)
	a.Equal(1, first.Runs)
	a.Equal(1, first.AttributedRuns)
	a.Equal(1, first.StructuredRuns)
	a.Equal(1, first.FindingsRaised)
	a.Equal(3, first.Cited)
	a.Equal(2, first.Agreed)
	a.Equal(1, first.Solo, "duplicate sources are still solo")
	a.Equal(2, second.Cited)
	a.Equal(2, second.Agreed)
	a.Zero(second.Solo)
	a.Zero(first.SoloLabeled)
	a.Nil(first.SoloNegative, "panel parent labels unavailable")
	a.InDelta(2.0, first.Cost.TotalUSD, 1e-9, "charge execution once, not per finding")
	a.InDelta(4.0, m.Cost.TotalUSD, 1e-9, "exclude synthesis and observer spend")
	full, err := db.GetFindingStats(SummaryOptions{}, "member")
	require.NoError(t, err)
	a.InDelta(8.0, full.Cost.TotalUSD, 1e-9, "whole-panel spend retains synthesis and advisory work")
	require.NotNil(t, first.CostPerCited)
	a.InDelta(2.0/3, *first.CostPerCited, 0.00001)
	require.NotNil(t, first.CostPerAgreed)
	a.InDelta(1.0, *first.CostPerAgreed, 1e-9)
}

func TestMemberStatsRejectsAmbiguousAttribution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		sources [][]int
		labels  []string
	}{
		{"label order", [][]int{{1}}, []string{"wrong", "test"}},
		{"label count", [][]int{{1}}, []string{"test"}},
		{"missing labels", [][]int{{1}}, nil},
		{"missing sources", [][]int{nil}, []string{"test", "test"}},
		{"zero source", [][]int{{0}}, []string{"test", "test"}},
		{"out of range", [][]int{{3}}, []string{"test", "test"}},
		{"partly invalid", [][]int{{1}, {2, 3}}, []string{"test", "test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			defer db.Close()
			memberStatsPanel(t, db, tc.sources, tc.labels)
			m := readMemberStats(t, db)
			a := assert.New(t)
			a.Equal(1, m.SkippedPanelRuns)
			a.Equal(2, m.SkippedMemberRuns)
			a.InDelta(4.0, m.SkippedCost.TotalUSD, 1e-9)
			a.Zero(m.Cost.JobsTotal)
			require.Len(t, m.Rows, 2)
			for _, r := range m.Rows {
				a.Equal(1, r.Runs)
				a.Equal(1, r.SkippedRuns)
				a.Zero(r.Cited)
				a.Zero(r.FindingsRaised)
				a.Zero(r.Cost.JobsTotal)
				a.InDelta(2.0, r.SkippedCost.TotalUSD, 1e-9)
			}
		})
	}
}

func TestMemberStatsCleanFailedAndUnavailableOutputs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status, raw, cfg string
		passthrough, valid     bool
		structured             int
	}{
		{"all clean", "done", `{"schema_version":2,"summary":"clean","verdict":"pass","findings":[]}`, "", false, true, 1},
		{"allowed failure clean shortcut", "failed", "", `{"allow_failure":true}`, false, true, 0},
		{"allowed cancellation clean shortcut", "canceled", "", `{"allow_failure":true}`, false, true, 0},
		{"skipped is not an allowed failure", "skipped", "", `{"allow_failure":true}`, false, false, 0},
		{"required failure not clean shortcut", "failed", "", "", false, false, 0},
		{"passthrough failure", "failed", "", "", true, true, 0},
		{"passthrough skipped", "skipped", "", "", true, true, 0},
		{"passthrough empty", "done", "", "", true, true, 0},
		{"passthrough unable", "done", `{"schema_version":2,"summary":"unavailable","verdict":"unable_to_review","findings":[]}`, "", true, true, 0},
		{"invalid document", "done", "{", "", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			defer db.Close()
			sources := [][]int(nil)
			labels := []string(nil)
			if tc.passthrough {
				sources = [][]int{{1}}
				labels = []string{"test"}
			}
			_, ids := memberStatsPanel(t, db, sources, labels)
			// First member is a clean usable input; one-success passthrough uses [1].
			if !tc.passthrough {
				_, err := db.Exec("UPDATE reviews SET structured_output=? WHERE job_id=?", `{"schema_version":2,"summary":"clean","verdict":"pass","findings":[]}`, ids[0])
				require.NoError(t, err)
			}
			_, err := db.Exec("UPDATE review_jobs SET status=?,panel_member_config_json=? WHERE id=?", tc.status, tc.cfg, ids[1])
			require.NoError(t, err)
			var raw any
			if tc.raw != "" {
				raw = tc.raw
			}
			_, err = db.Exec("UPDATE reviews SET structured_output=? WHERE job_id=?", raw, ids[1])
			require.NoError(t, err)
			m := readMemberStats(t, db)
			require.Len(t, m.Rows, 2)
			a := assert.New(t)
			if tc.valid {
				a.Zero(m.SkippedPanelRuns)
				a.InDelta(4.0, m.Cost.TotalUSD, 1e-9)
				a.Equal(1, m.Rows[1].Runs, "failed and empty runs stay visible")
				a.Equal(tc.structured, m.Rows[1].StructuredRuns)
				a.Nil(m.Rows[1].CostPerCited)
			} else {
				a.Equal(1, m.SkippedPanelRuns)
				a.Zero(m.Cost.JobsTotal)
			}
		})
	}
}

func TestMemberStatsCostCoverage(t *testing.T) {
	t.Parallel()
	for _, usage := range []string{`{"has_cost":true,"cost_usd":0}`, `{"has_cost":true}`, `{}`} {
		t.Run(usage, func(t *testing.T) {
			db := openTestDB(t)
			defer db.Close()
			_, ids := memberStatsPanel(t, db, [][]int{{1}}, []string{"test", "test"})
			_, err := db.Exec("UPDATE review_jobs SET token_usage=? WHERE id=?", usage, ids[0])
			require.NoError(t, err)
			m := readMemberStats(t, db)
			require.Len(t, m.Rows, 2)
			a := assert.New(t)
			r := m.Rows[0]
			a.Equal(1, r.Cost.JobsTotal)
			a.Zero(r.Cost.TotalUSD)
			if usage == `{"has_cost":true,"cost_usd":0}` {
				a.True(r.Cost.Complete)
				require.NotNil(t, r.CostPerCited)
				a.Zero(*r.CostPerCited)
			} else {
				a.False(r.Cost.Complete)
				a.Nil(r.CostPerCited)
			}
			a.Nil(r.CostPerAgreed)
		})
	}
}

func FuzzMemberSourceAttribution(f *testing.F) {
	f.Add([]byte{0, 0, 1}, "test", []byte{1, 2}, "", false)
	f.Add([]byte{1, 0, 0}, "", []byte{1, 1}, "", false)
	f.Add([]byte{0, 0}, "same", []byte{0, 3}, "", false)
	f.Add([]byte{0, 0}, "same", []byte{1}, "other,same", true)
	f.Fuzz(func(t *testing.T, statuses []byte, label string, sources []byte, arbitraryLabels string, corruptLabels bool) {
		// Three positions suffice to explore filtered order, duplicate labels,
		// missing sources and both source bounds without a database or subprocess.
		members := make([]memberExecution, 3)
		var positions []int
		var expectedLabels []string
		for i := range members {
			code := byte(1)
			if i < len(statuses) {
				code = statuses[i] % 4
			}
			members[i] = memberExecution{agent: label, status: []string{"done", "failed", "skipped", "done"}[code]}
			if code != 3 {
				members[i].doc = &structuredreview.Document{SchemaVersion: 2, Verdict: "pass"}
			}
			if code == 0 {
				positions = append(positions, i)
				expectedLabel := strings.TrimSpace(label)
				if expectedLabel == "" {
					expectedLabel = fmt.Sprintf("review %d", len(positions))
				}
				expectedLabels = append(expectedLabels, expectedLabel)
			}
		}
		sourceInts := make([]int, len(sources))
		valid := len(positions) > 0 && len(sources) > 0
		unique := map[int]bool{}
		for i, source := range sources {
			sourceInts[i] = int(int8(source))
			if sourceInts[i] < 1 || sourceInts[i] > len(positions) {
				valid = false
			}
			unique[sourceInts[i]] = true
		}
		labels := append([]string(nil), expectedLabels...)
		if corruptLabels {
			labels = strings.Split(arbitraryLabels, ",")
		}
		valid = valid && slices.Equal(labels, expectedLabels)
		doc := &structuredreview.Document{SchemaVersion: 2, Verdict: "fail", SourceLabels: labels, Findings: []structuredreview.Finding{{Sources: sourceInts}}}
		result, ok := memberAttribution(doc, members, "")
		require.Equal(t, valid, ok)
		if !ok {
			assert.Nil(t, result)
			return
		}
		expected := make([]memberContribution, len(members))
		for source := range unique {
			i := positions[source-1]
			expected[i].cited++
			if len(unique) > 1 {
				expected[i].agreed++
			} else {
				expected[i].solo++
			}
		}
		assert.Equal(t, expected, result, "source positions must survive filtering and duplicate labels")
		for _, c := range result {
			assert.Equal(t, c.cited, c.agreed+c.solo)
		}
	})
}

func TestMemberStatsGroupingAndPartialSpend(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	primaryRepo := createRepo(t, db, "/tmp/member-stats")
	memberStatsPanel(t, db, [][]int{{1}}, []string{"test", "test"})
	_, ids := memberStatsPanel(t, db, [][]int{{1}}, []string{"test", "test"})
	_, err := db.Exec("UPDATE review_jobs SET token_usage=? WHERE id=?", `{"has_cost":true}`, ids[0])
	require.NoError(t, err)
	otherParent, others := memberStatsPanel(t, db, [][]int{{1}}, []string{"test", "test"})
	_, err = db.Exec("UPDATE review_jobs SET panel_name='security' WHERE id=?", otherParent)
	require.NoError(t, err)
	repo := createRepo(t, db, "/tmp/other-member-stats")
	thirdParent, thirds := memberStatsPanel(t, db, [][]int{{1}}, []string{"test", "test"})
	for _, id := range append(thirds, thirdParent) {
		_, err = db.Exec("UPDATE review_jobs SET repo_id=? WHERE id=?", repo.ID, id)
		require.NoError(t, err)
	}
	// Non-invoked members contribute runs/findings, but cannot have prices.
	_, err = db.Exec("UPDATE review_jobs SET agent_invoked=0,token_usage=NULL WHERE id=?", others[0])
	require.NoError(t, err)
	m := readMemberStats(t, db)
	require.Len(t, m.Rows, 6)
	a := assert.New(t)
	var first *MemberStatsRow
	for i := range m.Rows {
		r := &m.Rows[i]
		if r.RepoPath == primaryRepo.RootPath && r.PanelName == "quality" && r.MemberName == "first" {
			first = r
		}
		if r.PanelName == "security" && r.MemberName == "first" {
			a.Equal(1, r.AttributedRuns)
			a.Equal(1, r.Cited)
			a.Zero(r.Cost.JobsTotal)
			a.Nil(r.CostPerCited)
		}
	}
	require.NotNil(t, first)
	a.Equal(2, first.Runs)
	a.Equal(2, first.Cited)
	a.InDelta(2.0, first.Cost.TotalUSD, 1e-9)
	a.Equal(2, first.Cost.JobsTotal)
	a.Equal(1, first.Cost.JobsWithCost)
	a.False(first.Cost.Complete)
	require.NotNil(t, first.CostPerCited)
	a.InDelta(1.0, *first.CostPerCited, 1e-9)
}

func TestMemberSourceLabels(t *testing.T) {
	t.Parallel()
	clean := &structuredreview.Document{SchemaVersion: 2, Verdict: "pass"}
	members := []memberExecution{{agent: "ignored", status: "failed"}, {agent: "  ", reviewType: " general ", status: "done", doc: clean}, {agent: " test ", reviewType: " security ", status: "done", doc: clean}}
	doc := &structuredreview.Document{SchemaVersion: 2, Verdict: "fail", SourceLabels: []string{"review 1", "test (security)"}, Findings: []structuredreview.Finding{{Sources: []int{2}}}}
	got, valid := memberAttribution(doc, members, "")
	require.True(t, valid)
	assert.Equal(t, []memberContribution{{}, {}, {cited: 1, solo: 1}}, got)
	doc.SourceLabels = []string{"test (security)", "review 1"}
	got, valid = memberAttribution(doc, members, "")
	assert.False(t, valid, "ordered labels must match even when counts match")
	assert.Nil(t, got)
	// A failed/filtered source must never shift credit to the following member.
	doc.SourceLabels = []string{"ignored", "review 1", "test (security)"}
	got, valid = memberAttribution(doc, members, "")
	assert.False(t, valid)
	assert.Nil(t, got)
}

func TestMemberStatsRepeatedMemberIndicesUseJobOrder(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	_, ids := memberStatsPanel(t, db, [][]int{{1}}, []string{"test", "test"})
	for _, id := range ids[:2] {
		_, err := db.Exec("UPDATE review_jobs SET panel_member_index=4 WHERE id=?", id)
		require.NoError(t, err)
	}
	// The second named member was inserted first. With equal configured indices,
	// id ordering makes it source position 1; neither names nor indices are IDs.
	m := readMemberStats(t, db)
	require.Len(t, m.Rows, 2)
	a := assert.New(t)
	a.Equal("first", m.Rows[0].MemberName)
	a.Zero(m.Rows[0].Cited)
	a.Equal("second", m.Rows[1].MemberName)
	a.Equal(1, m.Rows[1].Cited)
	a.Equal(1, m.Rows[1].Solo)
}
