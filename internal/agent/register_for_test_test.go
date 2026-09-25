package agent

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingTB struct {
	fatal    string
	cleanups []func()
}

func (r *recordingTB) Helper()                      {}
func (r *recordingTB) Fatalf(f string, args ...any) { r.fatal = fmt.Sprintf(f, args...) }
func (r *recordingTB) Cleanup(fn func())            { r.cleanups = append(r.cleanups, fn) }

func TestRegisterForTestRemovesAgentOnCleanup(t *testing.T) {
	t.Parallel()
	tb := &recordingTB{}
	RegisterForTest(tb, &FakeAgent{NameStr: "register-for-test-cleanup"})
	_, err := Get("register-for-test-cleanup")
	require.NoError(t, err)

	for _, fn := range tb.cleanups {
		fn()
	}
	_, err = Get("register-for-test-cleanup")
	assert.Error(t, err)
}

func TestRegisterForTestRejectsTakenName(t *testing.T) {
	t.Parallel()
	tb := &recordingTB{}
	RegisterForTest(tb, &FakeAgent{NameStr: "test"})

	assert.Contains(t, tb.fatal, `"test" is already registered`)
	assert.Empty(t, tb.cleanups)
	a, err := Get("test")
	require.NoError(t, err)
	assert.IsType(t, &TestAgent{}, a)
}
