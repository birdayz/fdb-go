//go:build stress

package stress_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/trace"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqldriver"
)

type latencyCapture struct {
	mu    sync.Mutex
	plans []embedded.PlanGenerationInfo
	execs []embedded.ExecutionStats
}

func (c *latencyCapture) LogPlanGeneration(_ context.Context, info embedded.PlanGenerationInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plans = append(c.plans, info)
}

func (c *latencyCapture) LogExecutionStats(_ context.Context, stats embedded.ExecutionStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execs = append(c.execs, stats)
}

type latencyTimerDelta struct {
	Count int64 `json:"count"`
	Value int64 `json:"value"`
}

type latencySample struct {
	SQL         string                        `json:"sql"`
	Started     time.Time                     `json:"started"`
	Acquire     time.Duration                 `json:"connection_acquire_ns"`
	Query       time.Duration                 `json:"query_ns"`
	Rows        int                           `json:"rows"`
	Plans       []embedded.PlanGenerationInfo `json:"plans"`
	Executions  []embedded.ExecutionStats     `json:"executions"`
	StoreTimers map[string]latencyTimerDelta  `json:"store_timers"`
}

// These spans overlap: execution contains record reads and store opening, and
// planning can open stores too. Never sum them as disjoint wall-clock phases.
func validateLatencySample(s latencySample) error {
	if len(s.Plans) != 1 || len(s.Executions) != 1 {
		return fmt.Errorf("missing or duplicated telemetry: plans=%d executions=%d", len(s.Plans), len(s.Executions))
	}
	if s.Plans[0].Err != nil || s.Executions[0].Err != nil {
		return fmt.Errorf("unsuccessful telemetry: plan=%v execution=%v", s.Plans[0].Err, s.Executions[0].Err)
	}
	if s.Executions[0].RowsReturned != int64(s.Rows) {
		return fmt.Errorf("execution rows=%d drained rows=%d", s.Executions[0].RowsReturned, s.Rows)
	}
	if s.Plans[0].PlanningDuration <= 0 || s.Executions[0].ExecutionDuration <= 0 || s.Query <= 0 {
		return fmt.Errorf("nonpositive timing: planning=%v execution=%v query=%v", s.Plans[0].PlanningDuration, s.Executions[0].ExecutionDuration, s.Query)
	}
	if len(s.StoreTimers) == 0 {
		return fmt.Errorf("record-store timer instrumentation emitted no deltas")
	}
	return nil
}

