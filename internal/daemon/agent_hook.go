package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

type daemonAgentHookSource struct {
	db *storage.DB
}

func (s daemonAgentHookSource) ResolveTrackedRepo(
	ctx context.Context, path, branch string,
) (agenthook.TrackedRepoResolution, bool) {
	if strings.TrimSpace(path) == "" {
		return agenthook.TrackedRepoResolution{}, false
	}
	resolved, err := resolveTrackedRepo(ctx, s.db, path, branch)
	return resolved, err == nil
}

func resolveTrackedRepo(
	ctx context.Context, db *storage.DB, path, branch string,
) (agenthook.TrackedRepoResolution, error) {
	path = strings.TrimSpace(path)
	lookupPath := path
	if repoRoot, err := gitrepo.MainRoot(ctx, path); err == nil {
		lookupPath = repoRoot
	}
	repo, err := db.GetRepoByPath(lookupPath)
	if errors.Is(err, sql.ErrNoRows) {
		return agenthook.TrackedRepoResolution{}, nil
	}
	if err != nil {
		return agenthook.TrackedRepoResolution{}, fmt.Errorf("lookup repo: %w", err)
	}
	if repo.Identity != "" &&
		config.ResolveRepoIdentity(lookupPath, nil) != repo.Identity {
		return agenthook.TrackedRepoResolution{}, nil
	}
	resolved := agenthook.TrackedRepoResolution{
		Tracked:  true,
		RootPath: repo.RootPath,
		Identity: repo.Identity,
		Name:     repo.Name,
	}
	snooze, err := db.ActiveAgentHookSnooze(
		repo.RootPath, path, branch, time.Now(),
	)
	if err != nil {
		return agenthook.TrackedRepoResolution{}, fmt.Errorf("lookup agent hook snooze: %w", err)
	}
	if snooze != nil {
		resolved.SnoozedUntil = snooze.SnoozedUntil
	}
	return resolved, nil
}

func (s daemonAgentHookSource) ListOpenReviewJobs(
	_ context.Context, repoRoot, branch string,
) ([]storage.ReviewJob, bool) {
	opts := []storage.ListJobsOption{
		storage.WithoutPrompt(),
		storage.WithClosed(false),
		storage.WithExcludePanelRole(storage.PanelRoleMember),
	}
	if branch != "" {
		opts = append(opts, storage.WithBranchOrEmpty(branch))
	}
	jobs, err := s.db.ListJobs(
		string(storage.JobStatusDone), repoRoot, 10000, 0, opts...,
	)
	if err != nil {
		return nil, false
	}
	return jobs, true
}

func (s *Server) agentHookStore() (*agenthook.StateStore, error) {
	if s.agentHookStateErr != nil {
		return nil, huma.Error503ServiceUnavailable(
			"Agent Hook state is unavailable: " + s.agentHookStateErr.Error(),
		)
	}
	return s.agentHookState, nil
}

func (s *Server) humaAgentHookEvent(
	ctx context.Context, input *AgentHookEventInput,
) (*AgentHookEventOutput, error) {
	state, err := s.agentHookStore()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Body.Event.SessionID) == "" {
		return nil, huma.Error400BadRequest("session_id is required")
	}
	response, err := state.RecordContext(ctx, input.Body)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("record Agent Hook event: %v", err),
		)
	}
	return &AgentHookEventOutput{Body: response}, nil
}

func (s *Server) humaAgentHookSessions(
	_ context.Context, _ *AgentHookSessionsInput,
) (*AgentHookSessionsOutput, error) {
	state, err := s.agentHookStore()
	if err != nil {
		return nil, err
	}
	resp := &AgentHookSessionsOutput{}
	resp.Body.Sessions = state.Sessions()
	return resp, nil
}

func (s *Server) humaAgentHookReset(
	_ context.Context, input *AgentHookResetInput,
) (*AgentHookResetOutput, error) {
	state, err := s.agentHookStore()
	if err != nil {
		return nil, err
	}
	if !input.Body.All && strings.TrimSpace(input.Body.SessionID) == "" {
		return nil, huma.Error400BadRequest(
			"session_id or all=true is required",
		)
	}
	if err := state.Reset(input.Body.SessionID, input.Body.All); err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("reset Agent Hook state: %v", err),
		)
	}
	resp := &AgentHookResetOutput{}
	resp.Body.OK = true
	return resp, nil
}
