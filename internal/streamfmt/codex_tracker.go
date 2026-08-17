package streamfmt

import "strings"

// codexCommandTracker pairs Codex started/completed command events so each
// command is rendered once while tolerating mixed ID coverage.
type codexCommandTracker struct {
	renderedIDs  map[string]struct{}
	startedByID  map[string]string
	startedCount map[string]int
	startedQueue map[string][]string
}

// Observe records a Codex command event and reports whether the normalized
// command text should be rendered.
func (t *codexCommandTracker) Observe(
	eventType, id, cmd string,
) (string, bool) {
	if eventType != "item.started" && eventType != "item.completed" {
		return "", false
	}

	id = strings.TrimSpace(id)
	cmd = strings.TrimSpace(cmd)

	if eventType == "item.started" {
		if cmd == "" {
			return "", false
		}
		if id != "" {
			if t.wasRendered(id) {
				return "", false
			}
			t.markRendered(id)
			t.rememberStarted(id, cmd)
		}
		t.incrementStarted(cmd)
		return cmd, true
	}

	if cmd == "" {
		if id != "" {
			t.clearStartedID(id)
		}
		return "", false
	}

	if id != "" {
		if startedCmd, ok := t.clearStartedID(id); ok && startedCmd == cmd {
			t.markRendered(id)
			return "", false
		}
	}

	if t.startedCount[cmd] > 0 {
		t.decrementStarted(cmd)
		if id == "" {
			t.consumeStartedIDForCommand(cmd)
		} else {
			t.markRendered(id)
		}
		return "", false
	}

	if id != "" {
		if t.wasRendered(id) {
			return "", false
		}
		t.markRendered(id)
	}

	return cmd, true
}

func (t *codexCommandTracker) rememberStarted(id, cmd string) {
	t.ensureMaps()
	t.startedByID[id] = cmd
	t.startedQueue[cmd] = append(t.startedQueue[cmd], id)
}

func (t *codexCommandTracker) incrementStarted(cmd string) {
	if cmd == "" {
		return
	}
	t.ensureMaps()
	t.startedCount[cmd]++
}

func (t *codexCommandTracker) decrementStarted(cmd string) {
	if cmd == "" {
		return
	}
	count := t.startedCount[cmd]
	if count <= 0 {
		return
	}
	if count == 1 {
		delete(t.startedCount, cmd)
		return
	}
	t.startedCount[cmd] = count - 1
}

func (t *codexCommandTracker) clearStartedID(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	cmd, ok := t.startedByID[id]
	if !ok {
		return "", false
	}
	t.decrementStarted(cmd)
	t.removeStartedIDFromQueue(cmd, id)
	delete(t.startedByID, id)
	return cmd, true
}

func (t *codexCommandTracker) consumeStartedIDForCommand(cmd string) {
	if cmd == "" {
		return
	}
	ids := t.startedQueue[cmd]
	if len(ids) == 0 {
		return
	}
	consumed := ids[0]
	if len(ids) == 1 {
		delete(t.startedQueue, cmd)
	} else {
		t.startedQueue[cmd] = ids[1:]
	}
	delete(t.startedByID, consumed)
}

func (t *codexCommandTracker) removeStartedIDFromQueue(cmd, id string) {
	ids := t.startedQueue[cmd]
	for i, v := range ids {
		if v == id {
			t.startedQueue[cmd] = append(ids[:i], ids[i+1:]...)
			if len(t.startedQueue[cmd]) == 0 {
				delete(t.startedQueue, cmd)
			}
			return
		}
	}
}

func (t *codexCommandTracker) wasRendered(id string) bool {
	if id == "" {
		return false
	}
	_, seen := t.renderedIDs[id]
	return seen
}

func (t *codexCommandTracker) markRendered(id string) {
	if id == "" {
		return
	}
	t.ensureMaps()
	t.renderedIDs[id] = struct{}{}
}

func (t *codexCommandTracker) ensureMaps() {
	if t.renderedIDs == nil {
		t.renderedIDs = make(map[string]struct{})
	}
	if t.startedByID == nil {
		t.startedByID = make(map[string]string)
	}
	if t.startedCount == nil {
		t.startedCount = make(map[string]int)
	}
	if t.startedQueue == nil {
		t.startedQueue = make(map[string][]string)
	}
}
