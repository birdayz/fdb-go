package bench

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	// AdaptiveLinger is the Go client's Nagle-style adaptive linger window; 0 keeps the default.
	AdaptiveLinger time.Duration `json:"adaptive_linger_ns,omitempty"`
	// Rate > 0 paces the run: operations start at Poisson arrival times averaging
	// Rate per second, drawn from Seed, so every run with the same seed starts the
	// same operations at the same offsets. Rate 0 is a closed loop.
	Rate float64 `json:"rate,omitempty"`
	Seed uint64  `json:"seed"`
	// Duration selects fixed-time closed-loop mode; zero preserves count mode.
	Duration       time.Duration `json:"duration_ns,omitempty"`
	WarmupDuration time.Duration `json:"warmup_duration_ns,omitempty"`
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
	if c.Workers <= 0 || c.Workers > 1024 || (c.Duration == 0 && (c.Iterations <= 0 || c.Iterations > 10000000 || c.Warmup < 0 || c.Warmup > 10000000)) {
		return errors.New("invalid iterations (1..10000000), warmup (0..10000000), or workers (1..1024)")
	}
	if math.IsNaN(c.Rate) || c.Rate < 0 || c.Rate > 1e6 {
		return errors.New("rate must be 0 (closed loop) or a positive number of operations per second up to 1e6")
	}
	if c.Duration < 0 || c.WarmupDuration < 0 {
		return errors.New("duration and warmup-duration must be nonnegative")
	}
	if c.Duration > 0 && c.Rate != 0 {
		return errors.New("duration mode requires rate 0 (closed loop)")
	}
	if c.Duration > 0 && (c.Duration > time.Duration(math.MaxInt64)-120*time.Second || c.WarmupDuration > time.Duration(math.MaxInt64)-120*time.Second-c.Duration) {
		return errors.New("duration plus warmup-duration exceeds the watchdog budget range")
	}
	if c.TimeoutMS <= 0 || c.RetryLimit < 0 {
		return errors.New("positive transaction timeout and nonnegative retry limit required")
	}
	if c.AdaptiveLinger < 0 {
		return errors.New("adaptive-linger must be nonnegative")
	}
	if c.CoalesceDelay != "" && c.AdaptiveLinger > 0 {
		return errors.New("coalesce-delay and adaptive-linger are mutually exclusive")
	}
	switch c.Workload {
	case "get100", "get1000", "get10000", "read1000", "grv", "set100", "range10", "range100", "ryw", "batchset50", "batchset50x1000", "getset100":
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

// PoissonSchedule returns n start offsets in nanoseconds with exponential gaps
// averaging 1/rate seconds, from seed and stream: evenly spread on average,
// pseudo-random in detail. Streams give one seed independent schedules.
func PoissonSchedule(n int, rate float64, seed, stream uint64) ([]int64, error) {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return nil, fmt.Errorf("schedule rate %v: want a positive finite rate", rate)
	}
	rng := rand.New(rand.NewPCG(seed, stream))
	out := make([]int64, n)
	var t float64
	for i := range out {
		t += rng.ExpFloat64() / rate
		ns := t * 1e9
		if ns >= math.MaxInt64 {
			return nil, fmt.Errorf("schedule rate %v: operation %d starts beyond %v", rate, i, time.Duration(math.MaxInt64))
		}
		out[i] = int64(ns)
	}
	return out, nil
}

// paceWaitHook, if set by a test, runs as a worker starts a scheduled wait on pc.
var paceWaitHook func(worker int, pc *pacer)

