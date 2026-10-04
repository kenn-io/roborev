package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

type GoalGateRequest struct {
	RepoPath  string                `json:"repo_path"`
	SpecFile  *string               `json:"spec_file,omitempty"`
	PlanFile  *string               `json:"plan_file,omitempty"`
	Candidate *goalreview.Candidate `json:"candidate,omitempty"`
}

type GoalGateResponse struct {
	Version    int                  `json:"version"`
	Status     string               `json:"status"`
	Mode       string               `json:"mode"`
	Blocked    bool                 `json:"blocked"`
	SnapshotID string               `json:"snapshot_id,omitempty"`
	Findings   []goalreview.Finding `json:"findings"`
	Error      string               `json:"error,omitempty"`
}

type goalGateInput struct {
	RawBody []byte
}
type goalGateOutput struct {
	Status int
	Body   GoalGateResponse
}

type goalGate struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newGoalGate() *goalGate {
	ctx, cancel := context.WithCancel(context.Background())
	return &goalGate{ctx: ctx, cancel: cancel}
}

func (g *goalGate) admit() (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.ctx.Err() != nil {
		return nil, fmt.Errorf("goal review gate is draining")
	}
	g.wg.Add(1)
	return g.wg.Done, nil
}

func (g *goalGate) stop() {
	g.mu.Lock()
	g.closed = true
	g.cancel()
	g.mu.Unlock()
	g.wg.Wait()
}

func (s *Server) humaGoalGate(ctx context.Context, input *goalGateInput) (*goalGateOutput, error) {
	response := GoalGateResponse{Version: 1, Mode: "block", Status: "error", Blocked: true, Findings: []goalreview.Finding{}}
	requestError := func(status int, err error) (*goalGateOutput, error) {
		response.Error = err.Error()
		if response.Findings == nil {
			response.Findings = []goalreview.Finding{}
		}
		response.Blocked = response.Mode == "block"
		return &goalGateOutput{Status: status, Body: response}, nil
	}
	if len(input.RawBody) == 0 {
		return requestError(http.StatusBadRequest, fmt.Errorf("request body is required"))
	}
	var req GoalGateRequest
	if err := goalreview.DecodeStrict(string(input.RawBody), &req); err != nil {
		return requestError(400, err)
	}
	if strings.TrimSpace(req.RepoPath) == "" {
		return requestError(400, fmt.Errorf("repo_path is required"))
	}
	if req.Candidate != nil {
		if strings.TrimSpace(req.Candidate.Title) == "" || strings.TrimSpace(req.Candidate.Body) == "" {
			return requestError(400, fmt.Errorf("candidate title and body are required"))
		}
		for _, link := range req.Candidate.Links {
			if strings.TrimSpace(link.ToRef) == "" || (link.Type != "parent" && link.Type != "blocks" && link.Type != "related") || (link.Incoming && link.Type != "blocks") {
				return requestError(400, fmt.Errorf("invalid candidate link"))
			}
		}
	}
	completedError := func(err error) (*goalGateOutput, error) {
		response.Error = err.Error()
		if response.Findings == nil {
			response.Findings = []goalreview.Finding{}
		}
		response.Blocked = response.Mode == "block"
		return &goalGateOutput{Status: http.StatusOK, Body: response}, nil
	}
	s.shutdownDrainMu.Lock()
	draining := s.shutdownDraining
	s.shutdownDrainMu.Unlock()

	root, err := gitrepo.Root(ctx, req.RepoPath)
	if err != nil {
		return requestError(400, err)
	}
	response.Mode = config.ResolveGoalReviewGate(s.configWatcher.Config())
	repo, err := config.LoadRepoConfig(root)
	if err != nil {
		return completedError(err)
	}
	if draining || s.goalGate.ctx.Err() != nil {
		return requestError(503, fmt.Errorf("goal review gate is draining"))
	}
	selection, err := goalreview.Select(repo, req.SpecFile, req.PlanFile)
	if err != nil {
		return requestError(400, err)
	}
	if response.Mode == "off" {
		response.Status = "off"
		response.Blocked = false
		return &goalGateOutput{Status: http.StatusOK, Body: response}, nil
	}
	release, err := s.goalGate.admit()
	if err != nil {
		return requestError(503, err)
	}
	defer release()
	cfg := s.configWatcher.Config()
	timeout := time.Duration(config.ResolveJobTimeout(root, cfg)) * time.Minute
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stopCancel := context.AfterFunc(s.goalGate.ctx, cancel)
	defer stopCancel()
	snapshot, err := goalreview.Capture(runCtx, root, selection, kata.NewCLIClient(root))
	if err != nil {
		return completedError(err)
	}
	if req.Candidate != nil {
		snapshot, err = snapshot.WithCandidate(*req.Candidate)
		if err != nil {
			return requestError(400, err)
		}
	}
	response.SnapshotID = snapshot.ID()
	output, err := s.enqueueGoalReviewWithConfig(runCtx, EnqueueRequest{
		RepoPath: root, ReviewType: config.ReviewTypeGoal, Source: "goal_gate",
	}, root, &snapshot)
	if err != nil {
		return completedError(err)
	}
	created, ok := output.Body.(EnqueueCreatedResponse)
	if output.Status != http.StatusCreated || !ok || created.ReviewJob == nil {
		if failure, ok := output.Body.(ErrorResponse); ok {
			return completedError(fmt.Errorf("%s", failure.Error))
		}
		return completedError(fmt.Errorf("enqueue goal review: HTTP %d", output.Status))
	}
	response.Findings, err = s.waitGoalReview(runCtx, created.ID, snapshot)
	if err != nil {
		return completedError(err)
	}
	response.Status = "pass"
	response.Blocked = false
	if len(response.Findings) > 0 {
		response.Status = "findings"
		response.Blocked = response.Mode == "block"
	}
	return &goalGateOutput{Status: http.StatusOK, Body: response}, nil
}

