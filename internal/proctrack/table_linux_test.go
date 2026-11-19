package proctrack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStat(t *testing.T) {
	// The command name holds spaces and a ')' of its own.
	line := "4242 (my (odd) cmd) S 17 4200 4200 0 -1 4194560 1 0 0 0 3 4 0 0 20 0 1 0 987654 12345678 300 18446744073709551615"
	p, err := parseStat(4242, []byte(line))
	require.NoError(t, err)
	assert.Equal(t, Proc{PID: 4242, PPID: 17, PGID: 4200, Start: 987654, RSS: 300 * pageSize, Name: "my (odd) cmd"}, p)

	_, err = parseStat(1, []byte("1 (init) S 0"))
	assert.Error(t, err)
}
