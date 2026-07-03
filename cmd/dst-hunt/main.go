// Command dst-hunt is the overnight brute-force bug hunter for the record layer. It sweeps
// seeds across all cores, rotating through every hunt profile (each index maintainer in
// isolation and together, normal and conflict-dense), and — crucially — does NOT stop at the
// first bug: every failing seed is shrunk to its minimal reproducer and appended to a durable
// findings log, then the sweep continues. A run is a permanent, append-only record of what was
// checked and what broke.
//
//	go build -o /tmp/dst-hunt ./cmd/dst-hunt
//	/tmp/dst-hunt -out ~/dst-hunt-results -seconds 3600 -workers 8 -seed-start 0
//
// Outputs (all under -out):
//   - findings.jsonl  one JSON object per bug (seed, profile, shrunk ops, violations, ...)
//   - progress.jsonl  periodic heartbeat (checked, bugs, seeds/sec, high-water seed)
//   - run.log         is this program's stdout (redirect when backgrounding)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fdb.dev/pkg/simfdb/hunt"
)

func main() {
	var (
		outDir      = flag.String("out", "", "results directory (required); findings.jsonl + progress.jsonl are appended here")
		seconds     = flag.Float64("seconds", 0, "wall-clock budget; 0 = run until SIGINT/SIGTERM")
		workers     = flag.Int("workers", runtime.NumCPU()-1, "concurrent hunt workers")
		seedStart   = flag.Uint64("seed-start", 0, "first seed to check (shard disjoint ranges across machines)")
		numOps      = flag.Int("numops", 0, "override ops per run (0 = each profile's default)")
		progressGap = flag.Float64("progress-secs", 30, "heartbeat interval")
		label       = flag.String("label", "", "free-text label recorded in every progress/finding line")
	)
	flag.Parse()

	if *outDir == "" {
		fmt.Fprintln(os.Stderr, "dst-hunt: -out is required")
		os.Exit(2)
	}
	if *workers < 1 {
		*workers = 1
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "dst-hunt: mkdir %s: %v\n", *outDir, err)
		os.Exit(1)
	}

	rec, err := newRecorder(*outDir, *label)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dst-hunt: %v\n", err)
		os.Exit(1)
	}
	defer rec.close()

	profiles := hunt.Profiles()

	// Stop on either the time budget or a signal. done is closed once; workers observe it and
	// drain.
	done := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(done) }) }

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; rec.logf("signal received — draining"); stop() }()
	if *seconds > 0 {
		go func() {
			t := time.NewTimer(time.Duration(*seconds * float64(time.Second)))
			defer t.Stop()
			select {
			case <-t.C:
				rec.logf("time budget (%.0fs) reached — draining", *seconds)
				stop()
			case <-done:
			}
		}()
	}

	start := time.Now()
	var (
		nextSeed atomic.Uint64
		checked  atomic.Uint64
		bugs     atomic.Uint64
	)
	nextSeed.Store(*seedStart)

	rec.logf("start: workers=%d seed-start=%d seconds=%.0f profiles=%d numops-override=%d",
		*workers, *seedStart, *seconds, len(profiles), *numOps)

	// Heartbeat.
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		tick := time.NewTicker(time.Duration(*progressGap * float64(time.Second)))
		defer tick.Stop()
		for {
			select {
			case <-done:
				rec.heartbeat(start, checked.Load(), bugs.Load(), *seedStart, nextSeed.Load(), "final")
				return
			case <-tick.C:
				rec.heartbeat(start, checked.Load(), bugs.Load(), *seedStart, nextSeed.Load(), "tick")
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				seed := nextSeed.Add(1) - 1
				p := profiles[int(seed%uint64(len(profiles)))]
				cfg := p.Cfg
				if *numOps > 0 {
					cfg.NumOps = *numOps
				}
				var rep *hunt.Report
				if msg := protect(func() { rep = hunt.Run(seed, cfg) }); msg != "" {
					// A seed that panics a maintainer is the severest possible bug — record it
					// and keep sweeping rather than aborting the night.
					rep = &hunt.Report{Seed: seed, Err: msg}
				}
				checked.Add(1)
				if rep.Failed() {
					bugs.Add(1)
					rec.finding(p, seed, cfg, rep)
				}
			}
		}()
	}
	wg.Wait()
	hbWG.Wait()

	elapsed := time.Since(start).Seconds()
	rec.logf("done: checked=%d bugs=%d elapsed=%.0fs rate=%.1f seeds/s high-water-seed=%d",
		checked.Load(), bugs.Load(), elapsed, float64(checked.Load())/elapsed, nextSeed.Load())
}

// recorder serializes durable output to the results directory.
type recorder struct {
	mu       sync.Mutex
	findings *os.File
	progress *os.File
	label    string
}

