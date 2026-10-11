package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	       jc.diff_content IS NOT NULL
	FROM reviews rv
	JOIN review_jobs j ON j.id = rv.job_id
	JOIN repos r ON r.id = j.repo_id
	LEFT JOIN commits c ON c.id = j.commit_id
	LEFT JOIN job_content jc ON jc.job_id = j.id
`

const searchFeedEligibility = `
	AND j.status IN ('done', 'applied', 'rebased')
	AND (COALESCE(rv.output, '') != '' OR COALESCE(rv.structured_output, '') != '')
	AND (
		j.job_type IN ('review', 'range', 'dirty', 'synthesis', 'compact', 'goal_review')
		OR (COALESCE(j.job_type, '') = '' AND (
			j.commit_id IS NOT NULL
			OR j.git_ref = 'dirty'
			OR jc.diff_content IS NOT NULL
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

// GetSearchDocumentForJob returns the job's complete search document, or nil
// when its review has been deleted or is no longer eligible for search.
func (db *DB) GetSearchDocumentForJob(ctx context.Context, jobID int64) (*SearchReviewSource, error) {
	source, job, verdict, err := scanSearchReviewSource(db.QueryRowContext(ctx,
		searchFeedSelect+` WHERE j.id = ? `+searchFeedEligibility, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !eligibleSearchReview(job, source) {
		return nil, nil
	}
	if err := db.attachSearchResponses(&source, job); err != nil {
		return nil, err
	}
	source.Verdict = searchVerdict(job, verdict, source.Output)
	return &source, nil
}

// SearchDocumentKey is the search document key for a review: its UUID, or a
// database-local key for legacy reviews without one.
func SearchDocumentKey(reviewID int64, reviewUUID string) string {
	if reviewUUID != "" {
		return reviewUUID
	}
	return "local:" + strconv.FormatInt(reviewID, 10)
}

// GetSearchReviews returns the eligible reviews among docKeys, keyed by
// document key. Missing keys were deleted or are no longer eligible. Responses
// are not attached; search pages only need review metadata.
func (db *DB) GetSearchReviews(ctx context.Context, docKeys []string) (map[string]SearchReviewSource, error) {
	var uuids, ids []any
	for _, key := range docKeys {
		if local, ok := strings.CutPrefix(key, "local:"); ok {
			id, err := strconv.ParseInt(local, 10, 64)
			if err != nil {
				continue
			}
			ids = append(ids, id)
			continue
		}
		uuids = append(uuids, key)
	}
	result := make(map[string]SearchReviewSource, len(docKeys))
	for _, lookup := range []struct {
		column string
		values []any
	}{{column: "rv.uuid", values: uuids}, {column: "rv.id", values: ids}} {
		if len(lookup.values) == 0 {
			continue
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(lookup.values)), ",")
		rows, err := db.QueryContext(ctx, searchFeedSelect+`
			WHERE `+lookup.column+` IN (`+placeholders+`)
			`+searchFeedEligibility, lookup.values...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			source, job, verdictBool, err := scanSearchReviewSource(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			if !eligibleSearchReview(job, source) {
				continue
			}
			source.Verdict = searchVerdict(job, verdictBool, source.Output)
			result[SearchDocumentKey(source.ReviewID, source.ReviewUUID)] = source
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
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
