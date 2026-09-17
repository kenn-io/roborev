package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
)

const searchFeedSelect = `
	SELECT rv.id,
	       COALESCE(CAST(rv.uuid AS TEXT), ''),
	       j.id,
	       COALESCE(CAST(j.uuid AS TEXT), ''),
	       j.repo_id,
	       r.name,
	       COALESCE(j.branch, ''),
	       j.git_ref,
	       COALESCE(c.sha, ''),
	       COALESCE(c.subject, ''),
	       COALESCE(NULLIF(j.review_type, ''), 'default'),
	       COALESCE(j.panel_role, ''),
	       COALESCE(CAST(NULLIF(j.panel_run_uuid, '') AS TEXT), ''),
	       j.agent,
	       COALESCE(rv.closed, 0),
	       j.finished_at,
	       rv.output,
	       rv.structured_output,
	       rv.verdict_bool,
	       j.status,
	       COALESCE(j.job_type, ''),
	       j.commit_id,
	       j.diff_content IS NOT NULL
	FROM reviews rv
	JOIN review_jobs j ON j.id = rv.job_id
	JOIN repos r ON r.id = j.repo_id
	LEFT JOIN commits c ON c.id = j.commit_id
`

const searchFeedEligibility = `
	AND j.status IN ('done', 'applied', 'rebased')
	AND (COALESCE(rv.output, '') != '' OR COALESCE(rv.structured_output, '') != '')
	AND (
		j.job_type IN ('review', 'range', 'dirty', 'synthesis', 'compact')
		OR (COALESCE(j.job_type, '') = '' AND (
			j.commit_id IS NOT NULL
			OR j.git_ref = 'dirty'
			OR j.diff_content IS NOT NULL
			OR instr(j.git_ref, '..') > 0
		))
	)
`

// ListSearchDocuments returns one stable page of eligible canonical reviews.
func (db *DB) ListSearchDocuments(ctx context.Context, afterReviewID int64, limit int) ([]SearchReviewSource, error) {
	if limit <= 0 {
		return nil, nil
	}

	sources := make([]SearchReviewSource, 0, limit)
	cursor := afterReviewID
	for len(sources) < limit {
		batchLimit := limit - len(sources)
		rows, err := db.QueryContext(ctx, searchFeedSelect+`
			WHERE rv.id > ?
			`+searchFeedEligibility+`
			ORDER BY rv.id ASC
			LIMIT ?`, cursor, batchLimit)
		if err != nil {
			return nil, err
		}

		scanned := 0
		for rows.Next() {
			source, job, verdictBool, err := scanSearchReviewSource(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			scanned++
			cursor = source.ReviewID
			if !eligibleSearchReview(job, source) {
				continue
			}
			if err := db.attachSearchResponses(&source, job); err != nil {
				_ = rows.Close()
				return nil, err
			}
			source.Verdict = searchVerdict(job, verdictBool, source.Output)
			sources = append(sources, source)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if scanned < batchLimit {
			break
		}
	}
	return sources, nil
}

// GetSearchDocument resolves an eligible review by its UUID document key or
// by the database-local key used for legacy reviews without UUIDs.
func (db *DB) GetSearchDocument(ctx context.Context, docKey string) (*SearchReviewSource, error) {
	row := db.QueryRowContext(ctx, searchFeedSelect+`
		WHERE CASE
			WHEN COALESCE(CAST(rv.uuid AS TEXT), '') != '' THEN CAST(rv.uuid AS TEXT)
			ELSE 'local:' || rv.id
		END = ?
		`+searchFeedEligibility, docKey)
	source, job, verdictBool, err := scanSearchReviewSource(row)
	if err != nil {
		return nil, err
	}
	if !eligibleSearchReview(job, source) {
		return nil, sql.ErrNoRows
	}
	if err := db.attachSearchResponses(&source, job); err != nil {
		return nil, err
	}
	source.Verdict = searchVerdict(job, verdictBool, source.Output)
	return &source, nil
}

func scanSearchReviewSource(scanner sqlScanner) (SearchReviewSource, ReviewJob, sql.NullInt64, error) {
	var source SearchReviewSource
	var job ReviewJob
	var closed int
	var finishedAt sql.NullString
	var structuredOutput sql.NullString
	var verdictBool sql.NullInt64
	var commitID sql.NullInt64
	var hasDiff bool
	err := scanner.Scan(
		&source.ReviewID,
		&source.ReviewUUID,
		&source.JobID,
		&source.JobUUID,
		&source.RepoID,
		&source.RepoName,
		&source.Branch,
		&source.GitRef,
		&source.CommitSHA,
		&source.CommitSubject,
		&source.ReviewType,
		&source.PanelRole,
		&source.PanelRunUUID,
		&source.Agent,
		&closed,
		&finishedAt,
		&source.Output,
		&structuredOutput,
		&verdictBool,
		&job.Status,
		&job.JobType,
		&commitID,
		&hasDiff,
	)
	if err != nil {
		return SearchReviewSource{}, ReviewJob{}, sql.NullInt64{}, err
	}
	source.Closed = closed != 0
	if finishedAt.Valid {
		source.FinishedAt = parseSQLiteTime(finishedAt.String)
	}
	if structuredOutput.Valid && structuredOutput.String != "" {
		if err := json.Unmarshal([]byte(structuredOutput.String), &source.StructuredOutput); err != nil {
			return SearchReviewSource{}, ReviewJob{}, sql.NullInt64{}, fmt.Errorf("decode search review structured output: %w", err)
		}
	}

	job.ID = source.JobID
	job.RepoID = source.RepoID
	job.GitRef = source.GitRef
	job.Agent = source.Agent
	job.ReviewType = source.ReviewType
	job.PanelRole = source.PanelRole
	if commitID.Valid {
		job.CommitID = &commitID.Int64
	}
	if hasDiff {
		diffMarker := ""
		job.DiffContent = &diffMarker
	}
	return source, job, verdictBool, nil
}

func eligibleSearchReview(job ReviewJob, source SearchReviewSource) bool {
	if !job.HasViewableOutput() || (source.Output == "" && len(source.StructuredOutput) == 0) {
		return false
	}
	return job.IsReviewJob() || job.IsSynthesisJob() || job.JobType == JobTypeCompact
}

func (db *DB) attachSearchResponses(source *SearchReviewSource, job ReviewJob) error {
	commitID, fallbackSHA := job.LegacyCommentLookupTarget()
	responses, err := db.GetAllCommentsForJob(job.ID, commitID, fallbackSHA)
	if err != nil {
		return err
	}
	slices.SortFunc(responses, CompareResponses)
	source.Responses = responses
	return nil
}

func searchVerdict(job ReviewJob, verdictBool sql.NullInt64, output string) string {
	applyJobVerdict(&job, verdictBool, output, output != "" || verdictBool.Valid)
	if job.Verdict == nil {
		return ""
	}
	switch Verdict(*job.Verdict) {
	case VerdictPass:
		return "pass"
	case VerdictFail:
		return "fail"
	default:
		return ""
	}
}