// runPhase gives operation i to worker i % workers. With a schedule, each worker
// starts its next operation at its scheduled offset, or at once if it is late.
func runPhase(count, workers int, schedule []int64, operation func(index, worker int) (int, error), collectStats bool) Phase {
	p := Phase{Samples: make([]Sample, count)}
	var ready, done sync.WaitGroup
	var aborted atomic.Bool
	// One pacer per worker; closing them on the first failure wakes scheduled waits.
	pacers := make([]*pacer, 0, workers)
	if schedule != nil {
		for range workers {
			pc, err := newPacer()
			if err != nil {
				for _, q := range pacers {
					q.close()
				}
				p.StatsError = err
				return p
			}
			pacers = append(pacers, pc)
		}
	}
	var stopOnce sync.Once
	stop := func() {
		aborted.Store(true)
		stopOnce.Do(func() {
			for _, pc := range pacers {
				pc.close()
			}
		})
	}
	defer stop()
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
						if paceWaitHook != nil {
							paceWaitHook(worker, pacers[worker])
						}
						if !pacers[worker].wait(wait) {
							return
						}
					}
					if aborted.Load() {
						return
					}
				}
				begin := time.Now()
				attempts, err := operation(i, worker)
				elapsed := time.Since(begin).Nanoseconds()
				p.Samples[i] = Sample{Index: i, Worker: worker, StartOffsetNS: begin.Sub(epoch).Nanoseconds(), LatencyNS: elapsed, ScheduledOffsetNS: scheduled, Attempts: attempts, Err: err, Attempted: true}
				if err != nil {
					stop()
					return
				}
			}
		}(worker)
	}
	ready.Wait()
	if collectStats {
		p.Before, p.StatsError = ReadStats(false)
		if p.StatsError != nil {
			stop()
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

// Schedule streams: the warmup and measured phases of one seed are independent.
const (
	measureStream uint64 = 0x636c69656e746d // "clientm"
	warmupStream  uint64 = 0x636c69656e7477 // "clientw"
)

// schedule is the phase's paced start offsets, nil in a closed loop.
func schedule(cfg Config, n int, stream uint64) ([]int64, error) {
	if cfg.Rate == 0 {
		return nil, nil
	}
	return PoissonSchedule(n, cfg.Rate, cfg.Seed, stream)
}

// Pacing reports how closely a paced phase's actual starts followed its
// schedule: start lag (actual - scheduled start) and the coefficient of variation
// of the gaps between consecutive starts, actual and scheduled (about 1 for
// Poisson arrivals; bunched starts raise it, regularized starts lower it).
type Pacing struct {
	LagP50NS, LagP99NS, LagMaxNS int64
	LateOver100us, LateOver1ms   float64
	GapCVActual, GapCVScheduled  float64
	Samples                      int
}

func pacing(samples []Sample) *Pacing {
	var lags, starts, scheduled []int64
	for _, s := range samples {
		if s.Attempted && s.ScheduledOffsetNS >= 0 {
			lags = append(lags, s.StartOffsetNS-s.ScheduledOffsetNS)
			starts = append(starts, s.StartOffsetNS)
			scheduled = append(scheduled, s.ScheduledOffsetNS)
		}
	}
	if len(lags) < 2 {
		return nil
	}
	slices.Sort(lags)
	slices.Sort(starts)
	slices.Sort(scheduled)
	late := func(limit int64) float64 {
		n := 0
		for _, l := range lags {
			if l > limit {
				n++
			}
		}
		return float64(n) / float64(len(lags))
	}
	return &Pacing{
		LagP50NS: lags[len(lags)/2], LagP99NS: lags[len(lags)*99/100], LagMaxNS: lags[len(lags)-1],
		LateOver100us: late(int64(100 * time.Microsecond)), LateOver1ms: late(int64(time.Millisecond)),
		GapCVActual: gapCV(starts), GapCVScheduled: gapCV(scheduled), Samples: len(lags),
	}
}

// gapCV is the coefficient of variation of the gaps between sorted offsets.
func gapCV(offsets []int64) float64 {
	n := float64(len(offsets) - 1)
	var sum, sq float64
	for i := 1; i < len(offsets); i++ {
		g := float64(offsets[i] - offsets[i-1])
		sum += g
		sq += g * g
	}
	mean := sum / n
	if mean == 0 {
		return 0
	}
	return math.Sqrt(max(sq/n-mean*mean, 0)) / mean
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
	if cfg.Duration > 0 {
		return runDuration(cfg, db, metadata, afterIdentity)
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
	var paced, warmSchedule []int64
	var countersBefore, countersAfter, countersDelta ClientCounters
	stage := "seed"
	if err := d.Seed(db); err != nil {
		runErr = err
	} else if err := d.ValidateSeed(db); err != nil {
		stage = "seed_validation"
		runErr = err
	}
	if runErr == nil {
		stage = "schedule"
		var err, err2 error
		warmSchedule, err = schedule(cfg, cfg.Warmup, warmupStream)
		paced, err2 = schedule(cfg, cfg.Iterations, measureStream)
		runErr = errors.Join(err, err2)
	}
	if runErr == nil {
		stage = "warmup"
		warm = runPhase(cfg.Warmup, cfg.Workers, warmSchedule, func(i, w int) (int, error) { return d.Operation(db, cfg.Workload, w, d.Warmup[i]) }, false)
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
			measured = runPhase(cfg.Iterations, cfg.Workers, paced, func(i, w int) (int, error) { return d.Operation(db, cfg.Workload, w, d.Measure[i]) }, true)
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
	summary := map[string]any{"config": cfg, "backend": metadata, "requested": cfg.Iterations, "completed": measured.Completed, "totalcompleted": measured.Completed, "errors": measured.Errors, "retries": measured.Retries, "warmup_requested": cfg.Warmup, "warmup_completed": warm.Completed, "warmup_errors": warm.Errors, "warmup_retries": warm.Retries, "measured_wall_ns": measured.WallNS, "warmup_wall_ns": warm.WallNS, "stats_after_identity": afterIdentity, "stats_before": measured.Before, "stats_after": measured.After, "lifecycle": lifecycle, "client_counters_before": countersBefore, "client_counters_after": countersAfter, "client_counters_delta": countersDelta, "persisted_validation": postValidation, "failure_stage": stage, "failure": failure, "success": runErr == nil && writeErr == nil, "profile": cfg.Profile, "pacing": pacing(measured.Samples)}
	return errors.Join(runErr, writeErr, WriteJSON(filepath.Join(cfg.Output, "summary.json"), summary))
}
