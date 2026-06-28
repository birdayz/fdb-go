package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// slot is a stable per-runner work directory. Because its path never changes,
// the bazel output_base derived from it (output_user_root + hash(workspacePath))
// is stable too, so the JVM server, loading/analysis cache, local action cache,
// and bazel-out that one job warms up survive into the next ephemeral runner that
// reuses the same slot. This is the warmth half of RFC-155 Option A.
type slot struct {
	index int
	path  string
}

// slotPool hands out a fixed pool of warm work-slots, one per concurrent runner.
// At maxRunners=1 there is a single, always-warm slot.
type slotPool struct {
	mu   sync.Mutex
	free []*slot
	all  []*slot
}

// newSlotPool creates n slot directories under baseDir (idempotent) and returns a
// pool with every slot free.
func newSlotPool(baseDir string, n int) (*slotPool, error) {
	if n < 1 {
		return nil, fmt.Errorf("slot pool needs at least 1 slot, got %d", n)
	}
	p := &slotPool{}
	for i := range n {
		path := filepath.Join(baseDir, fmt.Sprintf("slot-%d", i))
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, fmt.Errorf("creating slot dir %q: %w", path, err)
		}
		s := &slot{index: i, path: path}
		p.all = append(p.all, s)
		p.free = append(p.free, s)
	}
	return p, nil
}

// take removes and returns a free slot, or nil if none are available.
func (p *slotPool) take() *slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.free) == 0 {
		return nil
	}
	s := p.free[len(p.free)-1]
	p.free = p.free[:len(p.free)-1]
	return s
}

// give returns a slot to the pool.
func (p *slotPool) give(s *slot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free = append(p.free, s)
}

// size reports the total number of slots in the pool.
func (p *slotPool) size() int {
	return len(p.all)
}
