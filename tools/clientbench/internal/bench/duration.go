package bench

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type processCPU struct {
	UserNS   int64 `json:"user_ns"`
	SystemNS int64 `json:"system_ns"`
}

func readProcessCPU() (processCPU, error) {
	var u syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
		return processCPU{}, err
	}
	return processCPU{UserNS: u.Utime.Sec*1e9 + u.Utime.Usec*1000, SystemNS: u.Stime.Sec*1e9 + u.Stime.Usec*1000}, nil
}

// Worker and index are implicit in the containing worker buffer and its offset.
// Error values are retained separately, so successful samples cost 32 bytes.
type durationSample struct {
	latencyNS, completionOffsetNS int64
	attempts, errorIndex          int
}

type durationWorker struct {
	samples                                []durationSample
	errors                                 []error
	successful, failed, incorrect, retries int
	firstError, firstCorrectnessError      error
}

type durationPhase struct {
	workers                                               []durationWorker
	wallNS, timerLatenessNS, drainWallNS                  int64
	successful, failed, incorrect, retries, boundaryDrain int
	boundaryIncorrect                                     int
	before, after                                         processCPU
	statsError, firstError, firstCorrectnessError         error
}

func isCorrectnessError(err error) bool {
	var failure *correctnessError
	return errors.As(err, &failure)
}

// The controller owns the one observed stop timestamp. Every completion is
// classified against that timestamp after joining workers, never a worker clock
// or a count limit. CPU is captured there, before waiting for boundary drain.
func runDurationPhase(duration time.Duration, workers int, operation func(index, worker int) (int, error), record bool, readCPU func() (processCPU, error)) durationPhase {
	p := durationPhase{workers: make([]durationWorker, workers)}
	if duration == 0 {
		return p
	}
	var ready, done sync.WaitGroup
	var stopped atomic.Bool
	start := make(chan struct{})
	var epoch, nominalStop time.Time
	for worker := range workers {
		if record {
			p.workers[worker].samples = make([]durationSample, 0, max(16, 4096/workers))
		}
		ready.Add(1)
		done.Add(1)
		go func(worker int) {
			defer done.Done()
			w := &p.workers[worker]
			ready.Done()
			<-start
			for index := worker; !stopped.Load(); index += workers {
				begin := time.Now()
				attempts, err := operation(index, worker)
				end := time.Now()
				if err != nil {
					if w.firstError == nil {
						w.firstError = err
					}
					if w.firstCorrectnessError == nil && isCorrectnessError(err) {
						w.firstCorrectnessError = err
					}
				}
				// Warmup needs aggregates, not an unbounded raw sample buffer.
				// Only its tail can straddle the eventual observed stop.
				if !record && end.Before(nominalStop) {
					if err == nil {
						w.successful++
					} else if isCorrectnessError(err) {
						w.incorrect++
					} else {
						w.failed++
					}
					w.retries += max(0, attempts-1)
					continue
				}
				sample := durationSample{latencyNS: end.Sub(begin).Nanoseconds(), completionOffsetNS: end.Sub(epoch).Nanoseconds(), attempts: attempts}
				if err != nil {
					w.errors = append(w.errors, err)
					sample.errorIndex = len(w.errors)
				}
				w.samples = append(w.samples, sample)
			}
		}(worker)
	}
	ready.Wait()
	p.before, p.statsError = readCPU()
	if p.statsError != nil {
		stopped.Store(true)
		close(start)
		done.Wait()
		return p
	}
	epoch = time.Now()
	nominalStop = epoch.Add(duration)
	timer := time.NewTimer(duration)
	close(start)
	<-timer.C
	stop := time.Now()
	stopped.Store(true)
	p.after, p.statsError = readCPU()
	p.wallNS = stop.Sub(epoch).Nanoseconds()
	p.timerLatenessNS = max(0, p.wallNS-duration.Nanoseconds())
	done.Wait()
	p.drainWallNS = time.Since(stop).Nanoseconds()
	p.collect()
	return p
}

func (p *durationPhase) collect() {
	for _, worker := range p.workers {
		p.successful += worker.successful
		p.failed += worker.failed
		p.incorrect += worker.incorrect
		p.retries += worker.retries
		if p.firstError == nil {
			p.firstError = worker.firstError
		}
		if p.firstCorrectnessError == nil {
			p.firstCorrectnessError = worker.firstCorrectnessError
		}
		for _, sample := range worker.samples {
			incorrect := sample.errorIndex != 0 && isCorrectnessError(worker.errors[sample.errorIndex-1])
			if sample.completionOffsetNS >= p.wallNS {
				p.boundaryDrain++
				if incorrect {
					p.boundaryIncorrect++
				}
				continue
			}
			if sample.errorIndex == 0 {
				p.successful++
			} else if incorrect {
				p.incorrect++
			} else {
				p.failed++
			}
			p.retries += max(0, sample.attempts-1)
		}
	}
}

