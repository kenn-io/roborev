package storage

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// MemberStats separates attributable contribution/spend from excluded runs.
// Whole-panel spend, including advisory members, remains in FindingStats.Cost.
type MemberStats struct {
	SkippedPanelRuns  int              `json:"skipped_panel_runs"`
	SkippedMemberRuns int              `json:"skipped_member_runs"`
	Cost              CostAggregate    `json:"cost"`
	SkippedCost       CostAggregate    `json:"skipped_cost"`
	Rows              []MemberStatsRow `json:"rows"`
}

// MemberStatsRow groups voting executions by repository identity, panel and
// member name. Runs = AttributedRuns + SkippedRuns. StructuredRuns is the
// findings-raised denominator; Cited = Agreed + Solo counts synthesis findings.
type MemberStatsRow struct {
	RepoPath         string        `json:"repo_path"`
	PanelName        string        `json:"panel_name"`
	MemberName       string        `json:"member_name"`
	Runs             int           `json:"runs"`
	AttributedRuns   int           `json:"attributed_runs"`
	SkippedRuns      int           `json:"skipped_runs"`
	StructuredRuns   int           `json:"structured_runs"`
	FindingsRaised   int           `json:"findings_raised"`
	Cited            int           `json:"cited"`
	Agreed           int           `json:"agreed"`
	Solo             int           `json:"solo"`
	SoloLabeled      int           `json:"solo_labeled"`
	SoloNegative     *int          `json:"solo_negative"`
	SoloNegativeRate *float64      `json:"solo_negative_rate"`
	Cost             CostAggregate `json:"cost"`
	SkippedCost      CostAggregate `json:"skipped_cost"`
	CostPerCited     *float64      `json:"cost_per_cited"`
	CostPerAgreed    *float64      `json:"cost_per_agreed"`
}

type memberExecution struct {
	id                                                   int64
	name, agent, reviewType, status, minSeverity, config string
	doc                                                  *structuredreview.Document
}
type memberContribution struct{ cited, agreed, solo int }

// memberAttribution mirrors toReviewResults/filterSucceeded and source labels.
// Validate the ENTIRE map before crediting any input: duplicate display labels
// are valid, but source indices always refer to ordered positions, never names.
func memberAttribution(doc *structuredreview.Document, members []memberExecution, minSeverity string) ([]memberContribution, bool) {
	if doc == nil || len(members) == 0 {
		return nil, false
	}
	var positions []int
	var labels []string
	clean := true
	for i, m := range members {
		var frozen struct {
			AllowFailure bool `json:"allow_failure"`
		}
		_ = json.Unmarshal([]byte(m.config), &frozen)
		usable := m.status == "done" && m.doc != nil && !m.doc.UnableToReview()
		if !usable {
			if !frozen.AllowFailure || (m.status != "failed" && m.status != "canceled") {
				clean = false
			}
			continue
		}
		positions = append(positions, i)
		label := strings.TrimSpace(m.agent)
		if label == "" {
			label = fmt.Sprintf("review %d", len(positions))
		}
		if rt := strings.TrimSpace(m.reviewType); rt != "" && !config.IsDefaultReviewType(rt) {
			label += " (" + rt + ")"
		}
		labels = append(labels, label)
		threshold := minSeverity
		if threshold == "" {
			threshold = m.minSeverity
		}
		if !m.doc.Passed(threshold) || len(m.doc.Findings) > 0 {
			clean = false
		}
	}
	if len(positions) == 0 {
		return nil, false
	}
	// Only the all-clean shortcut omits source_labels. A passthrough or an agent
	// synthesis retains a complete ordered label list, even with no findings.
	if len(doc.Findings) == 0 && len(doc.SourceLabels) == 0 {
		if !clean || !doc.Passed(minSeverity) {
			return nil, false
		}
	} else if !slices.Equal(labels, doc.SourceLabels) || doc.RequireSources(len(positions)) != nil {
		return nil, false
	}
	result := make([]memberContribution, len(members))
	for _, finding := range doc.Findings {
		sources := map[int]bool{}
		for _, source := range finding.Sources {
			sources[source] = true
		}
		for source := range sources {
			c := &result[positions[source-1]]
			c.cited++
			if len(sources) > 1 {
				c.agreed++
			} else {
				c.solo++
			}
		}
	}
	return result, true
}

