package bench

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDurationConfigGuardsAndCountCompatibility(t *testing.T) {
	t.Parallel()
	base := Config{ClusterFile: "cluster", Output: "output", AllowWrites: true, Prefix: "clientbench/timed/", Iterations: 1, Workers: 1, TimeoutMS: 30000, RetryLimit: 10, Workload: "grv", Profile: "none"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	timed := base
	timed.Duration, timed.WarmupDuration = 30*time.Second, 10*time.Second
	timed.Iterations, timed.Warmup = 0, -1 // count limits are unused in duration mode
	if err := timed.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Duration = -1 },
		func(c *Config) { c.WarmupDuration = -1 },
		func(c *Config) { c.Rate = 1 },
		func(c *Config) { c.Duration = time.Duration(math.MaxInt64) },
		func(c *Config) { c.WarmupDuration = time.Duration(math.MaxInt64) },
	} {
		cfg := timed
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Fatal("invalid duration config accepted")
		}
	}
	base.Iterations = 0
	if base.Validate() == nil {
		t.Fatal("count mode no longer rejects zero iterations")
	}
}

func durationFixture() durationPhase {
	p := durationPhase{
		wallNS: 1000, before: processCPU{UserNS: 100, SystemNS: 200}, after: processCPU{UserNS: 300, SystemNS: 300},
		workers: []durationWorker{
			{samples: []durationSample{
				{latencyNS: 10, completionOffsetNS: 100, attempts: 1},
				{latencyNS: 900, completionOffsetNS: 900, attempts: 4, errorIndex: 1},
				{latencyNS: 9999, completionOffsetNS: 1000, attempts: 9},
			}, errors: []error{errors.New("terminal")}},
			{samples: []durationSample{
				{latencyNS: 20, completionOffsetNS: 500, attempts: 2},
				{latencyNS: 30, completionOffsetNS: 999, attempts: 0},
				{latencyNS: 8888, completionOffsetNS: 1001, attempts: 10, errorIndex: 1},
			}, errors: []error{errors.New("drain failure")}},
		},
	}
	p.collect()
	return p
}

func TestDurationWindowCountsQuantilesAndCPU(t *testing.T) {
	t.Parallel()
	p := durationFixture()
	if p.successful != 3 || p.failed != 1 || p.retries != 4 || p.boundaryDrain != 2 {
		t.Fatalf("wrong common-window population: %+v", p)
	}
	s := durationSummary(Config{Duration: time.Second}, p)
	if s["total_completed"] != 4 || s["process_cpu_ns"] != int64(300) || s["successful_ops_per_second"] != float64(3000000) || s["cores_busy"] != 0.3 {
		t.Fatalf("wrong rates/CPU: %+v", s)
	}
	for key, want := range map[string]float64{"cpu_ns_per_successful_op": 100, "terminal_errors_per_1000_completed_ops": 250, "retries_per_1000_completed_ops": 1000} {
		if got := s[key].(*float64); got == nil || *got != want {
			t.Fatalf("%s=%v, want %v", key, got, want)
		}
	}
	for key, want := range map[string]int64{"success_p50_ns": 20, "success_p99_ns": 30, "error_p99_ns": 900} {
		if got := s[key].(*int64); got == nil || *got != want {
			t.Fatalf("%s=%v, want %d", key, got, want)
		}
	}
}

func TestDurationQuantilesMatchNearestRankAcrossWorkers(t *testing.T) {
	t.Parallel()
	buffers := [][]int64{{1, 1, 3, 100}, {}, {2, 3, 4}, {1, 2, 10, 50}}
	var joined []int64
	for _, values := range buffers {
		joined = append(joined, values...)
	}
	slices.Sort(joined)
	for _, percentile := range []int{1, 50, 90, 99, 100} {
		want := joined[(len(joined)*percentile+99)/100-1]
		if got := durationQuantile(buffers, percentile); got == nil || *got != want {
			t.Fatalf("p%d=%v, want %d", percentile, got, want)
		}
	}
	if durationQuantile([][]int64{{}, nil}, 99) != nil {
		t.Fatal("empty population has a quantile")
	}
}

func TestDurationEmptySignalsAreNullNotInventedZeros(t *testing.T) {
	t.Parallel()
	summary := durationSummary(Config{}, durationPhase{})
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"success_p50_ns", "success_p99_ns", "error_p99_ns", "cpu_ns_per_successful_op", "terminal_errors_per_1000_completed_ops", "retries_per_1000_completed_ops"} {
		if value, ok := decoded[name]; !ok || value != nil {
			t.Fatalf("%s missing or not null: %v", name, value)
		}
	}
}

