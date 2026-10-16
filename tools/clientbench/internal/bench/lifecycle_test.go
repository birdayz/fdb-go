package bench

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

type finalizerNode struct {
	next *finalizerNode
	pad  [1024]byte
}

//go:noinline
func unreachableFinalizerChain(count int, executed *atomic.Int64) {
	var head *finalizerNode
	for range count {
		head = &finalizerNode{next: head}
		runtime.SetFinalizer(head, func(*finalizerNode) { executed.Add(1) })
	}
	runtime.KeepAlive(head)
}

func TestFinalizerDrainFollowsDependencyChain(t *testing.T) {
	t.Parallel()
	var executed atomic.Int64
	unreachableFinalizerChain(5, &executed)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := drainFinalizers(ctx, 16)
	if err != nil || !report.Complete || report.Rounds < 2 || report.WallNS <= 0 || executed.Load() != 5 {
		t.Fatalf("finalizer chain not fully drained: executed=%d report=%+v err=%v", executed.Load(), report, err)
	}
	if report.After.FinalizersExecuted != report.After.FinalizersQueued || report.After.CleanupsQueued != report.Before.CleanupsQueued {
		t.Fatalf("invalid fixpoint counters: %+v", report)
	}
	t.Logf("CLIENTBENCH_FINALIZER_CHAIN completed=5 rounds=%d", report.Rounds)
}

type cleanupNode struct {
	pad [1024]byte
}

// unreachableCleanupChain registers count cleanups whose arguments hold the
// next node, so each node becomes unreachable only after its predecessor's
// cleanup has run: one more GC cycle per link.
//
//go:noinline
func unreachableCleanupChain(count int, executed *atomic.Int64) {
	var next *cleanupNode
	for range count {
		node := &cleanupNode{}
		runtime.AddCleanup(node, func(held *cleanupNode) {
			executed.Add(1)
			runtime.KeepAlive(held)
		}, next)
		next = node
	}
	runtime.KeepAlive(next)
}

// Not parallel: the other drain tests assert that no cleanups are queued.
func TestFinalizerDrainWaitsForCleanupChain(t *testing.T) {
	var executed atomic.Int64
	unreachableCleanupChain(5, &executed)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := drainFinalizers(ctx, 16)
	if err != nil || !report.Complete || report.Rounds < 2 || executed.Load() != 5 {
		t.Fatalf("cleanup chain not fully drained: executed=%d report=%+v err=%v", executed.Load(), report, err)
	}
	if report.After.CleanupsExecuted != report.After.CleanupsQueued || report.After.CleanupsQueued < report.Before.CleanupsQueued+5 {
		t.Fatalf("invalid cleanup fixpoint counters: %+v", report)
	}
	t.Logf("CLIENTBENCH_CLEANUP_CHAIN completed=5 rounds=%d", report.Rounds)
}

func TestFinalizerDrainFailsClosed(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report, err := drainFinalizers(ctx, 16); !errors.Is(err, context.Canceled) || report.Complete {
		t.Fatalf("canceled drain accepted: %+v %v", report, err)
	}
	if report, err := drainFinalizers(context.Background(), 0); err == nil || report.Complete {
		t.Fatalf("empty drain accepted: %+v %v", report, err)
	}
	if report, err := drainFinalizers(context.Background(), 1); err == nil || report.Complete {
		t.Fatalf("one GC treated as a fixpoint: %+v %v", report, err)
	}
}
