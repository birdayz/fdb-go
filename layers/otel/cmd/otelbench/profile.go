package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"sync"
	"time"
)

// profileFlags select a run-relative [start, start+duration) profiling window;
// zero duration lasts until the run ends.
type profileFlags struct {
	cpu, mem, block, mutex, trace string
	start, duration               time.Duration
	blockRate, mutexFraction      int
}

func (p *profileFlags) register(flags *flag.FlagSet) {
	flags.StringVar(&p.cpu, "cpuprofile", "", "write a CPU profile of the profiling window to this new file")
	flags.StringVar(&p.mem, "memprofile", "", "write a heap profile at the end of the profiling window to this new file, and the same profile at its start to FILE.start (pprof -base FILE.start FILE: the window's allocations)")
	flags.StringVar(&p.block, "blockprofile", "", "write a goroutine blocking profile of the profiling window to this new file")
	flags.StringVar(&p.mutex, "mutexprofile", "", "write a mutex contention profile of the profiling window to this new file")
	flags.StringVar(&p.trace, "trace", "", "write a Go execution trace of the profiling window to this new file (go tool trace)")
	flags.DurationVar(&p.start, "profile-start", 0, "start of the profiling window after the run starts; collector-bench defaults to --warmup")
	flags.DurationVar(&p.duration, "profile-duration", 0, "length of the profiling window; 0 lasts until the run ends")
	flags.IntVar(&p.blockRate, "blockprofilerate", 10000, "with --blockprofile, sample one blocking event per this many nanoseconds blocked (runtime.SetBlockProfileRate)")
	flags.IntVar(&p.mutexFraction, "mutexprofilefraction", 100, "with --mutexprofile, sample one in this many mutex contention events (runtime.SetMutexProfileFraction)")
}

func (p *profileFlags) enabled() bool {
	return p.cpu != "" || p.mem != "" || p.block != "" || p.mutex != "" || p.trace != ""
}

func (p *profileFlags) validate() error {
	if p.start < 0 || p.duration < 0 {
		return errors.New("--profile-start and --profile-duration must not be negative")
	}
	if p.blockRate < 1 || p.mutexFraction < 1 {
		return errors.New("--blockprofilerate and --mutexprofilefraction must be positive")
	}
	return nil
}

// profiler runs one profiling window. Its files are created before the run,
// so a bad path fails before any work.
type profiler struct {
	cfg   profileFlags
	files map[string]*os.File // by flag name; "memstart" is the window-start heap

	mu     sync.Mutex
	opened bool
	closed bool
	err    error
	timer  *time.Timer
}

// newProfiler creates the profile files; begin schedules the window.
func newProfiler(cfg profileFlags) (*profiler, error) {
	p := &profiler{cfg: cfg, files: map[string]*os.File{}}
	paths := map[string]string{"cpuprofile": cfg.cpu, "memprofile": cfg.mem, "blockprofile": cfg.block, "mutexprofile": cfg.mutex, "trace": cfg.trace}
	if cfg.mem != "" {
		paths["memstart"] = cfg.mem + ".start"
	}
	for name, path := range paths {
		if path == "" {
			continue
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			p.abort()
			return nil, fmt.Errorf("--%s: %w", name, err)
		}
		p.files[name] = f
	}
	return p, nil
}

// begin starts the run's clock: the window opens after --profile-start.
func (p *profiler) begin() {
	if p.cfg.start == 0 {
		// Synchronously, so a window at the start covers the whole run.
		p.open()
		return
	}
	p.mu.Lock()
	p.timer = time.AfterFunc(p.cfg.start, p.open)
	p.mu.Unlock()
}

// abort removes the files of a profiler whose run does not start.
func (p *profiler) abort() {
	for _, f := range p.files {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	p.files = map[string]*os.File{}
}

func (p *profiler) open() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.opened = true
	if f := p.files["memstart"]; f != nil {
		runtime.GC()
		p.fail("memprofile start", pprof.Lookup("heap").WriteTo(f, 0))
	}
	if p.files["blockprofile"] != nil {
		runtime.SetBlockProfileRate(p.cfg.blockRate)
	}
	if p.files["mutexprofile"] != nil {
		runtime.SetMutexProfileFraction(p.cfg.mutexFraction)
	}
	if f := p.files["cpuprofile"]; f != nil {
		p.fail("cpuprofile", pprof.StartCPUProfile(f))
	}
	if f := p.files["trace"]; f != nil {
		p.fail("trace", trace.Start(f))
	}
	if p.cfg.duration > 0 {
		p.timer = time.AfterFunc(p.cfg.duration, func() { _ = p.close() })
	}
}

// close writes end-of-window profiles and retains the first error across calls;
// a window that never opened is an error.
func (p *profiler) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.err
	}
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
	}
	if !p.opened {
		p.err = fmt.Errorf("profiling window never opened: the run ended before --profile-start %s", p.cfg.start)
		p.closeFiles()
		return p.err
	}
	if p.files["cpuprofile"] != nil {
		pprof.StopCPUProfile()
	}
	if p.files["trace"] != nil {
		trace.Stop()
	}
	if f := p.files["memprofile"]; f != nil {
		runtime.GC()
		p.fail("memprofile", pprof.Lookup("heap").WriteTo(f, 0))
	}
	if f := p.files["blockprofile"]; f != nil {
		p.fail("blockprofile", pprof.Lookup("block").WriteTo(f, 0))
		runtime.SetBlockProfileRate(0)
	}
	if f := p.files["mutexprofile"]; f != nil {
		p.fail("mutexprofile", pprof.Lookup("mutex").WriteTo(f, 0))
		runtime.SetMutexProfileFraction(0)
	}
	p.closeFiles()
	return p.err
}

// fail keeps the first error; callers hold p.mu.
func (p *profiler) fail(what string, err error) {
	if err != nil && p.err == nil {
		p.err = fmt.Errorf("%s: %w", what, err)
	}
}

func (p *profiler) closeFiles() {
	for name, f := range p.files {
		if err := f.Close(); err != nil && p.err == nil {
			p.err = fmt.Errorf("--%s: %w", name, err)
		}
	}
	p.files = map[string]*os.File{}
}
