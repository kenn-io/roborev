package daemon

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"log"
	"os"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/backfill"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func (s *Server) registerMaintenanceAPI(api huma.API) {
	huma.Post(api, "/api/maintenance/verdicts/backfill", s.humaBackfillVerdicts, func(o *huma.Operation) {
		o.OperationID = "backfill-verdicts"
		o.Summary = "Backfill legacy review verdicts"
		o.Tags = []string{"maintenance"}
	})
	huma.Post(api, "/api/maintenance/tokens/backfill", s.humaScanTokenUsage, func(o *huma.Operation) {
		o.OperationID = "scan-token-usage"
		o.Summary = "Recover token usage from daemon job logs and AgentsView"
		o.Tags = []string{"maintenance"}
	})
	huma.Post(api, "/api/maintenance/legacy/convert", s.humaConvertLegacyReviews, func(o *huma.Operation) {
		o.OperationID = "convert-legacy-reviews"
		o.Summary = "Convert recognized archived review formats"
		o.Tags = []string{"maintenance"}
	})
	huma.Post(api, "/api/maintenance/legacy/export", s.humaExportLegacyReviews, func(o *huma.Operation) {
		o.OperationID = "export-legacy-reviews"
		o.Summary = "Export unresolved archived reviews"
		o.Tags = []string{"maintenance"}
	})
	huma.Post(api, "/api/maintenance/legacy/import", s.humaImportLegacyReview, func(o *huma.Operation) {
		o.OperationID = "import-legacy-review"
		o.Summary = "Restore an archived review from a structured document"
		o.Tags = []string{"maintenance"}
		o.MaxBodyBytes = -1
	})
	huma.Post(api, "/api/logs/clean", s.humaCleanJobLogs, func(o *huma.Operation) {
		o.OperationID = "clean-job-logs"
		o.Summary = "Remove old daemon job logs"
		o.Tags = []string{"maintenance"}
	})
}

type BackfillVerdictsOutput struct {
	Body struct {
		Count int `json:"count"`
	}
}

func (s *Server) humaBackfillVerdicts(ctx context.Context, _ *struct{}) (*BackfillVerdictsOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	count, err := s.db.BackfillVerdictBool()
	if err != nil {
		return nil, huma.Error500InternalServerError("backfill verdicts", err)
	}
	out := &BackfillVerdictsOutput{}
	out.Body.Count = count
	return out, nil
}

type ScanTokenUsageInput struct {
	Body struct {
		DryRun bool `json:"dry_run"`
	}
}

type ScannedTokenUsage struct {
	JobID   int64  `json:"job_id"`
	Agent   string `json:"agent"`
	Summary string `json:"summary"`
}

type ScanTokenUsageReport struct {
	Total   int                 `json:"total"`
	Updated int                 `json:"updated"`
	Skipped int                 `json:"skipped"`
	Failed  int                 `json:"failed"`
	Jobs    []ScannedTokenUsage `json:"jobs"`
}

type ScanTokenUsageOutput struct{ Body ScanTokenUsageReport }