// waitGoalReview keeps the HTTP contract synchronous while the ordinary worker
// pool owns execution, concurrency, retries and accounting.
func (s *Server) waitGoalReview(ctx context.Context, jobID int64, snapshot goalreview.Snapshot) ([]goalreview.Finding, error) {
	subscriber, events := s.broadcaster.Subscribe("")
	defer s.broadcaster.Unsubscribe(subscriber)
	// Events are best-effort; polling also catches a dropped completion event.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			_, cancelErr := s.humaCancelJob(context.Background(), &CancelJobInput{Body: CancelJobRequest{JobID: jobID}})
			return nil, errors.Join(err, cancelErr)
		}
		job, err := s.db.GetJobByID(jobID)
		if err != nil {
			return nil, err
		}
		switch job.Status {
		case storage.JobStatusDone:
			review, err := s.db.GetReviewByJobID(jobID)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(review.StructuredOutput)
			if err != nil {
				return nil, err
			}
			document, err := structuredreview.Decode(raw)
			if err != nil {
				return nil, err
			}
			return goalreview.FindingsFromDocument(document, snapshot), nil
		case storage.JobStatusFailed, storage.JobStatusCanceled:
			return nil, fmt.Errorf("goal review %s: %s", job.Status, job.Error)
		}
		select {
		case <-ctx.Done():
		case <-events:
		case <-ticker.C:
		}
	}
}

// GoalReview evaluates the synchronous gate. It respects the caller's context
// deadline; the daemon applies its configured job timeout to the evaluation.
func (c *HTTPClient) GoalReview(ctx context.Context, request GoalGateRequest) (*GoalGateResponse, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/goal-review", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := *c.httpClient
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result GoalGateResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&result)
	if resp.StatusCode != http.StatusOK {
		if decodeErr != nil || result.Version != 1 {
			return nil, fmt.Errorf("goal review gate HTTP status %d", resp.StatusCode)
		}
		return &result, fmt.Errorf("goal review gate HTTP status %d: %s", resp.StatusCode, result.Error)
	}
	if decodeErr != nil {
		return nil, decodeErr
	}
	if result.Version != 1 {
		return nil, fmt.Errorf("unsupported goal review gate version %d", result.Version)
	}
	switch result.Status {
	case "pass", "findings", "error", "off":
	default:
		return nil, fmt.Errorf("invalid goal review gate status")
	}
	return &result, nil
}
