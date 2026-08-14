// Package webfixture creates deterministic SQLite data for browser tests.
package webfixture

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/storage"
)

const (
	CancelJobID      int64 = 50
	CloseReviewJobID int64 = 51
	CommentJobID     int64 = 52
	FailedJobID      int64 = 48
)

const sqliteTime = "2006-01-02 15:04:05"

var baseTime = time.Date(2026, time.August, 1, 8, 0, 0, 0, time.UTC)

type fixtureJob struct {
	id          int
	repoID      int
	status      storage.JobStatus
	jobType     string
	agent       string
	model       string
	branch      string
	source      string
	panelRole   string
	panelMember string
	panelIndex  *int
	panelRun    string
	closed      bool
	verdict     *bool
	output      string
	tokenUsage  string
	invoked     bool
}

// Seed initializes path with the production storage schema and representative
// review data. It refuses to add fixtures to a database that already has rows.
func Seed(path string) (returnErr error) {
	db, err := storage.Open(path)
	if err != nil {
		return fmt.Errorf("open fixture database: %w", err)
	}
	defer func() {
		if err := db.Close(); returnErr == nil && err != nil {
			returnErr = fmt.Errorf("close fixture database: %w", err)
		}
	}()

	var existing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM repos`).Scan(&existing); err != nil {
		return fmt.Errorf("check fixture database: %w", err)
	}
	if existing != 0 {
		return errors.New("database already contains fixture data")
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin fixture transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := insertRepos(tx); err != nil {
		return err
	}
	jobs := fixtureJobs()
	if err := insertCommits(tx, jobs); err != nil {
		return err
	}
	for _, job := range jobs {
		if err := insertJob(tx, job); err != nil {
			return err
		}
		if job.verdict != nil {
			if err := insertReview(tx, job); err != nil {
				return err
			}
		}
	}
	if err := insertComments(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fixture transaction: %w", err)
	}
	return nil
}

func insertRepos(tx *sql.Tx) error {
	repos := []struct {
		id   int
		root string
		name string
	}{
		{1, "/workspace/project-alpha", "project-alpha"},
		{2, "/workspace/project-beta", "project-beta"},
	}
	for _, repo := range repos {
		_, err := tx.Exec(`
			INSERT INTO repos (id, root_path, name, identity, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			repo.id, repo.root, repo.name, "fixture:"+repo.name,
			baseTime.Format(sqliteTime),
		)
		if err != nil {
			return fmt.Errorf("insert repo %s: %w", repo.name, err)
		}
	}
	return nil
}

func insertCommits(tx *sql.Tx, jobs []fixtureJob) error {
	for _, job := range jobs {
		stamp := jobTime(job.id)
		_, err := tx.Exec(`
			INSERT INTO commits
				(id, repo_id, sha, author, subject, timestamp, created_at)
			VALUES (?, ?, ?, 'Fixture Author', ?, ?, ?)`,
			job.id, job.repoID, commitSHA(job.id),
			fmt.Sprintf("Fixture change %02d", job.id),
			stamp.Format(sqliteTime), stamp.Format(sqliteTime),
		)
		if err != nil {
			return fmt.Errorf("insert commit %d: %w", job.id, err)
		}
	}
	return nil
}

func insertJob(tx *sql.Tx, job fixtureJob) error {
	enqueued := jobTime(job.id)
	var started, finished, failure any
	switch job.status {
	case storage.JobStatusRunning:
		started = enqueued.Add(time.Minute).Format(sqliteTime)
	case storage.JobStatusDone, storage.JobStatusFailed,
		storage.JobStatusApplied, storage.JobStatusRebased,
		storage.JobStatusCanceled, storage.JobStatusSkipped:
		started = enqueued.Add(time.Minute).Format(sqliteTime)
		finished = enqueued.Add(time.Duration(2+job.id%7) * time.Minute).Format(sqliteTime)
	}
	if job.status == storage.JobStatusFailed {
		failure = "fixture agent exited before producing a review"
	}

	var tokenUsage any
	if job.tokenUsage != "" {
		tokenUsage = job.tokenUsage
	}
	var source any
	if job.source != "" {
		source = job.source
	}
	var panelRole, panelRun, panelMember any
	if job.panelRole != "" {
		panelRole = job.panelRole
		panelRun = job.panelRun
	}
	if job.panelMember != "" {
		panelMember = job.panelMember
	}
	var panelIndex any
	if job.panelIndex != nil {
		panelIndex = *job.panelIndex
	}

	_, err := tx.Exec(`
		INSERT INTO review_jobs (
			id, repo_id, commit_id, git_ref, branch, agent, model,
			status, enqueued_at, started_at, finished_at, error,
			job_type, source, token_usage, agent_invoked, uuid,
			panel_run_uuid, panel_role, panel_name, panel_member_name,
			panel_member_index, panel_member_config_json, claim_blocked
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, 0
		)`,
		job.id, job.repoID, job.id, jobRef(job), job.branch, job.agent,
		job.model, job.status, enqueued.Format(sqliteTime), started, finished,
		failure, job.jobType, source, tokenUsage, job.invoked,
		fmt.Sprintf("00000000-0000-4000-8000-%012d", job.id),
		panelRun, panelRole, panelName(job), panelMember, panelIndex,
		panelConfig(job),
	)
	if err != nil {
		return fmt.Errorf("insert job %d: %w", job.id, err)
	}
	return nil
}

