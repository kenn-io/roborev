package proctrack

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pids(found map[ident]Proc) []int {
	var out []int
	for id := range found {
		out = append(out, id.pid)
	}
	return out
}

func TestCollect(t *testing.T) {
	type set = map[int]struct{}
	cases := []struct {
		name       string
		table      []Proc
		groups     set
		remembered map[ident]struct{}
		want       []int
	}{
		{
			name: "group member after its parent exits",
			// 100 is the root; 101 forked 102 and exited, so 102 now has parent 1.
			table:  []Proc{{PID: 100, PPID: 9, PGID: 100}, {PID: 102, PPID: 1, PGID: 100}, {PID: 200, PPID: 1, PGID: 200}},
			groups: set{100: {}},
			want:   []int{100, 102},
		},
		{
			name: "remembered process after setsid and the child it starts later",
			table: []Proc{
				{PID: 100, PPID: 9, PGID: 100},
				{PID: 103, PPID: 1, PGID: 103, Start: 7},
				{PID: 104, PPID: 103, PGID: 103},
				{PID: 300, PPID: 1, PGID: 300},
			},
			groups:     set{100: {}},
			remembered: map[ident]struct{}{{103, 7}: {}},
			want:       []int{100, 103, 104},
		},
		{
			name: "reused PID with another start time is not counted",
			table: []Proc{
				{PID: 100, PPID: 9, PGID: 100},
				{PID: 103, PPID: 1, PGID: 103, Start: 99},
				{PID: 104, PPID: 103, PGID: 103},
			},
			groups:     set{100: {}},
			remembered: map[ident]struct{}{{103, 7}: {}},
			want:       []int{100},
		},
		{
			name:   "root that left its group still counts",
			table:  []Proc{{PID: 100, PPID: 9, PGID: 555}, {PID: 101, PPID: 100, PGID: 555}},
			groups: set{100: {}},
			want:   []int{100, 101},
		},
		{
			name: "exited process is skipped",
			// 103 was remembered but is gone from this snapshot.
			table:      []Proc{{PID: 100, PPID: 9, PGID: 100}},
			groups:     set{100: {}},
			remembered: map[ident]struct{}{{103, 7}: {}},
			want:       []int{100},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := map[int]Proc{}
			for _, p := range tc.table {
				table[p.PID] = p
			}
			assert.ElementsMatch(t, tc.want, pids(collect(table, tc.groups, tc.remembered)))
		})
	}
}

func TestCollectCountsEachIdentityOnce(t *testing.T) {
	// 101 is a group member, a remembered process, and a child of the root.
	table := map[int]Proc{
		100: {PID: 100, PPID: 9, PGID: 100, RSS: 10},
		101: {PID: 101, PPID: 100, PGID: 100, Start: 5, RSS: 20},
	}
	found := collect(table, map[int]struct{}{100: {}}, map[ident]struct{}{{101, 5}: {}})
	var sum uint64
	for _, p := range found {
		sum += p.RSS
	}
	assert.Equal(t, uint64(30), sum)
}

func TestParseTaskAllInfo(t *testing.T) {
	buf := make([]byte, taskAllInfoSize)
	le := binary.LittleEndian
	le.PutUint32(buf[16:], 77)  // pbi_ppid
	le.PutUint32(buf[100:], 70) // pbi_pgid
	copy(buf[48:], "python3")
	le.PutUint64(buf[120:], 1700000000) // pbi_start_tvsec
	le.PutUint64(buf[128:], 250)        // pbi_start_tvusec
	le.PutUint64(buf[144:], 3<<30)      // pti_resident_size

	p, err := parseTaskAllInfo(80, buf, taskAllInfoSize)
	require.NoError(t, err)
	assert.Equal(t, Proc{PID: 80, PPID: 77, PGID: 70, Start: 1700000000_000250, RSS: 3 << 30, Name: "python3"}, p)

	// A short or zero return is a failed read, not a process with zero RSS.
	for _, size := range []int{0, 136} {
		_, err := parseTaskAllInfo(80, buf, size)
		assert.Error(t, err, "size %d", size)
	}
}

func TestBreachMessage(t *testing.T) {
	b := &Breach{
		Limit:   16384 << 20,
		RSS:     18468359372,                                         // 17.2 GiB
		Largest: Proc{PID: 12345, Name: "python3", RSS: 18146236825}, // 16.9 GiB,
	}
	assert.Equal(t, "agent processes' summed RSS reached 17.2 GiB, over agent_memory_max_mb (16384 MiB); largest: python3 (pid 12345) at 16.9 GiB", b.Error())
	assert.ErrorIs(t, b, ErrMemoryBreach)
}
