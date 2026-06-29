package daemon

import (
	"encoding/json"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"go.kenn.io/roborev/internal/backfill"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

// registerHumaAPI creates a Huma API on the given mux and registers
// all typed endpoints. The returned huma.API can be used to serve
// the generated OpenAPI spec.
func (s *Server) registerHumaAPI(mux *http.ServeMux) huma.API {
	cfg := huma.DefaultConfig("roborev", version.Version)
	cfg.DocsPath = ""
	cfg.SchemasPath = ""
	api := humago.New(mux, cfg)
	errorSchema := jsonSchema(api, ErrorResponse{})
	enqueueSuccessSchema := oneOfJSONSchema(
		api,
		storage.ReviewJob{},
		EnqueueSkippedResponse{},
	)
	jobSchema := jsonSchema(api, storage.ReviewJob{})

	huma.Get(api, "/api/jobs", s.humaListJobs,
		func(o *huma.Operation) {
			o.OperationID = "list-jobs"
			o.Summary = "List review jobs"
			o.Tags = []string{"jobs"}
		})

	huma.Get(api, "/api/review", s.humaGetReview,
		func(o *huma.Operation) {
			o.OperationID = "get-review"
			o.Summary = "Get a review by job ID or SHA"
			o.Tags = []string{"reviews"}
		})

	huma.Get(api, "/api/export/reviews", s.humaExportReviews,
		func(o *huma.Operation) {
			o.OperationID = "export-reviews"
			o.Summary = "Export completed reviews"
			o.Tags = []string{"reviews"}
		})

	// /api/job/output is registered as a plain HandleFunc
	// (not Huma) because its stream=1 mode uses NDJSON
	// streaming which doesn't fit Huma's typed response model.

	huma.Get(api, "/api/comments", s.humaListComments,
		func(o *huma.Operation) {
			o.OperationID = "list-comments"
			o.Summary = "List comments for a job or commit"
			o.Tags = []string{"comments"}
		})

	huma.Get(api, "/api/repos", s.humaListRepos,
		func(o *huma.Operation) {
			o.OperationID = "list-repos"
			o.Summary = "List repos with job counts"
			o.Tags = []string{"repos"}
		})

	huma.Get(api, "/api/repos/resolve", s.humaResolveRepo,
		func(o *huma.Operation) {
			o.OperationID = "resolve-repo"
			o.Summary = "Resolve whether a path belongs to a tracked repo"
			o.Tags = []string{"repos"}
		})

	huma.Get(api, "/api/branches", s.humaListBranches,
		func(o *huma.Operation) {
			o.OperationID = "list-branches"
			o.Summary = "List branches with job counts"
			o.Tags = []string{"repos"}
		})

	huma.Get(api, "/api/status", s.humaGetStatus,
		func(o *huma.Operation) {
			o.OperationID = "get-status"
			o.Summary = "Get daemon status"
			o.Tags = []string{"daemon"}
		})

	huma.Post(api, "/api/queue/pause", s.humaPauseQueue,
		func(o *huma.Operation) {
			o.OperationID = "pause-queue"
			o.Summary = "Pause queue processing"
			o.Tags = []string{"daemon"}
		})

	huma.Post(api, "/api/queue/unpause", s.humaUnpauseQueue,
		func(o *huma.Operation) {
			o.OperationID = "unpause-queue"
			o.Summary = "Resume queue processing"
			o.Tags = []string{"daemon"}
		})

	huma.Get(api, "/api/summary", s.humaGetSummary,
		func(o *huma.Operation) {
			o.OperationID = "get-summary"
			o.Summary = "Get review summary statistics"
			o.Tags = []string{"daemon"}
		})

	huma.Get(api, "/api/cost", s.humaGetCost,
		func(o *huma.Operation) {
			o.OperationID = "get-cost"
			o.Summary = "Get aggregate review cost"
			o.Tags = []string{"daemon"}
		})

	huma.Get(api, "/api/health", s.humaGetHealth,
		func(o *huma.Operation) {
			o.OperationID = "get-health"
			o.Summary = "Get daemon health"
			o.Tags = []string{"daemon"}
		})

	huma.Get(api, "/api/ping", s.humaPing,
		func(o *huma.Operation) {
			o.OperationID = "ping"
			o.Summary = "Get daemon liveness identity"
			o.Tags = []string{"daemon"}
		})

	huma.Post(api, "/api/shutdown", s.humaShutdown,
		func(o *huma.Operation) {
			o.OperationID = "shutdown"
			o.Summary = "Gracefully shut down the daemon"
			o.Tags = []string{"daemon"}
		})

	huma.Get(api, "/api/sync/status", s.humaSyncStatus,
		func(o *huma.Operation) {
			o.OperationID = "get-sync-status"
			o.Summary = "Get sync worker status"
			o.Tags = []string{"sync"}
		})

	huma.Get(api, "/api/activity", s.humaActivity,
		func(o *huma.Operation) {
			o.OperationID = "list-activity"
			o.Summary = "List recent daemon activity"
			o.Tags = []string{"daemon"}
		})

	huma.Post(api, "/api/enqueue", s.humaEnqueue,
		func(o *huma.Operation) {
			o.OperationID = "enqueue-job"
			o.Summary = "Enqueue a daemon job"
			o.Tags = []string{"jobs"}
			o.DefaultStatus = 201
			o.SkipValidateBody = true
			o.MaxBodyBytes = -1
			o.Responses = jsonResponses(map[string]*huma.Schema{
				"200": enqueueSuccessSchema,
				"201": enqueueSuccessSchema,
				"400": errorSchema,
				"413": errorSchema,
				"500": errorSchema,
				"503": errorSchema,
			})
		})

	huma.Post(api, "/api/jobs/batch", s.humaBatchJobs,
		func(o *huma.Operation) {
			o.OperationID = "batch-jobs"
			o.Summary = "Fetch jobs with reviews by ID"
			o.Tags = []string{"jobs"}
			o.SkipValidateBody = true
		})

	huma.Post(api, "/api/repos/register", s.humaRegisterRepo,
		func(o *huma.Operation) {
			o.OperationID = "register-repo"
			o.Summary = "Register a repository"
			o.Tags = []string{"repos"}
			o.SkipValidateBody = true
		})

	huma.Post(api, "/api/job/update-branch", s.humaUpdateJobBranch,
		func(o *huma.Operation) {
			o.OperationID = "update-job-branch"
			o.Summary = "Update a job branch"
			o.Tags = []string{"jobs"}
			o.SkipValidateBody = true
		})

	huma.Post(api, "/api/remap", s.humaRemap,
		func(o *huma.Operation) {
			o.OperationID = "remap-jobs"
			o.Summary = "Remap jobs after git history rewrite"
			o.Tags = []string{"jobs"}
			o.SkipValidateBody = true
			o.MaxBodyBytes = 1 << 20
		})

	huma.Post(api, "/api/sync/now", s.humaSyncNow,
		func(o *huma.Operation) {
			o.OperationID = "sync-now"
			o.Summary = "Run an immediate sync"
			o.Tags = []string{"sync"}
		})

	huma.Post(api, "/api/tokens/backfill", s.humaBackfillTokens,
		func(o *huma.Operation) {
			o.OperationID = "backfill-tokens"
			o.Summary = "Backfill token usage from AgentsView payloads"
			o.Tags = []string{"jobs"}
			o.MaxBodyBytes = 10 << 20
			o.Responses = jsonResponses(map[string]*huma.Schema{
				"200": jsonSchema(api, backfill.TokenSummary{}),
				"400": errorSchema,
				"500": errorSchema,
			})
		})

	huma.Post(api, "/api/job/cancel", s.humaCancelJob,
		func(o *huma.Operation) {
			o.OperationID = "cancel-job"
			o.Summary = "Cancel a queued or running job"
			o.Tags = []string{"jobs"}
		})

	huma.Post(api, "/api/job/rerun", s.humaRerunJob,
		func(o *huma.Operation) {
			o.OperationID = "rerun-job"
			o.Summary = "Re-enqueue a completed or failed job"
			o.Tags = []string{"jobs"}
		})

	huma.Post(api, "/api/review/close", s.humaCloseReview,
		func(o *huma.Operation) {
			o.OperationID = "close-review"
			o.Summary = "Close or reopen a review"
			o.Tags = []string{"reviews"}
			o.SkipValidateBody = true
		})

	huma.Post(api, "/api/comment", s.humaAddComment,
		func(o *huma.Operation) {
			o.OperationID = "add-comment"
			o.Summary = "Add a comment to a job or commit"
			o.Tags = []string{"comments"}
			o.DefaultStatus = 201
			o.SkipValidateBody = true
		})

	huma.Post(api, "/api/job/fix", s.humaFixJob,
		func(o *huma.Operation) {
			o.OperationID = "create-fix-job"
			o.Summary = "Create a background fix job"
			o.Tags = []string{"jobs"}
			o.DefaultStatus = 201
			o.SkipValidateBody = true
			o.MaxBodyBytes = 50 << 20
			o.Responses = jsonResponses(map[string]*huma.Schema{
				"201": jobSchema,
				"400": errorSchema,
				"404": errorSchema,
				"500": errorSchema,
				"503": errorSchema,
			})
		})

	huma.Post(api, "/api/job/applied", s.humaMarkJobApplied,
		func(o *huma.Operation) {
			o.OperationID = "mark-job-applied"
			o.Summary = "Mark a fix job as applied"
			o.Tags = []string{"jobs"}
			o.SkipValidateBody = true
		})

	huma.Post(api, "/api/job/rebased", s.humaMarkJobRebased,
		func(o *huma.Operation) {
			o.OperationID = "mark-job-rebased"
			o.Summary = "Mark a fix job as rebased"
			o.Tags = []string{"jobs"}
			o.SkipValidateBody = true
		})

	huma.Get(api, "/api/job/output", s.humaJobOutput,
		func(o *huma.Operation) {
			o.OperationID = "get-job-output"
			o.Summary = "Get or stream in-memory job output"
			o.Tags = []string{"jobs"}
		})

	huma.Get(api, "/api/job/log", s.humaJobLog,
		func(o *huma.Operation) {
			o.OperationID = "get-job-log"
			o.Summary = "Get raw job log bytes"
			o.Tags = []string{"jobs"}
		})

	huma.Get(api, "/api/job/patch", s.humaJobPatch,
		func(o *huma.Operation) {
			o.OperationID = "get-job-patch"
			o.Summary = "Get a stored fix patch"
			o.Tags = []string{"jobs"}
		})

	huma.Get(api, "/api/stream/events", s.humaStreamEvents,
		func(o *huma.Operation) {
			o.OperationID = "stream-events"
			o.Summary = "Stream daemon events"
			o.Tags = []string{"daemon"}
		})

	return api
}

// OpenAPISpec returns the daemon OpenAPI document generated from the Huma
// route registry.
func OpenAPISpec() ([]byte, error) {
	mux := http.NewServeMux()
	api := (&Server{}).registerHumaAPI(mux)
	return json.MarshalIndent(api.OpenAPI(), "", "  ")
}

// OpenAPISpecYAML returns the daemon OpenAPI document as YAML generated from
// the Huma route registry.
func OpenAPISpecYAML() ([]byte, error) {
	mux := http.NewServeMux()
	api := (&Server{}).registerHumaAPI(mux)
	return api.OpenAPI().YAML()
}

// OpenAPISpec30 returns a downgraded OpenAPI 3.0 document for generators that
// do not yet support Huma's default OpenAPI 3.1 output.
func OpenAPISpec30() ([]byte, error) {
	mux := http.NewServeMux()
	api := (&Server{}).registerHumaAPI(mux)
	spec, err := api.OpenAPI().Downgrade()
	if err != nil {
		return nil, err
	}
	var formatted any
	if err := json.Unmarshal(spec, &formatted); err != nil {
		return nil, err
	}
	return json.MarshalIndent(formatted, "", "  ")
}

// OpenAPISpec30YAML returns a downgraded OpenAPI 3.0 document as YAML.
func OpenAPISpec30YAML() ([]byte, error) {
	mux := http.NewServeMux()
	api := (&Server{}).registerHumaAPI(mux)
	return api.OpenAPI().DowngradeYAML()
}

func jsonResponses(
	schemas map[string]*huma.Schema,
) map[string]*huma.Response {
	responses := make(map[string]*huma.Response, len(schemas))
	for status, schema := range schemas {
		responses[status] = &huma.Response{
			Content: map[string]*huma.MediaType{
				"application/json": {Schema: schema},
			},
		}
	}
	return responses
}

func jsonSchema(api huma.API, value any) *huma.Schema {
	return api.OpenAPI().Components.Schemas.Schema(
		reflect.TypeOf(value), true, "",
	)
}

func oneOfJSONSchema(api huma.API, values ...any) *huma.Schema {
	schemas := make([]*huma.Schema, 0, len(values))
	for _, value := range values {
		schemas = append(schemas, jsonSchema(api, value))
	}
	return &huma.Schema{OneOf: schemas}
}