func (s *Server) humaScanTokenUsage(ctx context.Context, input *ScanTokenUsageInput) (*ScanTokenUsageOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := s.configWatcher.Config()
	fetchConfig := tokens.FetchConfig{Endpoint: cfg.Cost.Endpoint, Timeout: cfg.Cost.ResolvedTimeout(), RequireCLI: true}
	jobs, err := s.db.ListJobs("", "", 0, 0)
	if err != nil {
		return nil, huma.Error500InternalServerError("list jobs", err)
	}
	agentsviewCandidates := make(map[int64]bool)
	var cursor int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := s.db.ListTokenCostCandidates(cursor, 1000, time.Time{})
		if err != nil {
			return nil, huma.Error500InternalServerError("list cost candidates", err)
		}
		if len(page) == 0 {
			break
		}
		for _, candidate := range page {
			agentsviewCandidates[candidate.JobID] = true
		}
		cursor = page[len(page)-1].JobID
	}
	out := &ScanTokenUsageOutput{}
	report := &out.Body
	for _, job := range backfill.LogTokenCandidates(jobs) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		report.Total++
		var logUsage *tokens.Usage
		currentLog, logErr := JobLogIsCurrentAttempt(job.ID, job.StartedAt)
		if logErr == nil && currentLog {
			logUsage, logErr = tokens.ParseCodexUsageFile(JobLogPath(job.ID))
		}
		if logErr != nil {
			log.Printf("job %d: parse job log: %v", job.ID, logErr)
		}
		var fetchedUsage *tokens.Usage
		var fetchErr error
		if agentsviewCandidates[job.ID] {
			fetchedUsage, fetchErr = tokens.FetchForSessionWithConfig(ctx, job.SessionID, fetchConfig)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if fetchErr != nil {
			log.Printf("job %d: fetch error: %v", job.ID, fetchErr)
			if logUsage == nil {
				report.Failed++
				continue
			}
		}
		usage := backfill.MergeTokenUsage(tokens.ToJSON(logUsage), fetchedUsage)
		if usage == nil {
			report.Skipped++
			continue
		}
		mergedUsage := backfill.MergeTokenUsage(job.TokenUsage, usage)
		if !input.Body.DryRun {
			sessionID := job.SessionID
			if sessionID == "" {
				sessionID = mergedUsage.ThreadID
			}
			stored, saved, err := backfill.StoreCapturedTokenUsage(s.db, backfill.CapturedUsage{
				JobID: job.ID, SessionID: sessionID, ExistingJSON: job.TokenUsage, ExpectedStartedAt: job.StartedAtRaw,
			}, logUsage, fetchedUsage)
			if err != nil {
				log.Printf("job %d: save error: %v", job.ID, err)
				report.Failed++
				continue
			}
			if !saved {
				report.Skipped++
				continue
			}
			mergedUsage = stored
		}
		report.Updated++
		report.Jobs = append(report.Jobs, ScannedTokenUsage{JobID: job.ID, Agent: job.Agent, Summary: mergedUsage.FormatSummary()})
	}
	return out, nil
}

// LegacyMaintenanceTarget selects the daemon database or a PostgreSQL archive.
// DB only verifies the daemon's database identity; it never opens another file.
type LegacyMaintenanceTarget struct {
	DB          string `json:"db,omitempty"`
	PostgresURL string `json:"postgres_url,omitempty"`
}

func (s *Server) validateLegacyTarget(ctx context.Context, target LegacyMaintenanceTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target.DB == "" {
		return nil
	}
	if target.PostgresURL != "" {
		return huma.Error400BadRequest("db and postgres_url are mutually exclusive")
	}
	var dbPath string
	if err := s.db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&dbPath); err != nil {
		return huma.Error500InternalServerError("identify daemon database", err)
	}
	wanted, err := os.Stat(target.DB)
	if err != nil {
		return huma.Error400BadRequest("--db must identify this daemon's database; use --server to select the daemon that owns the database", err)
	}
	actual, err := os.Stat(dbPath)
	if err != nil {
		return huma.Error500InternalServerError("identify daemon database", err)
	}
	if !os.SameFile(wanted, actual) {
		return huma.Error400BadRequest("--db must identify this daemon's database; use --server to select the daemon that owns the database")
	}
	return nil
}

type ConvertLegacyReviewsInput struct {
	Body struct {
		LegacyMaintenanceTarget
		DryRun bool `json:"dry_run"`
	}
}

type ConvertLegacyReviewsOutput struct {
	Body storage.LegacyConversionReport
}

func (s *Server) humaConvertLegacyReviews(ctx context.Context, input *ConvertLegacyReviewsInput) (*ConvertLegacyReviewsOutput, error) {
	if err := s.validateLegacyTarget(ctx, input.Body.LegacyMaintenanceTarget); err != nil {
		return nil, err
	}
	var report storage.LegacyConversionReport
	var err error
	if input.Body.PostgresURL != "" {
		pool, openErr := storage.NewPgPool(ctx, input.Body.PostgresURL, storage.DefaultPgPoolConfig())
		if openErr != nil {
			return nil, huma.Error500InternalServerError("connect PostgreSQL archive", openErr)
		}
		defer pool.Close()
		report, err = pool.ConvertLegacyReviews(ctx, input.Body.DryRun)
	} else {
		report, err = s.db.ConvertLegacyReviews(input.Body.DryRun)
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("convert legacy reviews", err)
	}
	return &ConvertLegacyReviewsOutput{Body: report}, nil
}