func TestDurationRawSamplesProveErrorRetriesAndBoundary(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "timed-samples.csv")
	if err := writeDurationSamples(path, durationFixture()); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(f).ReadAll()
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	want := [][]string{
		{"phase", "worker", "index", "latency_ns", "attempts", "error", "successful", "completion_offset_ns"},
		{"measure", "0", "0", "10", "1", "", "true", "100"},
		{"measure", "0", "2", "900", "4", "terminal", "false", "900"},
		{"drain", "0", "4", "9999", "9", "", "true", "1000"},
		{"measure", "1", "1", "20", "2", "", "true", "500"},
		{"measure", "1", "3", "30", "0", "", "true", "999"},
		{"drain", "1", "5", "8888", "10", "drain failure", "false", "1001"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("raw sample population differs: got %v want %v", rows, want)
	}
	if err := writeDurationSamples(path, durationFixture()); err == nil {
		t.Fatal("overwrote raw samples")
	}
}

func TestDurationPhaseContinuesAfterErrorAndCapturesCPUBeforeDrain(t *testing.T) {
	t.Parallel()
	failure := errors.New("terminal")
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	reads := 0
	readCPU := func() (processCPU, error) {
		reads++
		if reads == 2 {
			select {
			case <-started:
			case <-time.After(time.Second):
				close(release)
				return processCPU{}, errors.New("worker did not start before the CPU stop snapshot")
			}
			close(release) // A blocked operation may finish only after the stop snapshot.
		}
		return processCPU{UserNS: int64(reads) * 100}, nil
	}
	p := runDurationPhase(100*time.Millisecond, 1, func(index, worker int) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
			return 3, failure
		}
		<-release
		return 2, nil
	}, true, readCPU)
	if p.statsError != nil || reads != 2 || p.wallNS < int64(100*time.Millisecond) || p.after.UserNS-p.before.UserNS != 100 {
		t.Fatalf("invalid window/CPU snapshots: %+v reads=%d", p, reads)
	}
	if p.failed != 1 || p.successful != 0 || p.retries != 2 || p.boundaryDrain != 1 || calls.Load() != 2 || !errors.Is(p.firstError, failure) {
		t.Fatalf("error aborted the window or drain leaked into it: calls=%d phase=%+v", calls.Load(), p)
	}
}

func TestDurationCPUReadFailureStartsNoOperations(t *testing.T) {
	t.Parallel()
	failure := errors.New("getrusage failed")
	var calls atomic.Int32
	p := runDurationPhase(time.Second, 4, func(int, int) (int, error) { calls.Add(1); return 1, nil }, true, func() (processCPU, error) { return processCPU{}, failure })
	if !errors.Is(p.statsError, failure) || calls.Load() != 0 || p.wallNS != 0 {
		t.Fatalf("CPU failure did not fail closed: calls=%d phase=%+v", calls.Load(), p)
	}
}

func TestDurationOperandsAndPersistedValidationIncludeFinalDrainWrite(t *testing.T) {
	t.Parallel()
	for _, workload := range []string{"set100", "batchset50", "batchset50x1000"} {
		t.Run(workload, func(t *testing.T) {
			t.Parallel()
			const prefix = "clientbench/timed-persisted/"
			d := Prepare(prefix, 2, 0, 0)
			if size := batchValueBytes(workload); size > 0 {
				d.prepareBatchSet50(prefix, size)
			}
			warm := durationOperands(2, workload, 'w')
			measure := durationOperands(2, workload, 'm')
			r := &batchShapeRecorder{}
			if err := d.Seed(r); err != nil {
				t.Fatal(err)
			}
			expected := make([][]byte, 2)
			for worker := range 2 {
				if bytes.Equal(warm[worker][0], measure[worker][0]) || bytes.Equal(measure[worker][0], measure[worker][1]) || bytes.Equal(measure[worker][0], measure[1-worker][0]) {
					t.Fatal("phase, slot or worker payload identity was reused")
				}
				for _, payload := range [][]byte{warm[worker][0], measure[worker][0], measure[worker][1]} {
					if attempts, err := d.Operation(r, workload, worker, payload); err != nil || attempts != 1 {
						t.Fatalf("operation attempts=%d err=%v", attempts, err)
					}
					expected[worker] = payload
				}
			}
			if err := d.validateDurationWrites(r, workload, expected); err != nil {
				t.Fatal(err)
			}
			key := d.Writes[0]
			if len(d.BatchWrites) > 0 {
				key = d.BatchWrites[0][49]
			}
			r.values[string(key)][len(r.values[string(key)])-1] ^= 1
			if err := d.validateDurationWrites(r, workload, expected); err == nil {
				t.Fatal("same-length corruption of the final persisted write was accepted")
			}
		})
	}
}

