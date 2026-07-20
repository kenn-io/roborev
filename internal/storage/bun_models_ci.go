package storage

import "github.com/uptrace/bun"

var sqliteCIPanelColumns = []string{
	"id",
	"github_repo",
	"pr_number",
	"head_sha",
	"panel_run_uuid",
	"synthesis_job_id",
	"created_at",
	"posting_claimed_at",
	"posted_at",
	"retired_at",
	"outcome",
	"first_attempt_at",
	"attempt_count",
	"synthesis_agent",
	"synthesis_model",
	"allow_stale_post",
}

var sqliteReviewAttemptColumns = []string{
	"id", "github_repo", "pr_number", "head_sha", "attempt",
	"first_attempt_at", "next_attempt_at", "last_error_class",
	"consecutive_genuine_attempts", "last_error_excerpt",
	"last_panel_run_uuid", "state", "updated_at",
}

type ciPRReviewRow struct {
	bun.BaseModel `bun:"table:ci_pr_reviews,alias:cpr"`
	ID            int64  `bun:"id,pk,autoincrement"`
	GithubRepo    string `bun:"github_repo"`
	PRNumber      int    `bun:"pr_number"`
	HeadSHA       string `bun:"head_sha"`
	JobID         int64  `bun:"job_id"`
	CreatedAt     dbTime `bun:"created_at"`
}

type ciPanelRow struct {
	bun.BaseModel    `bun:"table:ci_pr_panels,alias:cp"`
	ID               int64   `bun:"id,pk,autoincrement"`
	GithubRepo       string  `bun:"github_repo"`
	PRNumber         int     `bun:"pr_number"`
	HeadSHA          string  `bun:"head_sha"`
	PanelRunUUID     string  `bun:"panel_run_uuid"`
	SynthesisJobID   *int64  `bun:"synthesis_job_id"`
	CreatedAt        dbTime  `bun:"created_at"`
	PostingClaimedAt dbTime  `bun:"posting_claimed_at"`
	PostedAt         dbTime  `bun:"posted_at"`
	RetiredAt        dbTime  `bun:"retired_at"`
	Outcome          *string `bun:"outcome"`
	FirstAttemptAt   dbTime  `bun:"first_attempt_at"`
	AttemptCount     *int64  `bun:"attempt_count"`
	SynthesisAgent   *string `bun:"synthesis_agent"`
	SynthesisModel   *string `bun:"synthesis_model"`
	AllowStalePost   bool    `bun:"allow_stale_post"`
}

func (row ciPanelRow) toModel() CIPanel {
	panel := CIPanel{
		ID:               row.ID,
		GithubRepo:       row.GithubRepo,
		PRNumber:         row.PRNumber,
		HeadSHA:          row.HeadSHA,
		PanelRunUUID:     row.PanelRunUUID,
		SynthesisJobID:   cloneInt64Pointer(row.SynthesisJobID),
		CreatedAt:        row.CreatedAt.Time,
		PostingClaimedAt: row.PostingClaimedAt.pointer(),
		PostedAt:         row.PostedAt.pointer(),
		RetiredAt:        row.RetiredAt.pointer(),
		Outcome:          cloneStringPointer(row.Outcome),
		FirstAttemptAt:   row.FirstAttemptAt.pointer(),
		AttemptCount:     cloneInt64Pointer(row.AttemptCount),
		SynthesisAgent:   cloneStringPointer(row.SynthesisAgent),
		SynthesisModel:   cloneStringPointer(row.SynthesisModel),
		AllowStalePost:   row.AllowStalePost,
	}
	return panel
}

type ciReviewAttemptRow struct {
	bun.BaseModel              `bun:"table:ci_pr_review_attempts,alias:ca"`
	ID                         int64  `bun:"id,pk,autoincrement"`
	GithubRepo                 string `bun:"github_repo"`
	PRNumber                   int    `bun:"pr_number"`
	HeadSHA                    string `bun:"head_sha"`
	Attempt                    int    `bun:"attempt"`
	FirstAttemptAt             dbTime `bun:"first_attempt_at"`
	NextAttemptAt              dbTime `bun:"next_attempt_at"`
	LastErrorClass             string `bun:"last_error_class"`
	ConsecutiveGenuineAttempts int    `bun:"consecutive_genuine_attempts"`
	LastErrorExcerpt           string `bun:"last_error_excerpt"`
	LastPanelRunUUID           string `bun:"last_panel_run_uuid"`
	State                      string `bun:"state"`
	UpdatedAt                  dbTime `bun:"updated_at"`
}

func (row ciReviewAttemptRow) toModel() ReviewAttempt {
	return ReviewAttempt{
		ID:                         row.ID,
		GithubRepo:                 row.GithubRepo,
		PRNumber:                   row.PRNumber,
		HeadSHA:                    row.HeadSHA,
		Attempt:                    row.Attempt,
		FirstAttemptAt:             row.FirstAttemptAt.Time,
		NextAttemptAt:              row.NextAttemptAt.pointer(),
		LastErrorClass:             row.LastErrorClass,
		ConsecutiveGenuineAttempts: row.ConsecutiveGenuineAttempts,
		LastErrorExcerpt:           row.LastErrorExcerpt,
		LastPanelRunUUID:           row.LastPanelRunUUID,
		State:                      row.State,
		UpdatedAt:                  row.UpdatedAt.Time,
	}
}

type daemonStateRow struct {
	bun.BaseModel `bun:"table:daemon_state,alias:ds"`
	Key           string `bun:"key,pk"`
	Value         string `bun:"value"`
	UpdatedAt     dbTime `bun:"updated_at"`
}

type syncStateRow struct {
	bun.BaseModel `bun:"table:sync_state,alias:ss"`
	Key           string `bun:"key,pk"`
	Value         string `bun:"value"`
}
