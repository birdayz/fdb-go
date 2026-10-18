package bench

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Config struct {
	ClusterFile    string `json:"cluster_file"`
	Workload       string `json:"workload"`
	Iterations     int    `json:"iterations"`
	Warmup         int    `json:"warmup"`
	Workers        int    `json:"workers"`
	Output         string `json:"output"`
	RunID          string `json:"run_id"`
	Block          int    `json:"block"`
	SourceRevision string `json:"source_revision"`
	HarnessID      string `json:"harness_id"`
	AllowWrites    bool   `json:"allow_writes"`
	Prefix         string `json:"prefix"`
	TimeoutMS      int64  `json:"transaction_timeout_ms"`
	RetryLimit     int64  `json:"retry_limit"`
	Profile        string `json:"profile"`
	// CoalesceDelay is the Go client's connection write delay, "min,max" as Go
	// durations; empty keeps the client default.
	CoalesceDelay string `json:"coalesce_delay,omitempty"`
	// Rate > 0 paces the run: operations start at Poisson arrival times averaging
	// Rate per second, drawn from Seed, so every run with the same seed starts the
	// same operations at the same offsets. Rate 0 is a closed loop.
	Rate float64 `json:"rate,omitempty"`
	Seed uint64  `json:"seed"`
}

func (c Config) Validate() error {
	if c.ClusterFile == "" || c.Output == "" {
		return errors.New("cluster-file and output are required")
	}
	if !c.AllowWrites {
		return errors.New("--allow-writes is required: setup overwrites keys under the selected prefix")
	}
	if !strings.HasPrefix(c.Prefix, "clientbench/") || !strings.HasSuffix(c.Prefix, "/") || len(c.Prefix) > 256 {
		return errors.New("prefix must start with clientbench/ and end with /, at most 256 bytes")
	}
	if c.Iterations <= 0 || c.Iterations > 10000000 || c.Warmup < 0 || c.Warmup > 10000000 || c.Workers <= 0 || c.Workers > 1024 {
		return errors.New("invalid iterations (1..10000000), warmup (0..10000000), or workers (1..1024)")
	}
	if c.Rate < 0 || c.Rate > 1e6 {
		return errors.New("rate must be 0 (closed loop) or at most 1e6 operations per second")
	}
	if c.TimeoutMS <= 0 || c.RetryLimit < 0 {
		return errors.New("positive transaction timeout and nonnegative retry limit required")
	}
	switch c.Workload {
	case "get100", "get1000", "get10000", "read1000", "grv", "set100", "range10", "range100", "ryw", "batchset50", "batchset50x1000":
	default:
		return fmt.Errorf("unknown workload %q", c.Workload)
	}
	switch c.Profile {
	case "none", "cpu", "heap", "mutex", "block", "trace":
	default:
		return fmt.Errorf("unknown profile %q", c.Profile)
	}
	return nil
}

type Sample struct {
	Index, Worker            int
	StartOffsetNS, LatencyNS int64
	// ScheduledOffsetNS is the paced start time; -1 in a closed loop.
	ScheduledOffsetNS int64
	Attempts          int
	Err               error
	Attempted         bool
}

type Phase struct {
	Samples                    []Sample
	WallNS                     int64
	Completed, Errors, Retries int
	Before, After              Stats
	StatsError                 error
}

// RunPhase excludes generation, goroutine creation, CSV formatting and stats
// collection. Latencies include retries, full materialization and exact checks.
func RunPhase(count, workers int, operation func(index, worker int) (int, error)) Phase {
	return runPhase(count, workers, nil, operation, false)
}

// PoissonSchedule returns n start offsets with exponential gaps averaging 1/rate
// seconds, from seed: evenly spread on average, pseudo-random in detail.
func PoissonSchedule(n int, rate float64, seed uint64) []int64 {
	rng := rand.New(rand.NewPCG(seed, 0x636c69656e7462)) // "clientb"
	out := make([]int64, n)
	var t float64
	for i := range out {
		t += rng.ExpFloat64() / rate
		out[i] = int64(t * 1e9)
	}
	return out
}