// Quantiles are exact nearest-rank quantiles. Sorting worker-local buffers and
// selecting a rank by value avoids a second full-size concatenated copy.
func durationQuantile(buffers [][]int64, percentile int) *int64 {
	var count int
	var lo, hi int64
	for _, values := range buffers {
		if len(values) == 0 {
			continue
		}
		if count == 0 {
			lo, hi = values[0], values[len(values)-1]
		} else {
			lo, hi = min(lo, values[0]), max(hi, values[len(values)-1])
		}
		count += len(values)
	}
	if count == 0 {
		return nil
	}
	rank := (count*percentile + 99) / 100
	for lo < hi {
		mid := lo + (hi-lo)/2
		n := 0
		for _, values := range buffers {
			n += sort.Search(len(values), func(i int) bool { return values[i] > mid })
		}
		if n >= rank {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return &lo
}

func (p durationPhase) quantiles() (p50, p99, errorP99 *int64) {
	successes := make([][]int64, len(p.workers))
	failures := make([][]int64, len(p.workers))
	for i, worker := range p.workers {
		var good, bad int
		for _, sample := range worker.samples {
			if sample.errorIndex != 0 && isCorrectnessError(worker.errors[sample.errorIndex-1]) {
				continue
			}
			if sample.completionOffsetNS < p.wallNS {
				if sample.errorIndex == 0 {
					good++
				} else {
					bad++
				}
			}
		}
		successes[i] = make([]int64, 0, good)
		failures[i] = make([]int64, 0, bad)
		for _, sample := range worker.samples {
			if sample.completionOffsetNS >= p.wallNS || (sample.errorIndex != 0 && isCorrectnessError(worker.errors[sample.errorIndex-1])) {
				continue
			}
			if sample.errorIndex == 0 {
				successes[i] = append(successes[i], sample.latencyNS)
			} else {
				failures[i] = append(failures[i], sample.latencyNS)
			}
		}
		slices.Sort(successes[i])
		slices.Sort(failures[i])
	}
	return durationQuantile(successes, 50), durationQuantile(successes, 99), durationQuantile(failures, 99)
}

func writeDurationSamples(path string, p durationPhase) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	err = w.Write([]string{"phase", "worker", "index", "latency_ns", "attempts", "error", "successful", "completion_offset_ns"})
	for worker, data := range p.workers {
		for sequence, sample := range data.samples {
			if err != nil {
				break
			}
			phase, message := "measure", ""
			if sample.completionOffsetNS >= p.wallNS {
				phase = "drain"
			}
			if sample.errorIndex != 0 {
				message = data.errors[sample.errorIndex-1].Error()
			}
			err = w.Write([]string{phase, strconv.Itoa(worker), strconv.Itoa(worker + sequence*len(p.workers)), strconv.FormatInt(sample.latencyNS, 10), strconv.Itoa(sample.attempts), message, strconv.FormatBool(sample.errorIndex == 0), strconv.FormatInt(sample.completionOffsetNS, 10)})
		}
	}
	w.Flush()
	return errors.Join(err, w.Error(), f.Close())
}

// Two alternating operands per worker keep writes changing without generation
// or formatting in the timed loop. Each phase uses a distinct payload identity.
func durationOperands(workers int, workload string, phase byte) [][][]byte {
	out := make([][][]byte, workers)
	for worker := range out {
		for slot := range 2 {
			value := Payload(100, worker, slot, phase)
			if size := batchValueBytes(workload); size > 0 {
				value = batchSet50Payload(size, worker, slot, phase)
			}
			out[worker] = append(out[worker], value)
		}
	}
	return out
}

func (d Dataset) validateDurationWrites(db Database, workload string, expected [][]byte) error {
	return db.Transact(true, func(tx Transaction) error {
		for worker, key := range d.Writes {
			want := d.Initial
			if workload == "set100" {
				want = expected[worker]
			}
			got, err := tx.Get(key)
			if err != nil {
				return err
			}
			if err := CheckValue(got, want); err != nil {
				return fmt.Errorf("persisted worker %d: %w", worker, err)
			}
		}
		for worker, keys := range d.BatchWrites {
			for slot, key := range keys {
				got, err := tx.Get(key)
				if err != nil {
					return err
				}
				if err := CheckValue(got, expected[worker][slot*d.BatchValueBytes:(slot+1)*d.BatchValueBytes]); err != nil {
					return fmt.Errorf("persisted batch worker %d slot %d: %w", worker, slot, err)
				}
			}
		}
		return nil
	})
}

func durationRatio(numerator, denominator float64) *float64 {
	if denominator == 0 {
		return nil
	}
	value := numerator / denominator
	return &value
}

func durationSummary(cfg Config, measured durationPhase) map[string]any {
	p50, p99, errorP99 := measured.quantiles()
	cpuNS := measured.after.UserNS - measured.before.UserNS + measured.after.SystemNS - measured.before.SystemNS
	completed := measured.successful + measured.failed + measured.incorrect
	var throughput, cores float64
	if measured.wallNS > 0 {
		throughput = float64(measured.successful) * 1e9 / float64(measured.wallNS)
		cores = float64(cpuNS) / float64(measured.wallNS)
	}
	return map[string]any{
		"config": cfg, "mode": "duration", "requested_duration_ns": cfg.Duration.Nanoseconds(),
		"measured_wall_ns": measured.wallNS, "timer_lateness_ns": measured.timerLatenessNS,
		"successful": measured.successful, "terminal_errors": measured.failed, "correctness_errors": measured.incorrect, "retries": measured.retries, "total_completed": completed,
		"process_cpu_ns": cpuNS, "successful_ops_per_second": throughput,
		"cpu_ns_per_successful_op": durationRatio(float64(cpuNS), float64(measured.successful)), "cores_busy": cores,
		"success_p50_ns": p50, "success_p99_ns": p99, "error_p99_ns": errorP99,
		"terminal_errors_per_1000_completed_ops": durationRatio(float64(measured.failed)*1000, float64(completed)),
		"retries_per_1000_completed_ops":         durationRatio(float64(measured.retries)*1000, float64(completed)),
		"boundary_drain_raw":                     measured.boundaryDrain, "boundary_drain_wall_ns": measured.drainWallNS,
		"boundary_drain_correctness_errors": measured.boundaryIncorrect,
		"cpu_before":                        measured.before, "cpu_at_window_stop": measured.after,
	}
}

func runDuration(cfg Config, db LifecycleDatabase, metadata map[string]any, afterIdentity Stats) error {
	if err := os.MkdirAll(cfg.Output, 0o755); err != nil {
		return err
	}
	attemptsScope := "transaction callback entries only; retries=sum(max(attempts-1,0)) for completed logical operations in the observed window; internal RPC retries and hedges unobserved"
	windowScope := "one common observed [start, timer-wake stop) window; requested duration plus recorded timer lateness; success and terminal-error latencies separate; boundary completions excluded and drained before validation/shutdown"
	if err := WriteJSON(filepath.Join(cfg.Output, "manifest.json"), map[string]any{
		"config": cfg, "backend": metadata, "pid": os.Getpid(), "go_version": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0),
		"started_utc": time.Now().UTC(), "timing": timing(cfg), "mode": "duration", "window_scope": windowScope, "attempts_scope": attemptsScope,
		"cpu_scope":     "process user+system CPU snapshots adjacent to observed window boundaries; stop snapshot precedes boundary-drain join but may include concurrent boundary work up to the snapshot; excludes quantiles, CSV, validation and shutdown",
		"payload_scope": "two alternating pregenerated worker-local payloads per phase; final successful persisted writes checked including drain",
		"sample_scope":  "timed-samples.csv contains every measured operation and boundary drain, no warmup rows; exact nearest-rank quantiles after the window",
		"environment":   map[string]string{"GOMAXPROCS": os.Getenv("GOMAXPROCS"), "GOGC": os.Getenv("GOGC"), "GOMEMLIMIT": os.Getenv("GOMEMLIMIT"), "GODEBUG": os.Getenv("GODEBUG")},
	}); err != nil {
		return err
	}
	d := Prepare(cfg.Prefix, cfg.Workers, 0, 0)
	if size := batchValueBytes(cfg.Workload); size > 0 {
		d.prepareBatchSet50(cfg.Prefix, size)
	}
	warmOperands := durationOperands(cfg.Workers, cfg.Workload, 'w')
	measureOperands := durationOperands(cfg.Workers, cfg.Workload, 'm')
	expected := make([][]byte, cfg.Workers)
	for worker := range expected {
		expected[worker] = d.Initial
		if len(d.BatchInitial) > 0 {
			expected[worker] = d.BatchInitial[worker]
		}
	}
	operation := func(operands [][][]byte) func(int, int) (int, error) {
		return func(index, worker int) (int, error) {
			payload := operands[worker][(index/cfg.Workers)%2]
			attempts, err := d.Operation(db, cfg.Workload, worker, payload)
			if err == nil && (cfg.Workload == "set100" || batchValueBytes(cfg.Workload) > 0) {
				expected[worker] = payload
			}
			return attempts, err
		}
	}
	var runErr error
	failureStage := ""
	fail := func(stage string, err error) {
		if err != nil {
			if failureStage == "" {
				failureStage = stage
			}
			runErr = errors.Join(runErr, err)
		}
	}
	var warm, measured durationPhase
	var before, after, delta ClientCounters
	fail("seed", d.Seed(db))
	if runErr == nil {
		fail("seed_validation", d.ValidateSeed(db))
	}
	if runErr == nil {
		warm = runDurationPhase(cfg.WarmupDuration, cfg.Workers, operation(warmOperands), false, readProcessCPU)
		fail("warmup", warm.statsError)
		fail("warmup_correctness", warm.firstCorrectnessError)
	}
	if runErr == nil {
		stop, err := StartProfile(cfg.Profile, cfg.Output)
		fail("profile", err)
		if err == nil {
			before = db.Counters()
			measured = runDurationPhase(cfg.Duration, cfg.Workers, operation(measureOperands), true, readProcessCPU)
			after = db.Counters()
			delta, err = CounterDelta(before, after)
			fail("measure", errors.Join(measured.statsError, err))
			fail("measured_correctness", measured.firstCorrectnessError)
			if err == nil && measured.successful > 0 {
				fail("counters", ValidateGRVWorkload(delta, cfg.Workload, measured.successful))
			}
			fail("profile_stop", stop())
			if measured.successful+measured.failed == 0 {
				fail("measure", errors.New("no logical operations completed in the measured window"))
			}
		}
	}
	// Workers have joined even on terminal errors. Cleanup never closes a
	// database concurrently with a measured operation.
	var lifecycle Lifecycle
	var err error
	lifecycle.MeasurementDrain, err = DrainFinalizers()
	fail("measurement_drain", err)
	if err == nil {
		err = db.Barrier()
		lifecycle.MeasurementBarrier = err == nil
		fail("measurement_barrier", err)
	}
	lifecycle.AfterDrain, err = ReadStats(true)
	fail("drain_stats", err)
	postValidation := "error"
	if err := d.validateDurationWrites(db, cfg.Workload, expected); err != nil {
		fail("persisted_validation", err)
	} else {
		postValidation = "ok"
	}
	fail("samples", writeDurationSamples(filepath.Join(cfg.Output, "timed-samples.csv"), measured))
	summary := durationSummary(cfg, measured)
	lifecycle.ShutdownDrain, err = DrainFinalizers()
	fail("shutdown_drain", err)
	lifecycle.NetworkStopped, err = db.Shutdown()
	lifecycle.DatabaseClosed = err == nil
	fail("shutdown", err)
	lifecycle.AfterShutdown, err = ReadStats(true)
	lifecycle.Complete = err == nil && lifecycle.DatabaseClosed && lifecycle.MeasurementBarrier && lifecycle.MeasurementDrain.Complete && lifecycle.ShutdownDrain.Complete
	fail("shutdown_stats", err)
	summary["backend"], summary["stats_after_identity"], summary["lifecycle"] = metadata, afterIdentity, lifecycle
	summary["client_counters_before"], summary["client_counters_after"], summary["client_counters_delta"] = before, after, delta
	summary["client_counters_scope"] = "raw measurement plus boundary drain; not used as headline window counts"
	summary["attempts_scope"], summary["window_scope"] = attemptsScope, windowScope
	summary["warmup"] = map[string]any{"requested_duration_ns": cfg.WarmupDuration.Nanoseconds(), "measured_wall_ns": warm.wallNS, "timer_lateness_ns": warm.timerLatenessNS, "successful": warm.successful, "terminal_errors": warm.failed, "correctness_errors": warm.incorrect, "retries": warm.retries, "boundary_drain_raw": warm.boundaryDrain, "boundary_drain_wall_ns": warm.drainWallNS, "boundary_drain_correctness_errors": warm.boundaryIncorrect}
	summary["first_warmup_error_raw"], summary["first_measurement_error_raw"] = "", ""
	if warm.firstError != nil {
		summary["first_warmup_error_raw"] = warm.firstError.Error()
	}
	if measured.firstError != nil {
		summary["first_measurement_error_raw"] = measured.firstError.Error()
	}
	summary["persisted_validation"], summary["failure_stage"], summary["success"] = postValidation, failureStage, runErr == nil
	summary["failure"] = ""
	if runErr != nil {
		summary["failure"] = runErr.Error()
	}
	return errors.Join(runErr, WriteJSON(filepath.Join(cfg.Output, "timed-summary.json"), summary))
}
