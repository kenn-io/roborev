package daemon

import (
	"encoding/json"
	"sync"
	"time"
)

// Event represents a review event that can be broadcast
type Event struct {
	Type         string    `json:"type"`
	TS           time.Time `json:"ts"`
	JobID        int64     `json:"job_id"`
	JobUUID      string    `json:"job_uuid,omitempty"`
	Repo         string    `json:"repo"`
	RepoName     string    `json:"repo_name"`
	SHA          string    `json:"sha"`
	Branch       string    `json:"branch,omitempty"`
	Agent        string    `json:"agent,omitempty"`
	Verdict      string    `json:"verdict,omitempty"`
	Findings     string    `json:"findings,omitempty"`
	Error        string    `json:"error,omitempty"`
	WorktreePath string    `json:"worktree_path,omitempty"`
}

// Subscriber represents a client subscribed to events
type Subscriber struct {
	ID       int
	RepoPath string // Filter: only send events for this repo (empty = all)
	Ch       chan Event
}

// Broadcaster interface manages event subscriptions and broadcasting
type Broadcaster interface {
	Subscribe(repoPath string) (int, <-chan Event)
	Unsubscribe(id int)
	Broadcast(event Event)
	SubscriberCount() int
}

// EventBroadcaster implements the Broadcaster interface
type EventBroadcaster struct {
	mu          sync.RWMutex
	subscribers map[int]*Subscriber
	nextID      int
}

// NewBroadcaster creates a new event broadcaster
func NewBroadcaster() Broadcaster {
	return &EventBroadcaster{
		subscribers: make(map[int]*Subscriber),
		nextID:      1,
	}
}

// Subscribe adds a new subscriber with optional repo filter
// Returns a subscriber ID and event channel
func (b *EventBroadcaster) Subscribe(repoPath string) (int, <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.nextID
	b.nextID++

	ch := make(chan Event, 10) // Buffer to prevent blocking
	sub := &Subscriber{
		ID:       id,
		RepoPath: repoPath,
		Ch:       ch,
	}

	b.subscribers[id] = sub
	return id, ch
}

// Unsubscribe removes a subscriber and closes its channel
func (b *EventBroadcaster) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if sub, ok := b.subscribers[id]; ok {
		close(sub.Ch)
		delete(b.subscribers, id)
	}
}

// Broadcast sends an event to all matching subscribers
// Non-blocking: if a subscriber's channel is full, the event is dropped for that subscriber
func (b *EventBroadcaster) Broadcast(event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.subscribers {
		// Apply repo filter if set
		if sub.RepoPath != "" && sub.RepoPath != event.Repo {
			continue
		}

		// Non-blocking send
		select {
		case sub.Ch <- event:
		default:
			// Channel full, drop event for this subscriber
		}
	}
}

// SubscriberCount returns the current number of subscribers (for testing)
func (b *EventBroadcaster) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// MarshalJSON converts an Event to JSON for streaming
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type         string `json:"type"`
		TS           string `json:"ts"`
		JobID        int64  `json:"job_id"`
		JobUUID      string `json:"job_uuid,omitempty"`
		Repo         string `json:"repo"`
		RepoName     string `json:"repo_name"`
		SHA          string `json:"sha"`
		Branch       string `json:"branch,omitempty"`
		Agent        string `json:"agent,omitempty"`
		Verdict      string `json:"verdict,omitempty"`
		Findings     string `json:"findings,omitempty"`
		Error        string `json:"error,omitempty"`
		WorktreePath string `json:"worktree_path,omitempty"`
	}{
		Type:         e.Type,
		TS:           e.TS.UTC().Format(time.RFC3339),
		JobID:        e.JobID,
		JobUUID:      e.JobUUID,
		Repo:         e.Repo,
		RepoName:     e.RepoName,
		SHA:          e.SHA,
		Branch:       e.Branch,
		Agent:        e.Agent,
		Verdict:      e.Verdict,
		Findings:     e.Findings,
		Error:        e.Error,
		WorktreePath: e.WorktreePath,
	})
}