// runPhase gives operation i to worker i % workers. With a schedule, each worker
// starts its next operation at its scheduled offset, or at once if it is late.
func runPhase(count, workers int, schedule []int64, operation func(index, worker int) (int, error), collectStats bool) Phase {
	p := Phase{Samples: make([]Sample, count)}
	var ready, done sync.WaitGroup
	var aborted atomic.Bool
	start := make(chan struct{})
	var epoch time.Time
	for worker := 0; worker < workers; worker++ {
		ready.Add(1)
		done.Add(1)
		go func(worker int) {
			defer done.Done()
			ready.Done()
			<-start
			for i := worker; i < count; i += workers {
				if aborted.Load() {
					return
				}
				scheduled := int64(-1)
				if schedule != nil {
					scheduled = schedule[i]
					if wait := time.Until(epoch.Add(time.Duration(scheduled))); wait > 0 {
						time.Sleep(wait)
					}
				}
				begin := time.Now()
				attempts, err := operation(i, worker)
				elapsed := time.Since(begin).Nanoseconds()
				p.Samples[i] = Sample{Index: i, Worker: worker, StartOffsetNS: begin.Sub(epoch).Nanoseconds(), LatencyNS: elapsed, ScheduledOffsetNS: scheduled, Attempts: attempts, Err: err, Attempted: true}
				if err != nil {
					aborted.Store(true)
					return
				}
			}
		}(worker)
	}
	ready.Wait()
	if collectStats {
		p.Before, p.StatsError = ReadStats(false)
		if p.StatsError != nil {
			aborted.Store(true)
		}
	}
	epoch = time.Now()
	close(start)
	done.Wait()
	p.WallNS = time.Since(epoch).Nanoseconds()
	if collectStats {
		var err error
		p.After, err = ReadStats(true)
		p.StatsError = errors.Join(p.StatsError, err)
	}
	for _, sample := range p.Samples {
		if !sample.Attempted {
			continue
		}
		p.Completed++
		if sample.Err != nil {
			p.Errors++
		}
		p.Retries += max(0, sample.Attempts-1)
	}
	return p
}

func WriteSamples(path, phase string, samples []Sample) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	err = w.Write([]string{"phase", "index", "worker", "start_offset_ns", "latency_ns", "attempts", "error", "check_status", "scheduled_offset_ns"})
	for _, s := range samples {
		if err != nil || !s.Attempted {
			continue
		}
		message, status := "", "ok"
		if s.Err != nil {
			message, status = s.Err.Error(), "error"
		}
		err = w.Write([]string{phase, strconv.Itoa(s.Index), strconv.Itoa(s.Worker), strconv.FormatInt(s.StartOffsetNS, 10), strconv.FormatInt(s.LatencyNS, 10), strconv.Itoa(s.Attempts), message, status, strconv.FormatInt(s.ScheduledOffsetNS, 10)})
	}
	w.Flush()
	return errors.Join(err, w.Error(), f.Close())
}

func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return errors.Join(err, f.Close())
}

type Stats struct {
	TotalAlloc uint64 `json:"go_total_alloc_bytes"`
	Mallocs    uint64 `json:"go_mallocs"`
	GC         uint32 `json:"go_gc_cycles"`
	GCPause    uint64 `json:"go_gc_pause_ns"`
	UserUS     int64  `json:"process_user_cpu_us"`
	SystemUS   int64  `json:"process_system_cpu_us"`
	MaxRSSKB   int64  `json:"process_high_water_rss_kb"`
}

func ReadStats(after bool) (Stats, error) {
	var u syscall.Rusage
	if after {
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
			return Stats{}, err
		}
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if !after {
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
			return Stats{}, err
		}
	}
	return Stats{m.TotalAlloc, m.Mallocs, m.NumGC, m.PauseTotalNs, u.Utime.Sec*1000000 + u.Utime.Usec, u.Stime.Sec*1000000 + u.Stime.Usec, u.Maxrss}, nil
}

// schedule is the phase's paced start offsets, nil in a closed loop; phase keeps
// the warmup and measured schedules distinct for one seed.
func schedule(cfg Config, n int, phase uint64) []int64 {
	if cfg.Rate == 0 {
		return nil
	}
	return PoissonSchedule(n, cfg.Rate, cfg.Seed*2+phase)
}

func timing(cfg Config) string {
	scope := "completion latency including transaction retries, result materialization, exact byte checks; no serialization or generation"
	if cfg.Rate == 0 {
		return "closed-loop " + scope
	}
	return fmt.Sprintf("paced: Poisson starts at %g/s from seed %d, operation i on worker i %% workers, a late worker starts at once; latency_ns from the actual start; %s", cfg.Rate, cfg.Seed, scope)
}

