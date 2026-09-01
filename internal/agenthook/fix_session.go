package agenthook

import (
	"errors"
	"maps"
	"time"
	"uuid"
)

const FixSessionLifetime = 12 * time.Hour

var ErrFixSessionNotFound = errors.New("agent hook fix session not found")

type FixSession struct {
	ID           uuid.UUID `json:"id"`
	Agent        string    `json:"agent"`
	SessionID    string    `json:"session_id"`
	WorktreeRoot string    `json:"worktree_root"`
	Branch       string    `json:"branch,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	CompletedAt  time.Time `json:"completed_at,omitzero"`
}

func (f FixSession) Active(now time.Time) bool {
	return f.CompletedAt.IsZero() && now.Before(f.ExpiresAt)
}

func tryGrantFixSession(
	fixSessions map[string]FixSession,
	req Request,
	scope hookScope,
	lineage string,
	now time.Time,
	id uuid.UUID,
) (FixSession, bool) {
	if current, ok := fixSessions[lineage]; ok && current.Active(now) {
		return FixSession{}, false
	}
	fixSession := FixSession{
		ID:           id,
		Agent:        req.Agent,
		SessionID:    req.Event.SessionID,
		WorktreeRoot: scope.WorktreeRoot,
		Branch:       scope.Branch,
		StartedAt:    now,
		ExpiresAt:    now.Add(FixSessionLifetime),
	}
	fixSessions[lineage] = fixSession
	return fixSession, true
}

func (s *StateStore) FixSessions() map[string]FixSession {
	s.mu.Lock()
	defer s.mu.Unlock()

	return maps.Clone(s.fixSessions)
}

func (s *StateStore) CompleteFixSession(id uuid.UUID) (FixSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for lineage, fixSession := range s.fixSessions {
		if fixSession.ID != id {
			continue
		}
		if !fixSession.CompletedAt.IsZero() {
			return fixSession, nil
		}
		previous := s.fixSessions
		s.fixSessions = maps.Clone(s.fixSessions)
		fixSession.CompletedAt = s.currentTime()
		s.fixSessions[lineage] = fixSession
		if err := s.saveLocked(); err != nil {
			s.fixSessions = previous
			return FixSession{}, err
		}
		return fixSession, nil
	}
	return FixSession{}, ErrFixSessionNotFound
}

func (s *StateStore) currentTime() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}
