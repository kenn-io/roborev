package review

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRetrySchedule(t *testing.T) {
	assert := assert.New(t)
	s := DefaultRetrySchedule // base 2m, cap 1h, transient cap 72h, genuineMax 3
	assert.Equal(2*time.Minute, s.NextDelay(1))
	assert.Equal(4*time.Minute, s.NextDelay(2))
	assert.Equal(8*time.Minute, s.NextDelay(3))
	assert.Equal(time.Hour, s.NextDelay(20))   // capped
	assert.Equal(time.Hour, s.NextDelay(1000)) // capped, no overflow
	assert.False(s.TransientExhausted(71 * time.Hour))
	assert.True(s.TransientExhausted(73 * time.Hour))
	assert.False(s.GenuineExhausted(2))
	assert.True(s.GenuineExhausted(3))
}

func TestNextDelayClampsLowAttempts(t *testing.T) {
	assert := assert.New(t)
	s := DefaultRetrySchedule
	assert.Equal(2*time.Minute, s.NextDelay(0))
	assert.Equal(2*time.Minute, s.NextDelay(-5))
}

func TestNextDelayCapBoundary(t *testing.T) {
	assert := assert.New(t)
	s := DefaultRetrySchedule
	// Doubling sequence: 2,4,8,16,32,64(->capped to 60).
	assert.Equal(16*time.Minute, s.NextDelay(4))
	assert.Equal(32*time.Minute, s.NextDelay(5))
	assert.Equal(time.Hour, s.NextDelay(6))
}

func TestTransientExhaustedBoundary(t *testing.T) {
	assert := assert.New(t)
	s := DefaultRetrySchedule
	assert.False(s.TransientExhausted(72 * time.Hour))
	assert.True(s.TransientExhausted(72*time.Hour + time.Nanosecond))
}

func TestGenuineExhaustedBoundary(t *testing.T) {
	assert := assert.New(t)
	s := DefaultRetrySchedule
	assert.False(s.GenuineExhausted(0))
	assert.True(s.GenuineExhausted(4))
}

func TestRetryScheduleUsesItsOwnFields(t *testing.T) {
	assert := assert.New(t)
	s := RetrySchedule{Base: time.Second, Cap: 4 * time.Second, TransientWall: 10 * time.Minute, GenuineMax: 2}
	assert.Equal(time.Second, s.NextDelay(1))
	assert.Equal(2*time.Second, s.NextDelay(2))
	assert.Equal(4*time.Second, s.NextDelay(3))          // 8s raw, clamped to custom 4s Cap
	assert.Equal(4*time.Second, s.NextDelay(50))         // stays at custom Cap
	assert.False(s.TransientExhausted(10 * time.Minute)) // exactly the wall: not exhausted
	assert.True(s.TransientExhausted(10*time.Minute + time.Nanosecond))
	assert.False(s.GenuineExhausted(1))
	assert.True(s.GenuineExhausted(2)) // custom GenuineMax
}
