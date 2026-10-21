package client

import (
	"runtime"
	"testing"
	"time"
	"unsafe"
)

// Not parallel. Queue initialization and the first runtime timer are per
// database, not free: report them separately from the per-transaction pin.
func TestTimeoutQueueInitializationAllocations(t *testing.T) {
	var retained *timeoutQueue
	queueOnly := testing.AllocsPerRun(100, func() { retained = newTimeoutQueue() })
	withTimer := testing.AllocsPerRun(100, func() {
		q := newTimeoutQueue()
		now := q.now()
		q.requestLocked(now+int64(time.Hour), now)
		q.timer.(*realTimer).t.Stop()
		retained = q
	})
	if queueOnly != 49 || withTimer != 50 {
		t.Fatalf("QUEUE_INIT_ALLOC: queue=%v with first runtime timer=%v, want 49/50", queueOnly, withTimer)
	}
	t.Logf("QUEUE_INIT_ALLOC: queue=%v with first timer=%v; root=%d shard=%d realTimer=%d bytes across %d shards, plus runtime timers and heap/ready backing arrays", queueOnly, withTimer, unsafe.Sizeof(timeoutQueueRoot{}), unsafe.Sizeof(timeoutQueue{}), unsafe.Sizeof(realTimer{}), len(retained.root.shards))
	runtime.KeepAlive(retained)
}
