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
