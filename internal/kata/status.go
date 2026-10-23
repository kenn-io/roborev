package kata

import (
	"context"
	"errors"

	"go.kenn.io/roborev/internal/config"
)

// State is the condition of the daemon's Kata connection.
type State string

// Connection states. Ready needs a compatible API schema and the project;
// typed replies need a newer schema, reported by Status.SupportsReplies.
const (
	StateDisabled               State = "disabled"
	StateUnreachable            State = "unreachable"
	StateAuthenticationRequired State = "authentication_required"
	StateIncompatible           State = "incompatible"
	StateProjectNotFound        State = "project_not_found"
	StateReady                  State = "ready"
)

// Status is what EvaluateWith learned about the connection. It carries no
// credential and no endpoint, so it is safe to log and to return from the
// daemon API.
type Status struct {
	State State `json:"state"`
	// Project is the project in effect for the workspace: its binding, else the
	// configured default.
	Project          string `json:"project,omitempty"`
	ServerVersion    string `json:"server_version,omitempty"`
	APISchemaVersion string `json:"api_schema_version,omitempty"`
	Message          string `json:"message"`
	SecurityNote     string `json:"security_note,omitempty"`
}

// RepliesAPISchemaVersion is the first API schema that stores typed comment
// replies (reply, confirm, refute, supersede).
const RepliesAPISchemaVersion = "0.28.0"

// SupportsReplies reports whether the connection is ready and the server
// stores typed replies.
func (s Status) SupportsReplies() bool {
	return s.State == StateReady && schemaAtLeast(s.APISchemaVersion, RepliesAPISchemaVersion)
}

// EvaluateWith checks the connection without writing anything: the server's
// health and API schema, then the project. An empty project is the workspace's
// binding, else the configured default; a named one replaces both. Options
// apply to the client it builds.
func EvaluateWith(ctx context.Context, workdir string, cfg config.KataIntegrationConfig, project string, opts ...APIOption) Status {
	if !cfg.Enabled {
		return Status{State: StateDisabled, Message: "The Kata API connection is disabled; roborev runs the kata CLI."}
	}
	c, err := NewAPIClient(workdir, cfg, opts...)
	if err != nil {
		return statusForError(err, project)
	}
	defer c.CloseIdleConnections()
	return c.EvaluateProject(ctx, project)
}

// EvaluateProject checks this client's connection for project, or for the
// workspace's project when it is empty.
func (c *APIClient) EvaluateProject(ctx context.Context, project string) Status {
	result := Status{SecurityNote: c.SecurityNote()}
	info, err := c.refreshInfo(ctx)
	result.ServerVersion, result.APISchemaVersion = info.Version, info.APISchemaVersion
	if err != nil {
		return withFacts(statusForError(err, ""), result)
	}
	if project == "" {
		binding, err := c.Binding(ctx)
		if err != nil {
			if errors.Is(err, ErrNoBinding) {
				result.State = StateProjectNotFound
				result.Message = "The workspace has no Kata binding and integrations.kata.default_project is not set."
				return result
			}
			result.State = StateIncompatible
			result.Message = "The workspace's Kata binding file could not be read."
			return result
		}
		project = binding.Project
	}
	result.Project = project
	projects, err := c.refreshProjects(ctx)
	if err != nil {
		return withFacts(statusForError(err, project), result)
	}
	if _, ok := projects[project]; !ok {
		return withFacts(statusForError(ErrProjectNotFound, project), result)
	}
	result.State = StateReady
	result.Message = "The Kata connection is ready."
	return result
}

// withFacts keeps the server facts already learned on a classified status.
func withFacts(status, facts Status) Status {
	status.ServerVersion, status.APISchemaVersion = facts.ServerVersion, facts.APISchemaVersion
	status.SecurityNote = facts.SecurityNote
	if status.Project == "" {
		status.Project = facts.Project
	}
	return status
}

func statusForError(err error, project string) Status {
	s := Status{Project: project}
	switch {
	case errors.Is(err, ErrPlatformSecurityLimit):
		s.State = StateIncompatible
		s.Message = "This platform cannot safely use the configured Unix socket endpoint."
		s.SecurityNote = "Socket ownership and mode checks are unavailable here, so the endpoint was refused."
	case errors.Is(err, ErrInsecureEndpoint):
		s.State = StateIncompatible
		s.Message = err.Error()
	case errors.Is(err, ErrAuthenticationRequired):
		s.State = StateAuthenticationRequired
		s.Message = "Kata rejected the credential, or no usable credential is configured."
	case errors.Is(err, ErrProjectNotFound), errors.Is(err, ErrNoBinding):
		s.State = StateProjectNotFound
		s.Message = "The Kata project was not found on the server."
	case errors.Is(err, ErrIncompatible), errors.Is(err, ErrInvalidResponse),
		errors.Is(err, ErrRedirect), errors.Is(err, ErrRequestRejected), errors.Is(err, ErrNotFound):
		s.State = StateIncompatible
		s.Message = "The server is reachable or configured but is not a compatible Kata API (needs API schema " + MinAPISchemaVersion + " or later)."
	default:
		s.State = StateUnreachable
		s.Message = "The Kata server is unavailable."
	}
	return s
}
