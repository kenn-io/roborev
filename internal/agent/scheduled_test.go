package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScheduledReadOnlyCapabilityAdmitsOnlyKnownAdapters(t *testing.T) {
	assert.True(t, SupportsScheduledReadOnly(NewTestAgent()))
	assert.False(t, SupportsScheduledReadOnly(NewOpenCodeAgent("opencode")))
	assert.False(t, SupportsScheduledReadOnly(nil))
	assert.False(t, SupportsScheduledReadOnly(&unknownScheduledAgent{}))
}

func TestScheduledPermissionWriteWaitsForInvocationLock(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		unlock := ScheduledExecutionLock()
		close(started)
		<-release
		unlock()
		close(done)
	}()
	<-started
	writerDone := make(chan struct{})
	go func() {
		WithScheduledPermissionWriteLock(func() {})
		close(writerDone)
	}()
	time.Sleep(20 * time.Millisecond)
	finished := false
	select {
	case <-writerDone:
		finished = true
	default:
	}
	assert.False(t, finished, "permission reload acquired the write lock during an invocation")
	close(release)
	<-done
	require.Eventually(t, func() bool {
		select {
		case <-writerDone:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}

type unknownScheduledAgent struct{ TestAgent }

func (*unknownScheduledAgent) Name() string { return "unknown" }