func Run(cfg Config, db LifecycleDatabase, metadata map[string]any, afterIdentity Stats) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Output, 0o755); err != nil {
		return err
	}
	// O_EXCL manifests prevent mixing samples from separate invocations.
	if err := WriteJSON(filepath.Join(cfg.Output, "manifest.json"), map[string]any{"config": cfg, "backend": metadata, "pid": os.Getpid(), "go_version": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "started_utc": time.Now().UTC(), "timing": timing(cfg), "attempts_scope": "transaction callback entries only; internal RPC retries unobserved", "memory_scope": "Go allocation counters exclude native libfdb_c heap; RSS high-water includes setup and is process lifetime, not interval peak", "environment": map[string]string{"GOMAXPROCS": os.Getenv("GOMAXPROCS"), "GOGC": os.Getenv("GOGC"), "GOMEMLIMIT": os.Getenv("GOMEMLIMIT"), "GODEBUG": os.Getenv("GODEBUG")}}); err != nil {
		return err
	}
	d := Prepare(cfg.Prefix, cfg.Workers, cfg.Warmup, cfg.Iterations)
	if size := batchValueBytes(cfg.Workload); size > 0 {
		d.prepareBatchSet50(cfg.Prefix, size)
	}
	var runErr error
	var warm, measured Phase
	var countersBefore, countersAfter, countersDelta ClientCounters
	stage := "seed"
	if err := d.Seed(db); err != nil {
		runErr = err
	} else if err := d.ValidateSeed(db); err != nil {
		stage = "seed_validation"
		runErr = err
	}
	if runErr == nil {
		stage = "warmup"
		warm = runPhase(cfg.Warmup, cfg.Workers, schedule(cfg, cfg.Warmup, 1), func(i, w int) (int, error) { return d.Operation(db, cfg.Workload, w, d.Warmup[i]) }, false)
		if warm.Errors > 0 || warm.Completed != cfg.Warmup {
			runErr = fmt.Errorf("warmup incomplete: completed=%d errors=%d", warm.Completed, warm.Errors)
		}
	}
	if runErr == nil {
		stage = "measure"
		stop, err := StartProfile(cfg.Profile, cfg.Output)
		if err != nil {
			runErr = err
		} else {
			countersBefore = db.Counters()
			measured = runPhase(cfg.Iterations, cfg.Workers, schedule(cfg, cfg.Iterations, 0), func(i, w int) (int, error) { return d.Operation(db, cfg.Workload, w, d.Measure[i]) }, true)
			countersAfter = db.Counters()
			countersDelta, err = CounterDelta(countersBefore, countersAfter)
			runErr = errors.Join(measured.StatsError, err)
			if runErr == nil {
				runErr = ValidateGRVWorkload(countersDelta, cfg.Workload, measured.Completed)
			}
			runErr = errors.Join(runErr, stop())
			if measured.Errors > 0 || measured.Completed != cfg.Iterations {
				runErr = errors.Join(runErr, fmt.Errorf("measurement incomplete: completed=%d errors=%d", measured.Completed, measured.Errors))
			}
		}
	}
	var lifecycle Lifecycle
	if runErr == nil {
		stage = "measurement_drain"
		lifecycle.MeasurementDrain, runErr = DrainFinalizers()
		if runErr == nil {
			runErr = db.Barrier()
			lifecycle.MeasurementBarrier = runErr == nil
		}
		if runErr == nil {
			lifecycle.AfterDrain, runErr = ReadStats(true)
		}
	}
	postValidation := "not_run"
	if runErr == nil {
		stage = "persisted_validation"
		runErr = d.ValidateWrites(db, cfg.Workload, cfg.Warmup, cfg.Iterations)
		if runErr == nil {
			postValidation = "ok"
		} else {
			postValidation = "error"
		}
	}
	writeErr := errors.Join(WriteSamples(filepath.Join(cfg.Output, "warmup.csv"), "warmup", warm.Samples), WriteSamples(filepath.Join(cfg.Output, "samples.csv"), "measure", measured.Samples))
	if runErr == nil && writeErr == nil {
		stage = "shutdown_drain"
		// Validation and the nonterminal barrier also create native finalizers.
		lifecycle.ShutdownDrain, runErr = DrainFinalizers()
		if runErr == nil {
			stage = "shutdown"
			lifecycle.NetworkStopped, runErr = db.Shutdown()
			lifecycle.DatabaseClosed = runErr == nil
		}
		if runErr == nil {
			lifecycle.AfterShutdown, runErr = ReadStats(true)
			lifecycle.Complete = runErr == nil
		}
	}
	failure := ""
	if runErr != nil {
		failure = runErr.Error()
	}
	summary := map[string]any{"config": cfg, "backend": metadata, "requested": cfg.Iterations, "completed": measured.Completed, "totalcompleted": measured.Completed, "errors": measured.Errors, "retries": measured.Retries, "warmup_requested": cfg.Warmup, "warmup_completed": warm.Completed, "warmup_errors": warm.Errors, "warmup_retries": warm.Retries, "measured_wall_ns": measured.WallNS, "warmup_wall_ns": warm.WallNS, "stats_after_identity": afterIdentity, "stats_before": measured.Before, "stats_after": measured.After, "lifecycle": lifecycle, "client_counters_before": countersBefore, "client_counters_after": countersAfter, "client_counters_delta": countersDelta, "persisted_validation": postValidation, "failure_stage": stage, "failure": failure, "success": runErr == nil && writeErr == nil, "profile": cfg.Profile}
	return errors.Join(runErr, writeErr, WriteJSON(filepath.Join(cfg.Output, "summary.json"), summary))
}