// This fake exercises the runner's error policy, not FDB transaction semantics.
// Alternate workload operations reenter the callback once and then fail; seed
// and persisted validation transactions remain successful.
type durationErrorDatabase struct {
	prefix          string
	values          map[string][]byte
	operations      int
	shutdown        bool
	corruptMeasured bool
	measuring       bool
}

type durationErrorTransaction struct {
	db                     *durationErrorDatabase
	sets                   []KV
	gets, pointReads, grvs int
}

func (d *durationErrorDatabase) Transact(_ bool, fn func(Transaction) error) error {
	tx := &durationErrorTransaction{db: d}
	if err := fn(tx); err != nil {
		return err
	}
	operation := tx.grvs > 0 || (tx.gets == 1 && tx.pointReads == 1) || (len(tx.sets) == 1 && string(tx.sets[0].Key) == d.prefix+"write/0000")
	if operation {
		d.operations++
		time.Sleep(time.Millisecond)
		if d.operations%2 == 1 {
			if err := fn(&durationErrorTransaction{db: d}); err != nil {
				return err
			}
			return errors.New("controlled terminal error")
		}
	}
	for _, kv := range tx.sets {
		d.values[string(kv.Key)] = bytes.Clone(kv.Value)
	}
	return nil
}

func (d *durationErrorDatabase) Local(fn func(Transaction) error) error {
	return fn(&durationErrorTransaction{db: d})
}

func (d *durationErrorDatabase) Counters() ClientCounters {
	d.measuring = true
	return ClientCounters{Observation: "unobserved"}
}
func (*durationErrorDatabase) Close()         {}
func (*durationErrorDatabase) Barrier() error { return nil }
func (d *durationErrorDatabase) Shutdown() (bool, error) {
	d.shutdown = true
	return false, nil
}

func (tx *durationErrorTransaction) Get(key []byte) ([]byte, error) {
	tx.gets++
	if string(key) == tx.db.prefix+"point/00100" {
		tx.pointReads++
		if tx.db.corruptMeasured && tx.db.measuring {
			value := bytes.Clone(tx.db.values[string(key)])
			value[0] ^= 1
			return value, nil
		}
	}
	return bytes.Clone(tx.db.values[string(key)]), nil
}

func (tx *durationErrorTransaction) Set(key, value []byte) {
	tx.sets = append(tx.sets, KV{Key: bytes.Clone(key), Value: bytes.Clone(value)})
}

func (tx *durationErrorTransaction) Range(begin, end []byte, limit int) ([]KV, error) {
	var rows []KV
	for key, value := range tx.db.values {
		if bytes.Compare([]byte(key), begin) >= 0 && bytes.Compare([]byte(key), end) < 0 {
			rows = append(rows, KV{Key: []byte(key), Value: bytes.Clone(value)})
		}
	}
	slices.SortFunc(rows, func(a, b KV) int { return bytes.Compare(a.Key, b.Key) })
	if tx.db.corruptMeasured && tx.db.measuring && len(rows) > 0 {
		rows[0].Value[0] ^= 1
	}
	return rows[:min(len(rows), limit)], nil
}

func (tx *durationErrorTransaction) ReadVersion() (int64, error) {
	tx.grvs++
	if tx.db.corruptMeasured && tx.db.measuring {
		return 0, nil
	}
	return 1, nil
}

