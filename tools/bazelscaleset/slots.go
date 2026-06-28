package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// slot is a stable per-runner work directory plus its own actions/runner dir. Because
// the work path never changes, the bazel output_base derived from it (output_user_root
// + hash(workspacePath)) is stable too, so the JVM server, loading/analysis cache, local
// action cache, and bazel-out that one job warms up survive into the next ephemeral
// runner that reuses the same slot. This is the warmth half of RFC-155 Option A.
//
// runnerDir is a per-slot clone of the base actions/runner: at maxRunners>1, concurrent
// runners must NOT share one runner dir, because the stock runner writes/removes
// .runner/.credentials there per ephemeral runner and would corrupt each other.
type slot struct {
	index     int
	path      string // work dir (the JIT runner's WorkFolder)
	runnerDir string // per-slot actions/runner dir (own run.sh + .runner/.credentials)
}

// slotPool hands out a fixed pool of warm work-slots, one per concurrent runner.
// At maxRunners=1 there is a single, always-warm slot.
type slotPool struct {
	mu   sync.Mutex
	free []*slot
	all  []*slot
}

// newSlotPool creates n slots under baseDir, each with its own actions/runner dir cloned
// from runnerBase (a clean template runner). Idempotent. Returns a pool with all free.
func newSlotPool(baseDir, runnerBase string, n int) (*slotPool, error) {
	if n < 1 {
		return nil, fmt.Errorf("slot pool needs at least 1 slot, got %d", n)
	}
	p := &slotPool{}
	for i := range n {
		path := filepath.Join(baseDir, fmt.Sprintf("slot-%d", i))
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, fmt.Errorf("creating slot dir %q: %w", path, err)
		}
		runnerDir := fmt.Sprintf("%s-slot%d", runnerBase, i)
		if err := cloneRunnerDir(runnerBase, runnerDir); err != nil {
			return nil, fmt.Errorf("cloning runner dir for slot %d: %w", i, err)
		}
		s := &slot{index: i, path: path, runnerDir: runnerDir}
		p.all = append(p.all, s)
		p.free = append(p.free, s)
	}
	return p, nil
}

// cloneRunnerDir makes a clean per-slot copy of the base actions/runner dir (binaries
// only — no runtime state), so each slot has its own .runner/.credentials and concurrent
// runners can't corrupt each other. Idempotent: a no-op if the clone already has run.sh.
func cloneRunnerDir(src, dst string) error {
	if _, err := os.Stat(filepath.Join(dst, "run.sh")); err == nil {
		return nil // already cloned
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", dst, err)
	}
	// -L dereferences the bin/externals version symlinks; the excludes drop any runtime
	// state so the clone is a clean install (a fresh JIT runner writes its own state).
	cmd := exec.Command("rsync", "-aL", "--delete",
		"--exclude=_work", "--exclude=_diag", "--exclude=.runner", "--exclude=.runner_migrated",
		"--exclude=.credentials", "--exclude=.credentials_rsaparams", "--exclude=.service",
		src+"/", dst+"/")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync %q -> %q: %w: %s", src, dst, err, out)
	}
	return nil
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