func insertReview(tx *sql.Tx, job fixtureJob) error {
	closed := 0
	if job.closed {
		closed = 1
	}
	verdict := 0
	if *job.verdict {
		verdict = 1
	}
	output := job.output
	if output == "" {
		if verdict == 1 {
			output = "No issues found. The change follows project conventions."
		} else {
			output = findingOutput(job.id)
		}
	}
	created := jobTime(job.id).Add(10 * time.Minute).Format(sqliteTime)
	_, err := tx.Exec(`
		INSERT INTO reviews
			(job_id, agent, prompt, output, created_at, closed,
			 verdict_bool, uuid, updated_at)
		VALUES (?, ?, 'Review the fixture change', ?, ?, ?, ?, ?, ?)`,
		job.id, job.agent, output, created, closed, verdict,
		fmt.Sprintf("10000000-0000-4000-8000-%012d", job.id), created,
	)
	if err != nil {
		return fmt.Errorf("insert review for job %d: %w", job.id, err)
	}
	return nil
}

func insertComments(tx *sql.Tx) error {
	comments := []struct {
		responder string
		body      string
	}{
		{"maintainer", "Confirmed; the error path needs explicit coverage."},
		{"reviewer", "A focused regression test will address this finding."},
	}
	for index, comment := range comments {
		created := jobTime(int(CommentJobID)).Add(time.Duration(12+index) * time.Minute)
		_, err := tx.Exec(`
			INSERT INTO responses
				(job_id, responder, response, created_at, uuid)
			VALUES (?, ?, ?, ?, ?)`,
			CommentJobID, comment.responder, comment.body,
			created.Format(sqliteTime),
			fmt.Sprintf("20000000-0000-4000-8000-%012d", index+1),
		)
		if err != nil {
			return fmt.Errorf("insert fixture comment %d: %w", index, err)
		}
	}
	return nil
}

