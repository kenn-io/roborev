package agenthook

import (
	"errors"
	"fmt"
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
	Head         string    `json:"head,omitempty"`
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
		Head:         scope.Head,
		StartedAt:    now,
		ExpiresAt:    now.Add(FixSessionLifetime),
	}
	fixSessions[lineage] = fixSession
	return fixSession, true
}

func cloneFixSessions(fixSessions map[string]FixSession) map[string]FixSession {
	cloned := maps.Clone(fixSessions)
	if cloned == nil {
		cloned = map[string]FixSession{}
	}
	return cloned
}

func activeFixSessionLineage(
	fixSessions map[string]FixSession,
	scope hookScope,
	lineage string,
	now time.Time,
) string {
	if current, ok := fixSessions[lineage]; ok && current.Active(now) {
		return lineage
	}
	for candidate, current := range fixSessions {
		if !current.Active(now) || current.Branch != "" || current.Head == "" {
			continue
		}
		if current.WorktreeRoot != scope.WorktreeRoot {
			continue
		}
		if refReachableFromHead(scope.WorktreeRoot, current.Head, scope.Head) {
			return candidate
		}
	}
	return lineage
}

// prepareFixSessionGrantLocked returns whether the reminder may be delivered.
// Unprofiled requests cannot form an owner identity. All hook entry points
// assign a profile before recording. Cursor cannot receive the control output,
// so it does not claim ownership.
func (s *StateStore) prepareFixSessionGrantLocked(
	req Request,
	scope hookScope,
	lineage string,
	now time.Time,
) (map[string]FixSession, *FixSession, bool) {
	if req.Agent == "" || req.Agent == "cursor" {
		return s.fixSessions, nil, true
	}
	lineage = activeFixSessionLineage(s.fixSessions, scope, lineage, now)
	fixSessions := cloneFixSessions(s.fixSessions)
	fixSession, granted := tryGrantFixSession(
		fixSessions, req, scope, lineage, now, uuid.New(),
	)
	if !granted {
		return s.fixSessions, nil, false
	}
	return fixSessions, new(fixSession), true
}

func (s *StateStore) activeOwnerFixSessionLocked(
	req Request,
	scope hookScope,
	now time.Time,
) (FixSession, bool) {
	if req.Agent == "" || req.Agent == "cursor" {
		return FixSession{}, false
	}
	state := cloneSessionState(s.sessions[req.Event.SessionID])
	lineage := ensureLineageKey(&state, scope)
	lineage = activeFixSessionLineage(s.fixSessions, scope, lineage, now)
	fixSession, ok := s.fixSessions[lineage]
	if !ok || !fixSession.Active(now) {
		return FixSession{}, false
	}
	return fixSession, fixSession.Agent == req.Agent && fixSession.SessionID == req.Event.SessionID
}

func ownerStopReason(fixSession FixSession) string {
	return fmt.Sprintf(
		"Finish the current Agent Hook fix, then run `roborev agent-hook fix-done %s`.",
		fixSession.ID,
	)
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