// Not parallel: Run performs the same process-wide finalizer drains as the
// executable. Other finalizer tests must not mutate their metrics concurrently.
func TestDurationRunReportsTerminalErrorsWithoutFailingPopulation(t *testing.T) {
	for _, workload := range []string{"get100", "set100", "grv"} {
		t.Run(workload, func(t *testing.T) {
			prefix := "clientbench/timed-errors/" + workload + "/"
			db := &durationErrorDatabase{prefix: prefix, values: make(map[string][]byte)}
			cfg := Config{ClusterFile: "fake", Output: t.TempDir(), Prefix: prefix, AllowWrites: true, Workload: workload, Workers: 1, TimeoutMS: 1000, RetryLimit: 10, Profile: "none", WarmupDuration: 20 * time.Millisecond, Duration: 30 * time.Millisecond}
			if err := Run(cfg, db, map[string]any{"backend": "fake"}, Stats{}); err != nil {
				t.Fatalf("terminal errors invalidated a complete population: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(cfg.Output, "timed-summary.json"))
			if err != nil {
				t.Fatal(err)
			}
			var summary map[string]any
			if err := json.Unmarshal(data, &summary); err != nil {
				t.Fatal(err)
			}
			warm := summary["warmup"].(map[string]any)
			if summary["success"] != true || summary["persisted_validation"] != "ok" || summary["failure"] != "" || !db.shutdown {
				t.Fatalf("runner did not finish successfully: %+v shutdown=%t", summary, db.shutdown)
			}
			if summary["terminal_errors"].(float64) <= 0 || summary["successful"].(float64) <= 0 || warm["terminal_errors"].(float64) <= 0 || summary["error_p99_ns"] == nil {
				t.Fatalf("missing error/success population: %+v", summary)
			}
			if summary["first_warmup_error_raw"] != "controlled terminal error" || summary["first_measurement_error_raw"] != "controlled terminal error" || summary["retries"] != summary["terminal_errors"] {
				t.Fatalf("missing raw error/callback retry evidence: %+v", summary)
			}
			f, err := os.Open(filepath.Join(cfg.Output, "timed-samples.csv"))
			if err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(f).ReadAll()
			if closeErr := f.Close(); err != nil || closeErr != nil {
				t.Fatal(errors.Join(err, closeErr))
			}
			var terminalErrors int
			for _, row := range rows[1:] {
				if row[0] == "measure" && row[6] == "false" {
					terminalErrors++
					if row[4] != "2" || row[5] != "controlled terminal error" {
						t.Fatalf("raw callback/error evidence missing: %v", row)
					}
				}
			}
			if float64(terminalErrors) != summary["terminal_errors"] {
				t.Fatalf("raw errors=%d differ from summary %v", terminalErrors, summary["terminal_errors"])
			}
		})
	}
}

func TestDurationCorrectnessErrorsAreNotTerminalErrors(t *testing.T) {
	t.Parallel()
	p := durationPhase{wallNS: 1000, workers: []durationWorker{{
		errors: []error{CheckValue([]byte("wrong"), []byte("right")), CheckRange(nil, []KV{{}}), errors.New("terminal")},
		samples: []durationSample{
			{latencyNS: 10, completionOffsetNS: 100, attempts: 1},
			{latencyNS: 900, completionOffsetNS: 200, attempts: 1, errorIndex: 1},
			{latencyNS: 800, completionOffsetNS: 300, attempts: 1, errorIndex: 2},
			{latencyNS: 700, completionOffsetNS: 400, attempts: 1, errorIndex: 3},
			{latencyNS: 600, completionOffsetNS: 1000, attempts: 1, errorIndex: 1},
		},
	}}}
	p.collect()
	summary := durationSummary(Config{}, p)
	for name, want := range map[string]int{"successful": 1, "terminal_errors": 1, "correctness_errors": 2, "boundary_drain_correctness_errors": 1, "total_completed": 4} {
		if summary[name] != want {
			t.Fatalf("%s=%v, want %d", name, summary[name], want)
		}
	}
	for name, want := range map[string]int64{"success_p50_ns": 10, "success_p99_ns": 10, "error_p99_ns": 700} {
		if got := summary[name].(*int64); got == nil || *got != want {
			t.Fatalf("%s=%v, want %d", name, got, want)
		}
	}
}

// Not parallel: Run drains process-wide finalizers.
func TestDurationRunRejectsMeasuredCorrectnessFailures(t *testing.T) {
	for _, workload := range []string{"get100", "range10", "range100", "grv"} {
		t.Run(workload, func(t *testing.T) {
			prefix := "clientbench/timed-corruption/" + workload + "/"
			db := &durationErrorDatabase{prefix: prefix, values: make(map[string][]byte), corruptMeasured: true}
			cfg := Config{ClusterFile: "fake", Output: t.TempDir(), Prefix: prefix, AllowWrites: true, Workload: workload, Workers: 1, TimeoutMS: 1000, RetryLimit: 10, Profile: "none", Duration: 30 * time.Millisecond}
			err := Run(cfg, db, map[string]any{"backend": "fake"}, Stats{})
			if err == nil || (!strings.Contains(err.Error(), "mismatch") && !strings.Contains(err.Error(), "invalid read version")) {
				t.Fatalf("wrong read result did not fail the run: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(cfg.Output, "timed-summary.json"))
			if err != nil {
				t.Fatal(err)
			}
			var summary map[string]any
			if err := json.Unmarshal(data, &summary); err != nil {
				t.Fatal(err)
			}
			if summary["success"] != false || summary["failure_stage"] != "measured_correctness" || summary["persisted_validation"] != "ok" || !db.shutdown {
				t.Fatalf("correctness failure did not invalidate a fully cleaned-up run: %+v", summary)
			}
			if summary["terminal_errors"] != float64(0) || summary["correctness_errors"].(float64)+summary["boundary_drain_correctness_errors"].(float64) <= 0 {
				t.Fatalf("correctness failure was counted as a transaction error: %+v", summary)
			}
			if summary["success_p50_ns"] != nil || summary["error_p99_ns"] != nil || summary["first_measurement_error_raw"] == "" {
				t.Fatalf("correctness failure leaked into latency populations or lost its diagnostic: %+v", summary)
			}
		})
	}
}