func fixtureJobs() []fixtureJob {
	statuses := []storage.JobStatus{
		storage.JobStatusDone, storage.JobStatusDone, storage.JobStatusFailed,
		storage.JobStatusCanceled, storage.JobStatusRunning, storage.JobStatusApplied,
		storage.JobStatusRebased, storage.JobStatusSkipped, storage.JobStatusQueued,
	}
	agents := []struct{ name, model string }{
		{"codex", "fixture-large"},
		{"claude", "fixture-medium"},
		{"gemini", "fixture-fast"},
	}
	branches := []string{"main", "feature/parser", "fix/streaming"}

	jobs := make([]fixtureJob, 0, 56)
	for id := 1; id <= 49; id++ {
		repoID := 1
		if id > 30 {
			repoID = 2
		}
		agent := agents[(id-1)%len(agents)]
		jobType := storage.JobTypeReview
		if id%13 == 0 {
			jobType = storage.JobTypeRange
		} else if id%17 == 0 {
			jobType = storage.JobTypeDirty
		}
		status := statuses[(id-1)%len(statuses)]
		job := fixtureJob{
			id: id, repoID: repoID, status: status, jobType: jobType,
			agent: agent.name, model: agent.model,
			branch: branches[(id-1)%len(branches)],
		}
		if status == storage.JobStatusDone || status == storage.JobStatusApplied || status == storage.JobStatusRebased {
			verdict := id%3 != 0
			job.verdict = &verdict
			job.closed = !verdict && id%2 == 0
		}
		jobs = append(jobs, job)
	}

	pricedVerdict := true
	jobs[44].status = storage.JobStatusDone
	jobs[44].verdict = &pricedVerdict
	jobs[44].tokenUsage = `{"has_cost":true,"cost_usd":1.25,"input_tokens":1200,"total_output_tokens":400}`
	jobs[44].invoked = true
	jobs[45].tokenUsage = `{"has_cost":true,"cost_usd":0,"input_tokens":800,"total_output_tokens":200}`
	jobs[45].invoked = true
	jobs[46].tokenUsage = ""
	jobs[46].invoked = true

	failedVerdict := false
	passVerdict := true
	memberZero := 0
	memberOne := 1
	jobs = append(jobs,
		fixtureJob{id: 50, repoID: 1, status: storage.JobStatusQueued, jobType: storage.JobTypeReview, agent: "codex", model: "fixture-large", branch: "main"},
		fixtureJob{id: 51, repoID: 1, status: storage.JobStatusDone, jobType: storage.JobTypeReview, agent: "claude", model: "fixture-medium", branch: "feature/parser", verdict: &failedVerdict},
		fixtureJob{id: 52, repoID: 1, status: storage.JobStatusDone, jobType: storage.JobTypeReview, agent: "gemini", model: "fixture-fast", branch: "fix/streaming", verdict: &failedVerdict, output: richFindingOutput()},
		fixtureJob{id: 53, repoID: 2, status: storage.JobStatusDone, jobType: storage.JobTypeCompact, agent: "codex", model: "fixture-large", branch: "main", verdict: &passVerdict, output: "No issues found after consolidated review."},
		fixtureJob{id: 54, repoID: 1, status: storage.JobStatusDone, jobType: storage.JobTypeReview, agent: "codex", model: "fixture-large", branch: "main", panelRole: storage.PanelRoleMember, panelMember: "correctness", panelIndex: &memberZero, panelRun: "fixture-panel-run", verdict: &failedVerdict},
		fixtureJob{id: 55, repoID: 1, status: storage.JobStatusFailed, jobType: storage.JobTypeReview, agent: "claude", model: "fixture-medium", branch: "main", panelRole: storage.PanelRoleMember, panelMember: "security", panelIndex: &memberOne, panelRun: "fixture-panel-run"},
		fixtureJob{id: 56, repoID: 1, status: storage.JobStatusDone, jobType: storage.JobTypeSynthesis, agent: "codex", model: "fixture-large", branch: "main", panelRole: storage.PanelRoleSynthesis, panelRun: "fixture-panel-run", verdict: &failedVerdict, output: "## Panel synthesis\n\nThe panel found one error-handling issue."},
	)
	return jobs
}

func jobRef(job fixtureJob) string {
	if job.panelRun != "" {
		return "panel-fixture-ref"
	}
	if job.jobType == storage.JobTypeRange {
		return commitSHA(max(job.id-1, 1)) + ".." + commitSHA(job.id)
	}
	if job.jobType == storage.JobTypeDirty {
		return "dirty"
	}
	return commitSHA(job.id)
}

func panelName(job fixtureJob) any {
	if job.panelRun == "" {
		return nil
	}
	return "fixture panel"
}

func panelConfig(job fixtureJob) any {
	if job.panelRole != storage.PanelRoleMember {
		return nil
	}
	return `{}`
}

func commitSHA(id int) string {
	return fmt.Sprintf("%040x", id)
}

func jobTime(id int) time.Time {
	return baseTime.Add(time.Duration(id*3) * time.Hour)
}

func findingOutput(id int) string {
	return fmt.Sprintf("## Review findings\n\n### Error handling\n\nJob %d discards an error returned by `loadReview()`.\n", id)
}

func richFindingOutput() string {
	var output strings.Builder
	output.WriteString("## Review findings\n\n")
	output.WriteString("### Streaming errors are discarded\n\n")
	output.WriteString("Return the error from the stream reader:\n\n")
	output.WriteString("```go\nif err := readStream(); err != nil {\n")
	output.WriteString("    return fmt.Errorf(\"read stream: %w\", err)\n}\n```\n\n")
	output.WriteString("```mermaid\nflowchart LR\n  Request --> Stream --> Review\n```\n")
	return output.String()
}
