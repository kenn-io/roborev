package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/storage"
)

type GetRepoInput struct {
	Identifier string `query:"identifier" required:"true" doc:"Repository path or display name"`
	ByPath     bool   `query:"by_path" doc:"Identifier was resolved to a filesystem path by the client"`
}
type GetRepoOutput struct{ Body *storage.RepoStats }

type RenameRepoInput struct {
	Body struct {
		Identifier string `json:"identifier" minLength:"1"`
		ByPath     bool   `json:"by_path"`
		Name       string `json:"name" minLength:"1"`
	}
}
type RenameRepoOutput struct {
	Body struct {
		Repo *storage.Repo `json:"repo"`
	}
}

type MoveRepoInput struct {
	Body struct {
		RepoID   int64  `json:"repo_id" minimum:"1"`
		Path     string `json:"path" minLength:"1"`
		Identity string `json:"identity"`
	}
}

type DeleteRepoInput struct {
	Body struct {
		RepoID  int64 `json:"repo_id" minimum:"1"`
		Cascade bool  `json:"cascade"`
	}
}

type MergeReposInput struct {
	Body struct {
		SourceID int64 `json:"source_id" minimum:"1"`
		TargetID int64 `json:"target_id" minimum:"1"`
	}
}
type MergeReposOutput struct {
	Body struct {
		Moved int64 `json:"moved"`
	}
}

func (s *Server) registerRepoManagementAPI(api huma.API) {
	huma.Get(api, "/api/repos/detail", s.humaGetRepo, func(o *huma.Operation) {
		o.OperationID = "get-repo"
		o.Summary = "Get repository details and statistics"
		o.Tags = []string{"repos"}
	})
	huma.Post(api, "/api/repos/rename", s.humaRenameRepo, func(o *huma.Operation) {
		o.OperationID = "rename-repo"
		o.Summary = "Rename a repository's display name"
		o.Tags = []string{"repos"}
	})
	huma.Post(api, "/api/repos/move", s.humaMoveRepo, func(o *huma.Operation) {
		o.OperationID = "move-repo"
		o.Summary = "Update a repository's path and identity"
		o.Tags = []string{"repos"}
		o.Errors = []int{http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError}
	})
	huma.Post(api, "/api/repos/delete", s.humaDeleteRepo, func(o *huma.Operation) {
		o.OperationID = "delete-repo"
		o.Summary = "Delete a repository and optionally its reviews"
		o.Tags = []string{"repos"}
		o.Errors = []int{http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError}
	})
	huma.Post(api, "/api/repos/merge", s.humaMergeRepos, func(o *huma.Operation) {
		o.OperationID = "merge-repos"
		o.Summary = "Move reviews to another repository and delete the source"
		o.Tags = []string{"repos"}
	})
}

func (s *Server) humaGetRepo(_ context.Context, input *GetRepoInput) (*GetRepoOutput, error) {
	var repo *storage.Repo
	var err error
	if input.ByPath {
		repo, err = s.db.FindRepo(input.Identifier)
	} else {
		repo, err = s.db.GetRepoByName(input.Identifier)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound(fmt.Sprintf("repository not found: %s", input.Identifier))
		}
		return nil, huma.Error500InternalServerError(fmt.Sprintf("find repository: %v", err))
	}
	stats, err := s.db.GetRepoStats(repo.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("get stats: %v", err))
	}
	return &GetRepoOutput{Body: stats}, nil
}

func (s *Server) humaRenameRepo(_ context.Context, input *RenameRepoInput) (*RenameRepoOutput, error) {
	var affected int64
	var err error
	if input.Body.ByPath {
		affected, err = s.db.RenameRepo(input.Body.Identifier, input.Body.Name)
	} else {
		affected, err = s.db.RenameRepoByName(input.Body.Identifier, input.Body.Name)
	}
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("rename repo: %v", err))
	}
	if affected == 0 {
		return nil, huma.Error404NotFound(fmt.Sprintf("no repository found matching %q", input.Body.Identifier))
	}
	repo, err := s.db.GetRepoByName(input.Body.Name)
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("get renamed repository: %v", err))
	}
	output := &RenameRepoOutput{}
	output.Body.Repo = repo
	return output, nil
}

func (s *Server) humaMoveRepo(_ context.Context, input *MoveRepoInput) (*struct{}, error) {
	if _, err := s.db.GetRepoByID(input.Body.RepoID); err != nil {
		return nil, repoManagementError(err)
	}
	if err := s.db.MoveRepo(input.Body.RepoID, input.Body.Path, input.Body.Identity); err != nil {
		if errors.Is(err, storage.ErrRepoPathConflict) {
			return nil, huma.Error409Conflict(fmt.Sprintf("another repository is already at %s; consider 'roborev repo merge' to combine them", input.Body.Path))
		}
		return nil, huma.Error500InternalServerError(fmt.Sprintf("move repo: %v", err))
	}
	return &struct{}{}, nil
}

func (s *Server) humaDeleteRepo(_ context.Context, input *DeleteRepoInput) (*struct{}, error) {
	if err := s.db.DeleteRepo(input.Body.RepoID, input.Body.Cascade); err != nil {
		if errors.Is(err, storage.ErrRepoHasJobs) {
			return nil, huma.Error409Conflict("cannot delete repository with existing jobs (use --cascade)")
		}
		return nil, repoManagementError(err)
	}
	return &struct{}{}, nil
}

func (s *Server) humaMergeRepos(_ context.Context, input *MergeReposInput) (*MergeReposOutput, error) {
	if input.Body.SourceID == input.Body.TargetID {
		return nil, huma.Error400BadRequest("source and target are the same repository")
	}
	for _, id := range []int64{input.Body.SourceID, input.Body.TargetID} {
		if _, err := s.db.GetRepoByID(id); err != nil {
			return nil, repoManagementError(err)
		}
	}
	moved, err := s.db.MergeRepos(input.Body.SourceID, input.Body.TargetID)
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("merge repos: %v", err))
	}
	output := &MergeReposOutput{}
	output.Body.Moved = moved
	return output, nil
}

func repoManagementError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return huma.Error404NotFound("repository not found")
	}
	return huma.Error500InternalServerError(err.Error())
}
