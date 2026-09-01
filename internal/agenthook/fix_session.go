package agenthook

import (
	"maps"
	"time"
	"uuid"

	kitagenthook "go.kenn.io/kit/agenthook"
)

const FixSessionLifetime = 12 * time.Hour

type FixSession struct {
	ID        uuid.UUID          `json:"id"`
	Agent     kitagenthook.Agent `json:"agent"`
	SessionID string             `json:"session_id"`
	ExpiresAt time.Time          `json:"expires_at"`
}

func (f FixSession) Active(now time.Time) bool {
	return now.Before(f.ExpiresAt)
}

// prepareFixSessionGrantLocked returns whether the reminder may be delivered.
// Unprofiled requests cannot form an owner identity. All hook entry points
// assign a profile before recording. Cursor cannot receive the control output,
// so it does not claim ownership.
func (s *StateStore) prepareFixSessionGrantLocked(
	req Request,
	key string,
	now time.Time,
) (map[string]FixSession, *FixSession, bool) {
	if req.Agent == "" || req.Agent == kitagenthook.AgentCursor {
		return s.fixSessions, nil, true
	}
	fixSessions := maps.Clone(s.fixSessions)
	if fixSessions == nil {
		fixSessions = map[string]FixSession{}
	}
	for worktreeKey, fixSession := range fixSessions {
		if !fixSession.Active(now) {
			delete(fixSessions, worktreeKey)
		}
	}
	if _, exists := fixSessions[key]; exists {
		return s.fixSessions, nil, false
	}
	fixSession := FixSession{
		ID: uuid.New(), Agent: req.Agent, SessionID: req.Event.SessionID,
		ExpiresAt: now.Add(FixSessionLifetime),
	}
	fixSessions[key] = fixSession
	return fixSessions, new(fixSession), true
}

func (s *StateStore) activeOwnerFixSessionLocked(
	req Request,
	scope hookScope,
	now time.Time,
) (FixSession, bool) {
	if req.Agent == "" || req.Agent == kitagenthook.AgentCursor {
		return FixSession{}, false
	}
	fixSession, ok := s.fixSessions[scope.WorktreeKey]
	if !ok || !fixSession.Active(now) {
		return FixSession{}, false
	}
	return fixSession, fixSession.Agent == req.Agent && fixSession.SessionID == req.Event.SessionID
}

func (s *StateStore) CompleteFixSession(id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for worktreeKey, fixSession := range s.fixSessions {
		if fixSession.ID != id {
			continue
		}
		previous := s.fixSessions
		s.fixSessions = maps.Clone(s.fixSessions)
		delete(s.fixSessions, worktreeKey)
		if err := s.saveLocked(); err != nil {
			s.fixSessions = previous
			return err
		}
		return nil
	}
	return nil
}

func (s *StateStore) currentTime() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}