type ExportLegacyReviewsInput struct{ Body LegacyMaintenanceTarget }

type ExportLegacyReviewsOutput struct {
	Body struct {
		SQLiteRecords   []storage.LegacyReview         `json:"sqlite_records"`
		PostgresRecords []storage.PostgresLegacyReview `json:"postgres_records"`
	}
}

func (s *Server) humaExportLegacyReviews(ctx context.Context, input *ExportLegacyReviewsInput) (*ExportLegacyReviewsOutput, error) {
	if err := s.validateLegacyTarget(ctx, input.Body); err != nil {
		return nil, err
	}
	out := &ExportLegacyReviewsOutput{}
	var err error
	if input.Body.PostgresURL != "" {
		pool, openErr := storage.NewPgPool(ctx, input.Body.PostgresURL, storage.DefaultPgPoolConfig())
		if openErr != nil {
			return nil, huma.Error500InternalServerError("connect PostgreSQL archive", openErr)
		}
		defer pool.Close()
		out.Body.PostgresRecords, err = pool.UnresolvedLegacyReviews(ctx)
	} else {
		out.Body.SQLiteRecords, err = s.db.UnresolvedLegacyReviews()
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("export legacy reviews", err)
	}
	return out, nil
}

type ImportLegacyReviewInput struct {
	Body struct {
		LegacyMaintenanceTarget
		ID       int64          `json:"id,omitempty"`
		UUID     uuid.UUID      `json:"uuid,omitzero" format:"uuid"`
		Document jsontext.Value `json:"document"`
	}
}

func (s *Server) humaImportLegacyReview(ctx context.Context, input *ImportLegacyReviewInput) (*struct{}, error) {
	if err := s.validateLegacyTarget(ctx, input.Body.LegacyMaintenanceTarget); err != nil {
		return nil, err
	}
	if _, err := structuredreview.Decode(input.Body.Document); err != nil {
		return nil, huma.Error400BadRequest("invalid review document", err)
	}
	var err error
	if input.Body.PostgresURL != "" {
		if input.Body.UUID == uuid.Nil() {
			return nil, huma.Error400BadRequest("a legacy review UUID is required")
		}
		pool, openErr := storage.NewPgPool(ctx, input.Body.PostgresURL, storage.DefaultPgPoolConfig())
		if openErr != nil {
			return nil, huma.Error500InternalServerError("connect PostgreSQL archive", openErr)
		}
		defer pool.Close()
		err = pool.ResolveLegacyReview(ctx, input.Body.UUID, input.Body.Document)
	} else {
		if input.Body.ID <= 0 {
			return nil, huma.Error400BadRequest("a positive legacy review ID is required")
		}
		err = s.db.ResolveLegacyReview(input.Body.ID, input.Body.Document)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("legacy review not found")
		}
		if errors.Is(err, storage.ErrInvalidReviewDocument) {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if errors.Is(err, storage.ErrReviewNotLegacy) {
			return nil, huma.Error409Conflict(err.Error())
		}
		return nil, huma.Error500InternalServerError("import legacy review", err)
	}
	return &struct{}{}, nil
}

type CleanJobLogsInput struct {
	Body struct {
		Days int `json:"days" minimum:"0" maximum:"3650"`
	}
}
type CleanJobLogsOutput struct {
	Body struct {
		Removed int `json:"removed"`
	}
}

func (s *Server) humaCleanJobLogs(ctx context.Context, input *CleanJobLogsInput) (*CleanJobLogsOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &CleanJobLogsOutput{}
	out.Body.Removed = CleanJobLogs(time.Duration(input.Body.Days) * 24 * time.Hour)
	return out, nil
}
