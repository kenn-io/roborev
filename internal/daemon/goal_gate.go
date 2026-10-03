package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/prompt"
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
	slots  chan struct{}
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newGoalGate(workers int) *goalGate {
	ctx, cancel := context.WithCancel(context.Background())
	return &goalGate{ctx: ctx, cancel: cancel, slots: make(chan struct{}, max(workers, 1))}
}

func (g *goalGate) admit() (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.ctx.Err() != nil {
		return nil, fmt.Errorf("goal review gate is draining")
	}
	select {
	case g.slots <- struct{}{}:
	default:
		return nil, fmt.Errorf("goal review gate is full")
	}
	g.wg.Add(1)
	return func() { <-g.slots; g.wg.Done() }, nil
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
	prepared, err := prompt.NewBuilderWithConfig(s.db, cfg).ForRepo(root, 0).Prepare(
		goalreview.BuildPrompt(snapshot),
		prompt.SnapshotTarget{RepoPath: root, ConfigRepoPath: root},
	)
	if err != nil {
		return completedError(err)
	}
	if prepared.Cleanup != nil {
		defer prepared.Cleanup()
	}
	a, _, err := goalreview.ResolveAgent(root, cfg, goalreview.AgentOptions{})
	if err != nil {
		return completedError(err)
	}
	runner := s.goalReviewRunner
	if runner == nil {
		runner = goalreview.RunPrepared
	}
	response.Findings, err = runner(runCtx, a, root, snapshot, prepared, io.Discard)
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