// loadMemberExecutions uses the same persisted voting order and canonical
// document conversion as getPanelMemberReviews. Invalid documents exclude a
// run instead of aborting the entire summary; no free-form output is guessed.
func loadMemberExecutions(q querier, run string, asOf time.Time) ([]memberExecution, bool, error) {
	rows, err := q.Query(`SELECT j.id,COALESCE(j.panel_member_name,''),j.agent,COALESCE(j.review_type,''),j.status,
 COALESCE(j.min_severity,''),COALESCE(j.panel_member_config_json,''),COALESCE(j.finished_at,''),rv.structured_output
 FROM review_jobs j LEFT JOIN reviews rv ON rv.job_id=j.id
 WHERE j.panel_run_uuid=? AND j.panel_role='member' AND COALESCE(j.non_voting,0)=0
 ORDER BY j.panel_member_index,j.id`, run)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var members []memberExecution
	valid := true
	for rows.Next() {
		var m memberExecution
		var finished string
		var raw sql.NullString
		if err = rows.Scan(&m.id, &m.name, &m.agent, &m.reviewType, &m.status, &m.minSeverity, &m.config, &finished, &raw); err != nil {
			return nil, false, err
		}
		if raw.Valid {
			doc, e := structuredreview.Decode(jsontext.Value(raw.String))
			if e != nil {
				valid = false
			} else {
				m.doc = &doc
			}
		}
		if parseSQLiteTime(finished).After(asOf) {
			valid = false
		}
		members = append(members, m)
	}
	return members, valid, rows.Err()
}

func findingMemberStats(q querier, reviews []*findingReview, asOf time.Time) (*MemberStats, error) {
	stats := &MemberStats{Rows: []MemberStatsRow{}}
	type key struct{ repo, panel, member string }
	cells := map[key]*MemberStatsRow{}
	seen := map[int64]bool{}
	for _, r := range reviews {
		if r.kind != JobTypeSynthesis {
			continue
		}
		var repo, panel, minimum string
		if err := q.QueryRow(`SELECT r.root_path,COALESCE(j.panel_name,''),COALESCE(j.min_severity,'') FROM review_jobs j JOIN repos r ON r.id=j.repo_id WHERE j.id=?`, r.id).Scan(&repo, &panel, &minimum); err != nil {
			return nil, err
		}
		members, valid, err := loadMemberExecutions(q, r.run, asOf)
		if err != nil {
			return nil, err
		}
		contributions, attributed := memberAttribution(r.doc, members, minimum)
		attributed = attributed && valid && r.run != ""
		if !attributed {
			stats.SkippedPanelRuns++
		}
		costs := map[int64]CostAggregate{}
		for _, execution := range r.executions {
			costs[execution.id] = execution.cost
		}
		for i, m := range members {
			if seen[m.id] {
				continue
			}
			seen[m.id] = true
			k := key{repo, panel, m.name}
			row := cells[k]
			if row == nil {
				row = &MemberStatsRow{RepoPath: repo, PanelName: panel, MemberName: m.name}
				cells[k] = row
			}
			row.Runs++
			if !attributed {
				row.SkippedRuns++
				stats.SkippedMemberRuns++
				addFindingCost(&row.SkippedCost, costs[m.id])
				addFindingCost(&stats.SkippedCost, costs[m.id])
				continue
			}
			row.AttributedRuns++
			if m.status == "done" && m.doc != nil && m.doc.Legacy == nil && !m.doc.UnableToReview() {
				row.StructuredRuns++
				row.FindingsRaised += len(m.doc.Findings)
			}
			row.Cited += contributions[i].cited
			row.Agreed += contributions[i].agreed
			row.Solo += contributions[i].solo
			addFindingCost(&row.Cost, costs[m.id])
			addFindingCost(&stats.Cost, costs[m.id])
		}
	}
	keys := make([]key, 0, len(cells))
	for k := range cells {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.repo != b.repo {
			return a.repo < b.repo
		}
		if a.panel != b.panel {
			return a.panel < b.panel
		}
		return a.member < b.member
	})
	for _, k := range keys {
		row := cells[k]
		finishFindingCost(&row.Cost)
		finishFindingCost(&row.SkippedCost)
		if row.Cost.JobsWithCost > 0 {
			if row.Cited > 0 {
				v := row.Cost.TotalUSD / float64(row.Cited)
				row.CostPerCited = &v
			}
			if row.Agreed > 0 {
				v := row.Cost.TotalUSD / float64(row.Agreed)
				row.CostPerAgreed = &v
			}
		}
		// Panel-parent tier R outcomes are unavailable. Null negatives distinguish
		// unavailable labels from observed zero noise; SoloLabeled stays zero.
		stats.Rows = append(stats.Rows, *row)
	}
	finishFindingCost(&stats.Cost)
	finishFindingCost(&stats.SkippedCost)
	return stats, nil
}
