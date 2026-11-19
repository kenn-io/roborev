package proctrack

// Proc is one process as the process table reports it.
type Proc struct {
	PID, PPID, PGID int
	// Start is the process start time in a platform unit. Together with PID it
	// identifies one process; a reused PID has a different start time.
	Start uint64
	RSS   uint64 // bytes
	Name  string
}

type ident struct {
	pid   int
	start uint64
}

// collect finds a scope's processes in one table snapshot: members of the
// owned groups, roots that left their group, live remembered descendants, and
// every descendant of those by parent PID. It returns the found set, which the
// caller keeps as the new remembered descendants.
func collect[G, R any](table map[int]Proc, groups map[int]G, remembered map[ident]R) map[ident]Proc {
	found := map[ident]Proc{}
	add := func(p Proc) bool {
		id := ident{p.PID, p.Start}
		if _, ok := found[id]; ok {
			return false
		}
		found[id] = p
		return true
	}
	children := map[int][]Proc{}
	for _, p := range table {
		children[p.PPID] = append(children[p.PPID], p)
		if _, ok := groups[p.PGID]; ok {
			add(p)
		}
	}
	// A root is unreaped while its group is owned, so its PID is still its own.
	for pgid := range groups {
		if p, ok := table[pgid]; ok {
			add(p)
		}
	}
	for id := range remembered {
		if p, ok := table[id.pid]; ok && p.Start == id.start {
			add(p)
		}
	}
	queue := make([]Proc, 0, len(found))
	for _, p := range found {
		queue = append(queue, p)
	}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p.PID] {
			if c.PID != p.PID && add(c) {
				queue = append(queue, c)
			}
		}
	}
	return found
}
