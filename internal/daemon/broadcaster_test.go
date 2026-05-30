package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

// newTestEvent creates an Event with sensible defaults. Override fields via opts.
func newTestEvent(jobID int64, opts ...func(*Event)) Event {
	e := Event{
		Type:    "review.completed",
		TS:      time.Now(),
		JobID:   jobID,
		Repo:    "/test/repo",
		SHA:     "abc123",
		Agent:   "test",
		Verdict: "P",
	}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func assertEventFields(t *testing.T, got Event, expected Event) {
	t.Helper()
	assert.Equal(t, expected.Type, got.Type, "event type")
	assert.Equal(t, expected.JobID, got.JobID, "event job id")
	assert.Equal(t, expected.Repo, got.Repo, "event repo")
	assert.Equal(t, expected.SHA, got.SHA, "event sha")
	assert.Equal(t, expected.Agent, got.Agent, "event agent")
	assert.Equal(t, expected.Verdict, got.Verdict, "event verdict")
}

func assertNoEventWithin(t *testing.T, ch <-chan Event, duration time.Duration) {
	t.Helper()
	// fast path non-blocking check
	select {
	case e := <-ch:
		require.Failf(t, "Received unexpected event", "%+v", e)
		return
	default:
	}

	// timeout-backed check
	select {
	case e := <-ch:
		require.Failf(t, "Received unexpected event", "%+v", e)
	case <-time.After(duration):
		// OK
	}
}

func TestBroadcaster_BroadcastAndSubscribe(t *testing.T) {
	broadcaster := NewBroadcaster()

	_, eventCh := broadcaster.Subscribe("")

	testEvent := newTestEvent(42, func(e *Event) {
		e.Repo = "/path/to/repo"
		e.Agent = "test-agent"
	})
	broadcaster.Broadcast(testEvent)

	received := testutil.ReceiveWithTimeout(t, eventCh, 1*time.Second)
	assertEventFields(t, received, testEvent)
}

func TestStreamEventsWithRepoFilter(t *testing.T) {
	t.Parallel()
	broadcaster := NewBroadcaster()

	_, eventCh := broadcaster.Subscribe("/path/to/repo1")

	broadcaster.Broadcast(newTestEvent(1, func(e *Event) { e.Repo = "/path/to/repo1"; e.SHA = "sha1" }))
	broadcaster.Broadcast(newTestEvent(2, func(e *Event) { e.Repo = "/path/to/repo2"; e.SHA = "sha2"; e.Verdict = "F" }))
	broadcaster.Broadcast(newTestEvent(3, func(e *Event) { e.Repo = "/path/to/repo1"; e.SHA = "sha3" }))

	// Should receive only events for repo1 (JobID 1 and 3)
	e1 := testutil.ReceiveWithTimeout(t, eventCh, 500*time.Millisecond)
	e2 := testutil.ReceiveWithTimeout(t, eventCh, 500*time.Millisecond)

	// Should not receive more events (repo2 event was filtered out)
	assertNoEventWithin(t, eventCh, 100*time.Millisecond)

	assert.Equal(t, int64(1), e1.JobID)
	assert.Equal(t, int64(3), e2.JobID)
}

func TestStreamMultipleEvents(t *testing.T) {
	broadcaster := NewBroadcaster()

	_, eventCh := broadcaster.Subscribe("")

	for i := 1; i <= 3; i++ {
		broadcaster.Broadcast(newTestEvent(int64(i)))
	}

	// Receive all 3 events
	for i := 1; i <= 3; i++ {
		e := testutil.ReceiveWithTimeout(t, eventCh, 500*time.Millisecond)
		assert.Equal(t, int64(i), e.JobID)
	}
}

func TestBroadcaster_MultiSubscriber(t *testing.T) {
	t.Parallel()
	broadcaster := NewBroadcaster()

	_, chAll := broadcaster.Subscribe("")
	_, chRepo1 := broadcaster.Subscribe("/path/to/repo1")
	_, chRepo2 := broadcaster.Subscribe("/path/to/repo2")

	broadcaster.Broadcast(newTestEvent(123, func(e *Event) { e.Repo = "/path/to/repo1" }))

	// catch-all should receive
	e1 := testutil.ReceiveWithTimeout(t, chAll, 500*time.Millisecond)
	assert.Equal(t, int64(123), e1.JobID)

	// exact-match should receive
	e2 := testutil.ReceiveWithTimeout(t, chRepo1, 500*time.Millisecond)
	assert.Equal(t, int64(123), e2.JobID)

	// mismatch should NOT receive
	assertNoEventWithin(t, chRepo2, 100*time.Millisecond)
}

func TestBroadcaster_Subscribe(t *testing.T) {
	b := NewBroadcaster()

	// Subscribe without filter
	id1, ch1 := b.Subscribe("")

	// Subscribe with filter
	id2, ch2 := b.Subscribe("/path/to/repo")

	// Verify IDs are different
	assert.NotEqual(t, id1, id2, "expected distinct subscriber IDs")

	// Verify channels are different
	assert.NotEqual(t, ch1, ch2, "subscriber channels should be different")

	assert.Equal(t, 2, b.SubscriberCount())
}

func TestBroadcaster_Unsubscribe(t *testing.T) {
	b := NewBroadcaster()

	id, ch := b.Subscribe("")

	// Unsubscribe
	b.Unsubscribe(id)

	// Verify channel is closed
	_, ok := <-ch
	assert.False(t, ok, "expected channel to be closed after unsubscribe")

	assert.Zero(t, b.SubscriberCount())
}

func TestBroadcaster_NonBlockingBroadcast(t *testing.T) {
	b := NewBroadcaster()

	_, ch := b.Subscribe("")

	const testBufferSize = 10 // Must match channel buffer size in NewBroadcaster

	// Fill the channel buffer
	for i := range testBufferSize {
		b.Broadcast(Event{JobID: int64(i)})
	}

	// Broadcast one more event - should not block even though channel is full
	done := make(chan bool, 1)
	go func() {
		b.Broadcast(Event{JobID: 999})
		done <- true
	}()

	select {
	case <-done:
		// OK - broadcast didn't block
	case <-time.After(100 * time.Millisecond):
		require.Fail(t, "broadcast blocked when channel was full")
	}

	// Verify we received the first testBufferSize events (not the dropped one)
	for i := range testBufferSize {
		e := <-ch
		assert.Equal(t, int64(i), e.JobID)
	}

	// Channel should be empty now
	assertNoEventWithin(t, ch, 10*time.Millisecond)
}
