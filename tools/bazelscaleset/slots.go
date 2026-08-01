package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
//
// A slot with a non-empty host is REMOTE: its runner executes on that pool box over
// SSH, and both path (the JIT WorkFolder) and runnerDir are paths ON THAT HOST. A
// remote host runs at most one slot, so it uses the box's extracted actions/runner
// directly — no clone (cloud-init owns that install). Remote slots have no local
// directories at all, which keeps reconcileStrayRunners's workBase/slot-* scan
// local-only by construction.
type slot struct {
	index     int
	path      string // work dir (the JIT runner's WorkFolder); on host for remote slots
	runnerDir string // actions/runner dir (own run.sh); on host for remote slots
	host      string // "" = local; else user@host executed over SSH
}

// remoteSlotConfig maps slots 1..len(hosts) onto pool boxes.
type remoteSlotConfig struct {
	hosts     []string // user@host, in slot order (slot i>0 -> hosts[i-1])
	runnerDir string   // extracted actions/runner dir on every pool box
	workBase  string   // warm work base on every pool box (each gets <workBase>/slot-0)
}

// slotPool hands out a fixed pool of warm work-slots, one per concurrent runner.
// At maxRunners=1 there is a single, always-warm slot. A slot marked unhealthy
// (its host failed at launch) is skipped by take until its cooldown expires, so
// a dead pool box shrinks capacity instead of black-holing every acquisition.
type slotPool struct {
	mu             sync.Mutex
	free           []*slot
	all            []*slot
	unhealthyUntil map[int]time.Time // slot index -> earliest revival time
}

// newSlotPool creates n slots under baseDir. Slot 0 (and any further slot beyond the
// remote host list) is local, with its own actions/runner dir cloned from runnerBase
// (a clean template runner). Slots 1..len(remote.hosts) are remote and get no local
// dirs. Idempotent. Returns a pool with all slots free.
func newSlotPool(baseDir, runnerBase string, n int, remote remoteSlotConfig) (*slotPool, error) {
	if n < 1 {
		return nil, fmt.Errorf("slot pool needs at least 1 slot, got %d", n)
	}
	// Clean the base so a trailing slash can't turn the slot dir into a CHILD of the
	// template (".../actions-runner/-slot0") — which would also make the clone recurse
	// into the source. We want a sibling (".../actions-runner-slot0").
	runnerBase = filepath.Clean(runnerBase)
	p := &slotPool{unhealthyUntil: make(map[int]time.Time)}
	for i := range n {
		var s *slot
		if i > 0 && i-1 < len(remote.hosts) {
			s = &slot{
				index:     i,
				host:      remote.hosts[i-1],
				path:      remote.workBase + "/slot-0", // one slot per host; remote path, not filepath.Join'd with local rules
				runnerDir: remote.runnerDir,
			}
		} else {
			path := filepath.Join(baseDir, fmt.Sprintf("slot-%d", i))
			if err := os.MkdirAll(path, 0o755); err != nil {
				return nil, fmt.Errorf("creating slot dir %q: %w", path, err)
			}
			runnerDir := fmt.Sprintf("%s-slot%d", runnerBase, i)
			if err := cloneRunnerDir(runnerBase, runnerDir); err != nil {
				return nil, fmt.Errorf("cloning runner dir for slot %d: %w", i, err)
			}
			s = &slot{index: i, path: path, runnerDir: runnerDir}
		}
		p.all = append(p.all, s)
		p.free = append(p.free, s)
	}
	return p, nil
}

// runnerStateNames are a runner's per-job runtime files: never copied into a slot clone,
// so each slot is a clean install (a fresh JIT runner writes its own .runner/.credentials).
var runnerStateNames = map[string]bool{
	"_work": true, "_diag": true, ".runner": true, ".runner_migrated": true,
	".credentials": true, ".credentials_rsaparams": true, ".service": true,
}

// cloneRunnerDir copies the base actions/runner tree (binaries only — runtime state
// excluded) into the per-slot dst, so each slot has its own .runner/.credentials and
// concurrent runners can't corrupt each other. Done in-process (no rsync dependency, so
// it works on a minimal image), and run on EVERY startup, not skipped when dst exists:
// re-copying overwrites changed files, so it repairs a partial/interrupted clone and
// propagates a pinned-runner upgrade into existing slots. The excluded state dirs are
// left untouched, so a live runner's in-flight state survives a concurrent re-sync.
func cloneRunnerDir(src, dst string) error {
	// Resolve a symlinked --runner-dir so WalkDir traverses the real tree: walking a
	// symlink root visits only the link, yielding an empty clone with no run.sh.
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		src = resolved
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		// Skip the runtime-state names (and their subtrees) wherever they appear.
		top := rel
		if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
			top = rel[:i]
		}
		if runnerStateNames[top] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			// Recreate symlinks as-is (the runner tree currently has none, but be safe);
			// relative links stay self-contained within the clone.
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		default:
			return copyFile(path, target, info.Mode().Perm())
		}
	})
}

// copyFile copies a regular file's contents into a freshly-created dst with the given
// mode. It unlinks any existing dst first, which (a) applies the current mode on a resync
// even when dst already existed (a plain O_CREATE keeps the old perms), and
// (b) avoids ETXTBSY if the old dst is a currently-executing runner binary: the running
// process keeps the old, now-unlinked inode while we write a brand-new one.
func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	_ = os.Remove(dst)
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	// OpenFile's mode is masked by the process umask, so chmod explicitly to preserve the
	// template's exact perms (e.g. run.sh's 0755 under a restrictive umask).
	if err := out.Chmod(mode); err != nil {
		_ = out.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// take removes and returns a free healthy slot, or nil if none are available.
// A slot whose unhealthy cooldown has expired is revived (its host gets another
// chance; the next launch failure marks it unhealthy again).
func (p *slotPool) take() *slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for i := len(p.free) - 1; i >= 0; i-- {
		s := p.free[i]
		if until, ok := p.unhealthyUntil[s.index]; ok {
			if now.Before(until) {
				continue
			}
			delete(p.unhealthyUntil, s.index)
		}
		p.free = append(p.free[:i], p.free[i+1:]...)
		return s
	}
	return nil
}

// takeIndex removes and returns the free slot with the given index, or nil if
// that slot is not currently free. Used by startup adoption to occupy the slot
// of a still-running runner from the previous incarnation; health cooldowns are
// ignored — the runner is demonstrably alive on that host.
func (p *slotPool) takeIndex(index int) *slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, s := range p.free {
		if s.index == index {
			p.free = append(p.free[:i], p.free[i+1:]...)
			return s
		}
	}
	return nil
}

// give returns a slot to the pool.
func (p *slotPool) give(s *slot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free = append(p.free, s)
}

// markUnhealthy returns a slot to the pool but bars take from handing it out
// until the cooldown passes — used when its host failed at launch, so the
// acquisition falls through to other slots (or stays queued for the next poll)
// instead of retrying a dead box in a tight loop.
func (p *slotPool) markUnhealthy(s *slot, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unhealthyUntil[s.index] = until
	p.free = append(p.free, s)
}

// size reports the total number of slots in the pool.
func (p *slotPool) size() int {
	return len(p.all)
}