func observedStressQuery(h *stressHarness, timer *recordlayer.StoreTimer, query string, args ...any) queryResult {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	conn, err := h.db.Conn(ctx)
	if err != nil {
		return queryResult{Query: query, Duration: time.Since(start), Err: err}
	}
	defer conn.Close()
	sample := latencySample{SQL: query, Started: start, Acquire: time.Since(start)}
	capture := &latencyCapture{}
	if err := conn.Raw(func(raw any) error {
		ec, ok := raw.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("SQL connection is %T, not embedded", raw)
		}
		ec.SetPlanLogger(capture)
		ec.SetExecutionStatsLogger(capture)
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
	defer func() {
		if err := conn.Raw(func(raw any) error {
			ec := raw.(*embedded.EmbeddedConnection)
			ec.SetPlanLogger(nil)
			ec.SetExecutionStatsLogger(nil)
			return nil
		}); err != nil {
			h.t.Fatal(err)
		}
	}()
	before := timer.Snapshot()
	region := trace.StartRegion(ctx, "stress-query: "+query)
	queryStart := time.Now()
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		region.End()
		return queryResult{Query: query, Duration: time.Since(queryStart), Err: err}
	}
	for rows.Next() {
		sample.Rows++
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	sample.Query = time.Since(queryStart)
	region.End()
	if readErr != nil {
		return queryResult{Query: query, Duration: sample.Query, RowCount: sample.Rows, Err: readErr}
	}
	if closeErr != nil {
		return queryResult{Query: query, Duration: sample.Query, RowCount: sample.Rows, Err: closeErr}
	}
	sample.StoreTimers = make(map[string]latencyTimerDelta)
	for name, current := range timer.Snapshot() {
		delta := latencyTimerDelta{Count: current.Count, Value: current.CumulativeValue}
		if old := before[name]; old != nil {
			delta.Count -= old.Count
			delta.Value -= old.CumulativeValue
		}
		if delta.Count != 0 || delta.Value != 0 {
			sample.StoreTimers[name] = delta
		}
	}
	capture.mu.Lock()
	sample.Plans = append(sample.Plans, capture.plans...)
	sample.Executions = append(sample.Executions, capture.execs...)
	capture.mu.Unlock()
	encoded, err := json.Marshal(sample)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Logf("LATENCY_ATTRIBUTION %s", encoded)
	if err := validateLatencySample(sample); err != nil {
		h.t.Fatal(err)
	}
	// Connection acquisition is reported separately; callback installation,
	// snapshots and JSON formatting are outside the measured query interval.
	return queryResult{Query: query, Duration: sample.Query, RowCount: sample.Rows}
}

func newLatencyStressHarness(t *testing.T) *stressHarness {
	t.Helper()
	// The driver caches handles/timers by cluster-file path. A private copy
	// isolates telemetry from concurrent tests without changing the real FDB
	// server, client options, fixture loading or query ordering.
	content, err := os.ReadFile(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	clusterFile := filepath.Join(dir, "latency.cluster")
	if err := os.WriteFile(clusterFile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	// TempDir's final component is a per-test sequence (usually "001"),
	// not a unique name. Include its randomized parent as well: independent
	// client handles still share the same FDB catalog on an external cluster.
	suffix := filepath.Base(filepath.Dir(dir)) + "_" + filepath.Base(dir)
	return newStressHarnessWithCluster(t, suffix, clusterFile)
}

func TestFDB_LatencyHarnessIsolation(t *testing.T) {
	t.Parallel()
	var names sync.Map
	for _, value := range []int{11, 29} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			t.Parallel()
			h := newLatencyStressHarness(t)
			if _, loaded := names.LoadOrStore(h.dbPath, true); loaded {
				t.Fatalf("independent temporary directories reused database %s", h.dbPath)
			}
			h.createSchema(`CREATE TABLE marker (id BIGINT, v BIGINT, PRIMARY KEY (id))`)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if _, err := h.db.ExecContext(ctx, "INSERT INTO marker VALUES (1, ?)", value); err != nil {
				t.Fatal(err)
			}
			var got int
			if err := h.db.QueryRowContext(ctx, "SELECT v FROM marker WHERE id = 1").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != value {
				t.Fatalf("database %s read another fixture: got %d, want %d", h.dbPath, got, value)
			}
		})
	}
}

func TestFDB_Stress_1M_LatencyAttribution(t *testing.T) {
	t.Parallel()
	h := newLatencyStressHarness(t)
	timer := sqldriver.EnableStoreTimer(h.clusterFile)
	h.observeQuery = func(query string, args ...any) queryResult {
		return observedStressQuery(h, timer, query, args...)
	}
	runStressHarness(t, h, 1_000_000)
}

func TestLatencySampleValidation(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"valid", "missing_plan", "extra_plan", "missing_execution", "extra_execution", "plan_error", "execution_error", "wrong_rows", "no_planning_time", "no_execution_time", "no_query_time", "no_store_timers"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := latencySample{Rows: 1, Query: time.Millisecond, Plans: []embedded.PlanGenerationInfo{{PlanningDuration: time.Microsecond}}, Executions: []embedded.ExecutionStats{{RowsReturned: 1, ExecutionDuration: time.Microsecond}}, StoreTimers: map[string]latencyTimerDelta{"load_record": {Count: 1, Value: 1}}}
			switch name {
			case "missing_plan":
				s.Plans = nil
			case "extra_plan":
				s.Plans = append(s.Plans, s.Plans[0])
			case "missing_execution":
				s.Executions = nil
			case "extra_execution":
				s.Executions = append(s.Executions, s.Executions[0])
			case "plan_error":
				s.Plans[0].Err = fmt.Errorf("planning failed")
			case "execution_error":
				s.Executions[0].Err = fmt.Errorf("execution failed")
			case "wrong_rows":
				s.Rows++
			case "no_planning_time":
				s.Plans[0].PlanningDuration = 0
			case "no_execution_time":
				s.Executions[0].ExecutionDuration = 0
			case "no_query_time":
				s.Query = 0
			case "no_store_timers":
				s.StoreTimers = nil
			}
			if err := validateLatencySample(s); (err == nil) != (name == "valid") {
				t.Fatalf("%s: validation error=%v", name, err)
			}
		})
	}
}
