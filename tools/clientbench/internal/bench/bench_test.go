package bench

import (
	"bytes"
	"encoding/csv"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreparedDataset(t *testing.T) {
	t.Parallel()
	d := Prepare("clientbench/test/", 4, 9, 17)
	if len(d.Range) != 100 || len(d.Writes) != 4 || len(d.Warmup) != 9 || len(d.Measure) != 17 {
		t.Fatal("wrong dataset population")
	}
	for _, size := range []int{100, 1000, 10000} {
		if len(d.Points[size].Value) != size {
			t.Fatalf("point size %d", size)
		}
	}
	for i, row := range d.Range {
		if len(row.Value) != 8 {
			t.Fatalf("range %d size", i)
		}
		if i > 0 && bytes.Compare(d.Range[i-1].Key, row.Key) >= 0 {
			t.Fatal("range keys not strictly ordered")
		}
	}
	if bytes.Compare(d.Range[99].Key, d.RangeEnd) >= 0 {
		t.Fatal("range end excludes data")
	}
	for i, value := range d.Measure {
		if len(value) != 100 || bytes.Equal(value, d.Initial) {
			t.Fatal("write is not a changing 100-byte overwrite")
		}
		if i > 0 && bytes.Equal(value, d.Measure[i-1]) {
			t.Fatal("duplicate write payload")
		}
		if i < len(d.Warmup) && bytes.Equal(value, d.Warmup[i]) {
			t.Fatal("warmup equals measured payload")
		}
	}
	for i, key := range d.Writes {
		for j, other := range d.Writes {
			if i != j && bytes.Equal(key, other) {
				t.Fatal("workers share a write key")
			}
		}
	}
}

func TestExactChecksRejectCorruption(t *testing.T) {
	t.Parallel()
	d := Prepare("clientbench/test/", 1, 0, 1)
	if err := CheckValue(d.Initial, append([]byte(nil), d.Initial...)); err != nil {
		t.Fatal(err)
	}
	if err := CheckValue(d.Initial, make([]byte, 100)); err == nil {
		t.Fatal("length-only point check")
	}
	if err := CheckValue(nil, d.Initial); err == nil {
		t.Fatal("missing value accepted")
	}
	if err := CheckRange(d.Range, d.Range); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"empty", "short", "extra", "key", "value", "order"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			rows := append([]KV(nil), d.Range...)
			switch kind {
			case "empty":
				rows = nil
			case "short":
				rows = rows[:99]
			case "extra":
				rows = append(rows, rows[99])
			case "key":
				rows[0].Key = []byte("wrong")
			case "value":
				rows[0].Value = []byte("badvalue")
			case "order":
				rows[0], rows[1] = rows[1], rows[0]
			}
			if CheckRange(rows, d.Range) == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
}

func TestPhaseRoundRobinAndRetries(t *testing.T) {
	t.Parallel()
	p := RunPhase(17, 4, func(i, w int) (int, error) {
		if w != i%4 {
			t.Errorf("worker %d for index %d", w, i)
		}
		return 2, nil
	})
	if p.Completed != 17 || p.Errors != 0 || p.Retries != 17 || p.WallNS <= 0 {
		t.Fatalf("bad phase: %+v", p)
	}
	for i, s := range p.Samples {
		if !s.Attempted || s.Index != i || s.Worker != i%4 || s.LatencyNS <= 0 || s.StartOffsetNS < 0 {
			t.Fatalf("bad sample: %+v", s)
		}
	}
}

func TestFailureIsRecordedWithoutPadding(t *testing.T) {
	t.Parallel()
	failure := errors.New("terminal failure")
	p := RunPhase(10, 1, func(i, w int) (int, error) {
		if i == 2 {
			return 3, failure
		}
		return 1, nil
	})
	if p.Completed != 3 || p.Errors != 1 || p.Retries != 2 || !errors.Is(p.Samples[2].Err, failure) {
		t.Fatalf("bad failed phase: %+v", p)
	}
	path := filepath.Join(t.TempDir(), "samples.csv")
	if err := WriteSamples(path, "measure", p.Samples); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[3][6] != "terminal failure" || rows[3][7] != "error" {
		t.Fatalf("failed CSV: %v", rows)
	}
	if err := WriteSamples(path, "measure", p.Samples); err == nil {
		t.Fatal("overwrote samples")
	}
}

func TestConfigGuards(t *testing.T) {
	t.Parallel()
	base := Config{ClusterFile: "cluster", Output: "output", AllowWrites: true, Prefix: "clientbench/v1/", Iterations: 1, Warmup: 0, Workers: 1, TimeoutMS: 30000, RetryLimit: 10, Workload: "get100", Profile: "none"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.AllowWrites = false }, func(c *Config) { c.Prefix = "" }, func(c *Config) { c.Iterations = 0 }, func(c *Config) { c.Warmup = -1 }, func(c *Config) { c.Workers = 0 }, func(c *Config) { c.TimeoutMS = 0 }, func(c *Config) { c.RetryLimit = -1 }, func(c *Config) { c.Workload = "wrong" }, func(c *Config) { c.Profile = "wrong" }} {
		cfg := base
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func mustSchedule(t *testing.T, cfg Config, n int, stream uint64) []int64 {
	t.Helper()
	s, err := schedule(cfg, n, stream)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPoissonScheduleIsSeededAndEven(t *testing.T) {
	t.Parallel()
	cfg := Config{Rate: 2000, Seed: 7}
	a, b := mustSchedule(t, cfg, 100000, measureStream), mustSchedule(t, cfg, 100000, measureStream)
	if !slices.Equal(a, b) {
		t.Fatal("one seed gave two schedules")
	}
	for _, seed := range []uint64{8, 7 ^ 1<<63} {
		if slices.Equal(a[:100], mustSchedule(t, Config{Rate: 2000, Seed: seed}, 100, measureStream)) {
			t.Fatalf("seeds 7 and %d gave one schedule", seed)
		}
	}
	prev := int64(0)
	for i, at := range a {
		if at <= prev && i > 0 {
			t.Fatalf("start %d at %d ns is not after %d ns", i, at, prev)
		}
		prev = at
	}
	// 100,000 exponential gaps: the mean is within 1% of 1/rate with overwhelming probability.
	if mean := float64(a[len(a)-1]) / float64(len(a)) / 1e9; math.Abs(mean*2000-1) > 0.01 {
		t.Fatalf("mean gap %v s, want 1/2000", mean)
	}
	if slices.Equal(mustSchedule(t, cfg, 100, measureStream), mustSchedule(t, cfg, 100, warmupStream)) {
		t.Fatal("warmup and measured phases share a schedule")
	}
	if mustSchedule(t, Config{}, 100, measureStream) != nil {
		t.Fatal("a closed loop has a schedule")
	}
}

func TestPacingSummary(t *testing.T) {
	t.Parallel()
	us := int64(time.Microsecond)
	var samples []Sample
	for i := range 100 {
		lag := int64(10) * us
		if i >= 95 {
			lag = 2000 * us // five starts 2 ms late
		}
		samples = append(samples, Sample{Attempted: true, ScheduledOffsetNS: int64(i) * 1000 * us, StartOffsetNS: int64(i)*1000*us + lag})
	}
	p := pacing(samples)
	if p == nil || p.Samples != 100 || p.LagP50NS != 10*us || p.LagMaxNS != 2000*us || p.LateOver1ms != 0.05 || p.LateOver100us != 0.05 {
		t.Fatalf("pacing summary %+v", p)
	}
	if p.GapCVScheduled != 0 || p.GapCVActual <= 0 {
		t.Fatalf("evenly scheduled starts: CV scheduled %v, actual %v", p.GapCVScheduled, p.GapCVActual)
	}
	if pacing([]Sample{{Attempted: true, ScheduledOffsetNS: -1}}) != nil {
		t.Fatal("a closed loop has a pacing summary")
	}
}

func TestPacedRateValidation(t *testing.T) {
	t.Parallel()
	base := Config{ClusterFile: "c", Output: "o", AllowWrites: true, Prefix: "clientbench/v1/", Iterations: 1, Workers: 1, TimeoutMS: 1, Workload: "get100", Profile: "none"}
	for _, rate := range []float64{math.NaN(), math.Inf(1), -1, 2e6} {
		cfg := base
		cfg.Rate = rate
		if cfg.Validate() == nil {
			t.Errorf("rate %v accepted", rate)
		}
	}
	for _, rate := range []float64{0, 1e-12, 1e6} {
		cfg := base
		cfg.Rate = rate
		if err := cfg.Validate(); err != nil {
			t.Errorf("rate %v rejected: %v", rate, err)
		}
	}
	// A tiny rate passes validation but would start beyond any representable offset.
	if _, err := PoissonSchedule(10, 1e-12, 1, measureStream); err == nil {
		t.Fatal("a schedule past the representable horizon was accepted")
	}
	for _, rate := range []float64{0, math.NaN(), math.Inf(1)} {
		if _, err := PoissonSchedule(1, rate, 1, measureStream); err == nil {
			t.Errorf("schedule rate %v accepted", rate)
		}
	}
}

// A failure ends a paced phase at once: workers already waiting for later
// starts wake, and nothing scheduled later runs. Not parallel: it sets
// paceWaitHook, which parallel tests never see because they run afterwards.
func TestPacedPhaseStopsOnFailure(t *testing.T) {
	failure := errors.New("terminal failure")
	sched := []int64{0, int64(time.Hour), int64(2 * time.Hour)}
	parked := make(chan int, 2)
	paceWaitHook = func(worker int) { parked <- worker }
	defer func() { paceWaitHook = nil }()
	var calls atomic.Int32
	done := make(chan Phase, 1)
	go func() {
		done <- runPhase(3, 3, sched, func(i, w int) (int, error) {
			calls.Add(1)
			if i == 0 {
				// Fail only once both later workers are waiting for their starts.
				for range 2 {
					select {
					case <-parked:
					case <-time.After(30 * time.Second):
						t.Error("PACED_ABORT_PRECONDITION: later workers never started waiting")
					}
				}
				return 1, failure
			}
			return 1, nil
		}, false)
	}()
	var p Phase
	select {
	case p = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("PACED_ABORT_WAITED: waiting workers did not wake on the failure")
	}
	if calls.Load() != 1 || p.Completed != 1 || p.Errors != 1 || p.Samples[1].Attempted || p.Samples[2].Attempted {
		t.Fatalf("PACED_ABORT_RAN: %d calls after a failure, phase %+v", calls.Load(), p)
	}
}

func TestPacedPhaseStartsOnSchedule(t *testing.T) {
	t.Parallel()
	sched := mustSchedule(t, Config{Rate: 1000, Seed: 3}, 40, measureStream)
	p := runPhase(40, 4, sched, func(i, w int) (int, error) {
		if w != i%4 {
			t.Errorf("worker %d for index %d", w, i)
		}
		return 1, nil
	}, false)
	if p.Completed != 40 || p.WallNS < sched[39] {
		t.Fatalf("paced phase %+v ended before its last start %d ns", p, sched[39])
	}
	for i, s := range p.Samples {
		if s.ScheduledOffsetNS != sched[i] || s.StartOffsetNS < s.ScheduledOffsetNS {
			t.Fatalf("sample %d started at %d ns, scheduled %d ns (recorded %d)", i, s.StartOffsetNS, sched[i], s.ScheduledOffsetNS)
		}
	}
	closed := RunPhase(4, 2, func(i, w int) (int, error) { return 1, nil })
	for _, s := range closed.Samples {
		if s.ScheduledOffsetNS != -1 {
			t.Fatalf("closed-loop sample has a schedule: %+v", s)
		}
	}
}