func newRecorder(dir, label string) (*recorder, error) {
	f, err := os.OpenFile(filepath.Join(dir, "findings.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open findings.jsonl: %w", err)
	}
	p, err := os.OpenFile(filepath.Join(dir, "progress.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("open progress.jsonl: %w", err)
	}
	return &recorder{findings: f, progress: p, label: label}, nil
}

func (r *recorder) close() {
	r.findings.Sync()
	r.progress.Sync()
	r.findings.Close()
	r.progress.Close()
}

// logf writes a human line to stdout (captured to run.log when backgrounded).
func (r *recorder) logf(format string, args ...any) {
	fmt.Printf("[%s] "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

type progressLine struct {
	Time      string  `json:"time"`
	Label     string  `json:"label,omitempty"`
	Kind      string  `json:"kind"`
	ElapsedS  float64 `json:"elapsed_s"`
	Checked   uint64  `json:"checked"`
	Bugs      uint64  `json:"bugs"`
	SeedStart uint64  `json:"seed_start"`
	HighWater uint64  `json:"high_water_seed"`
	SeedsPerS float64 `json:"seeds_per_s"`
}

func (r *recorder) heartbeat(start time.Time, checked, bugs, seedStart, highWater uint64, kind string) {
	el := time.Since(start).Seconds()
	rate := 0.0
	if el > 0 {
		rate = float64(checked) / el
	}
	line := progressLine{
		Time: time.Now().Format(time.RFC3339), Label: r.label, Kind: kind,
		ElapsedS: el, Checked: checked, Bugs: bugs,
		SeedStart: seedStart, HighWater: highWater, SeedsPerS: rate,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeJSON(r.progress, line)
	r.progress.Sync()
	fmt.Printf("[%s] %s checked=%d bugs=%d rate=%.1f/s high-water=%d\n",
		time.Now().Format("15:04:05"), kind, checked, bugs, rate, highWater)
}

type findingLine struct {
	Time           string   `json:"time"`
	Label          string   `json:"label,omitempty"`
	Profile        string   `json:"profile"`
	Seed           uint64   `json:"seed"`
	NumOps         int      `json:"num_ops"`
	MaxPKs         int64    `json:"max_pks"`
	FaultProb      float64  `json:"fault_prob"`
	FailedAtOp     int      `json:"failed_at_op"`
	FaultsFired    int      `json:"faults_fired"`
	ShrunkNumOps   int      `json:"shrunk_num_ops"`
	FaultDependent bool     `json:"fault_dependent"`
	Err            string   `json:"err,omitempty"`
	Violations     []string `json:"violations,omitempty"`
	Reproduce      string   `json:"reproduce"`
}

// finding shrinks the failing seed to its minimal reproducer, then records it durably. Shrink
// is best-effort: if it panics or the seed doesn't re-fail under per-op verify, the raw report
// is still recorded (never lose a finding).
func (r *recorder) finding(p hunt.Profile, seed uint64, cfg hunt.Config, rep *hunt.Report) {
	shrunkOps := rep.Ops
	faultDep := false
	protect(func() {
		sc, sr := hunt.Shrink(seed, cfg)
		if sr.Failed() {
			shrunkOps = sc.NumOps
		}
		faultDep = hunt.FaultDependent(seed, cfg)
	})

	line := findingLine{
		Time: time.Now().Format(time.RFC3339), Label: r.label,
		Profile: p.Name, Seed: seed,
		NumOps: cfg.NumOps, MaxPKs: cfg.MaxPKs, FaultProb: cfg.FaultProb,
		FailedAtOp: rep.Ops, FaultsFired: rep.FaultsFired,
		ShrunkNumOps: shrunkOps, FaultDependent: faultDep,
		Err: rep.Err, Violations: rep.Violations,
		Reproduce: fmt.Sprintf("HUNT_SEED=%d bazelisk test //pkg/simfdb/hunt:hunt_test --test_arg=-test.run=TestHuntSeed --test_env=HUNT_SEED=%d --nocache_test_results", seed, seed),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeJSON(r.findings, line)
	r.findings.Sync()
	fmt.Printf("[%s] *** BUG *** profile=%s seed=%d shrunk=%d ops fault-dependent=%v\n    %v %s\n",
		time.Now().Format("15:04:05"), p.Name, seed, shrunkOps, faultDep, rep.Violations, rep.Err)
}

func (r *recorder) writeJSON(f *os.File, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dst-hunt: marshal: %v\n", err)
		return
	}
	f.Write(append(b, '\n'))
}

// protect runs fn and converts a panic into a returned message (with stack trace). This is the
// one deliberate panic→error boundary in dst-hunt (RFC-134 allowlisted): a seed that panics a
// record-layer maintainer is the most severe bug the hunt can find, so a worker catches it,
// records it as a finding, and keeps sweeping — one poisoned seed must not abort an overnight
// unattended run. The panic is surfaced (as an Err on the recorded finding), never swallowed.
func protect(fn func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprintf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	fn()
	return ""
}
