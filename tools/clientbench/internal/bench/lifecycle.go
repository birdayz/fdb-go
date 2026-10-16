package bench

import (
	"context"
	"fmt"
	"runtime"
	"runtime/metrics"
	"time"
)

// LifecycleDatabase is owned by one benchmark process; Shutdown is terminal.
type LifecycleDatabase interface {
	Database
	Barrier() error
	Shutdown() (networkStopped bool, err error)
}

type CleanupCounters struct {
	FinalizersQueued   uint64 `json:"finalizers_queued"`
	FinalizersExecuted uint64 `json:"finalizers_executed"`
	CleanupsQueued     uint64 `json:"cleanups_queued_estimate"`
	CleanupsExecuted   uint64 `json:"cleanups_executed_estimate"`
	GCCycles           uint64 `json:"gc_cycles"`
}

type FinalizerDrain struct {
	Before   CleanupCounters `json:"before"`
	After    CleanupCounters `json:"after"`
	Rounds   int             `json:"rounds"`
	WallNS   int64           `json:"wall_ns"`
	Complete bool            `json:"complete"`
}

type Lifecycle struct {
	MeasurementDrain   FinalizerDrain `json:"measurement_drain"`
	MeasurementBarrier bool           `json:"measurement_barrier_complete"`
	AfterDrain         Stats          `json:"stats_after_measurement_drain"`
	ShutdownDrain      FinalizerDrain `json:"shutdown_drain"`
	DatabaseClosed     bool           `json:"database_closed"`
	NetworkStopped     bool           `json:"network_stopped"`
	AfterShutdown      Stats          `json:"stats_after_shutdown"`
	Complete           bool           `json:"complete"`
}

func drainFinalizers(ctx context.Context, maxRounds int) (report FinalizerDrain, err error) {
	start := time.Now()
	defer func() { report.WallNS = time.Since(start).Nanoseconds() }()
	samples := []metrics.Sample{
		{Name: "/gc/finalizers/queued:finalizers"},
		{Name: "/gc/finalizers/executed:finalizers"},
		{Name: "/gc/cleanups/queued:cleanups"},
		{Name: "/gc/cleanups/executed:cleanups"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	read := func() (CleanupCounters, error) {
		metrics.Read(samples)
		for _, sample := range samples {
			if sample.Value.Kind() != metrics.KindUint64 {
				return CleanupCounters{}, fmt.Errorf("required runtime metric unavailable: %s", sample.Name)
			}
		}
		return CleanupCounters{samples[0].Value.Uint64(), samples[1].Value.Uint64(), samples[2].Value.Uint64(), samples[3].Value.Uint64(), samples[4].Value.Uint64()}, nil
	}
	report.Before, err = read()
	if err != nil {
		return report, err
	}
	var previous, previousCleanups uint64
	stable := false
	for report.Rounds < maxRounds {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		before, err := read()
		if err != nil {
			return report, err
		}
		runtime.GC()
		report.Rounds++
		if err := ctx.Err(); err != nil {
			return report, err
		}
		after, err := read()
		if err != nil {
			return report, err
		}
		if after.GCCycles != before.GCCycles+1 {
			stable = false
			continue
		}
		for {
			runtime.Gosched()
			if err := ctx.Err(); err != nil {
				return report, err
			}
			report.After, err = read()
			if err != nil {
				return report, err
			}
			if report.After.GCCycles != after.GCCycles {
				stable = false
				break
			}
			if report.After.FinalizersExecuted == report.After.FinalizersQueued && report.After.CleanupsExecuted == report.After.CleanupsQueued {
				// Finalizers and cleanups can release objects for the NEXT GC
				// cycle (the Go client stops an unreachable transaction's
				// timeout timer from a cleanup), so both must be stable.
				if stable && report.After.FinalizersQueued == previous && report.After.CleanupsQueued == previousCleanups {
					report.Complete = true
					return report, nil
				}
				previous, previousCleanups, stable = report.After.FinalizersQueued, report.After.CleanupsQueued, true
				break
			}
			select {
			case <-ctx.Done():
				return report, ctx.Err()
			case <-time.After(200 * time.Microsecond):
			}
		}
	}
	return report, fmt.Errorf("finalizer drain did not reach a fixpoint in %d rounds", maxRounds)
}

func DrainFinalizers() (FinalizerDrain, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return drainFinalizers(ctx, 16)
}
