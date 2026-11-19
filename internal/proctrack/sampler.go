package proctrack

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// ErrMemoryBreach is the cause of a scope context canceled for exceeding the
// memory limit. *Breach wraps it.
var ErrMemoryBreach = errors.New("agent memory limit exceeded")

// Breach records the sample that exceeded an attempt's memory limit.
type Breach struct {
	Limit   uint64 // bytes
	RSS     uint64 // summed RSS of the attempt's processes, bytes
	Largest Proc
}

func (b *Breach) Error() string {
	gib := func(n uint64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }
	return fmt.Sprintf("agent processes' summed RSS reached %s, over agent_memory_max_mb (%d MiB); largest: %s (pid %d) at %s",
		gib(b.RSS), b.Limit>>20, b.Largest.Name, b.Largest.PID, gib(b.Largest.RSS))
}

func (b *Breach) Unwrap() error { return ErrMemoryBreach }

// sampleInterval is a target cadence; a slow scan delays the next one.
var sampleInterval = 2 * time.Second

var sampling struct {
	mu       sync.Mutex
	trackers map[*tracker]struct{}
	running  bool
}

func startSampling(t *tracker) {
	sampling.mu.Lock()
	defer sampling.mu.Unlock()
	if sampling.trackers == nil {
		sampling.trackers = map[*tracker]struct{}{}
	}
	sampling.trackers[t] = struct{}{}
	if !sampling.running {
		sampling.running = true
		go sampleLoop()
	}
}

func stopSampling(t *tracker) {
	sampling.mu.Lock()
	delete(sampling.trackers, t)
	sampling.mu.Unlock()
}

// sampleLoop reads the process table once per interval for all limited
// attempts, and exits when none remain.
func sampleLoop() {
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()
	var lastErr string
	for range ticker.C {
		sampling.mu.Lock()
		if len(sampling.trackers) == 0 {
			sampling.running = false
			sampling.mu.Unlock()
			return
		}
		trackers := make([]*tracker, 0, len(sampling.trackers))
		for t := range sampling.trackers {
			trackers = append(trackers, t)
		}
		sampling.mu.Unlock()

		scanned := time.Now()
		table, unreadable, err := readTable()
		if err != nil {
			if err.Error() != lastErr {
				log.Printf("agent memory limit: %v", err)
				lastErr = err.Error()
			}
			continue
		}
		lastErr = ""
		for _, t := range trackers {
			t.sample(table, unreadable, scanned)
		}
	}
}

// sample sums the attempt's RSS in one table snapshot and, over the limit,
// records a breach on the innermost open scope, then stops and cancels it.
//
// A root registered after the scan began is left for the next sample: its PID
// may name an older process in this snapshot.
func (t *tracker) sample(table map[int]Proc, unreadable map[int]struct{}, scanned time.Time) {
	t.mu.Lock()
	scopes := t.attempt.treeLocked()
	seen := map[ident]struct{}{}
	var total uint64
	var largest Proc
	var target *Scope
	for _, s := range scopes {
		if s.closed {
			continue
		}
		target = s
		groups := make(map[int]struct{}, len(s.roots))
		for pgid, r := range s.roots {
			if r.registered.Before(scanned) {
				groups[pgid] = struct{}{}
			}
		}
		found := collect(table, groups, s.remembered)
		s.remembered = found
		for id, p := range found {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			total += p.RSS
			if p.RSS > largest.RSS {
				largest = p
			}
		}
		for pid := range s.roots {
			if _, ok := unreadable[pid]; ok && !t.warned[pid] {
				t.warned[pid] = true
				log.Printf("agent memory limit: cannot read job process %d; its memory is not counted", pid)
			}
		}
	}
	if target == nil || total <= t.limit || target.breach != nil {
		t.mu.Unlock()
		return
	}
	b := &Breach{Limit: t.limit, RSS: total, Largest: largest}
	target.breach = b
	t.mu.Unlock()
	go func() {
		if err := target.cleanup(); err != nil {
			log.Printf("agent memory limit: %v", err)
		}
		target.cancel(b)
	}()
}
