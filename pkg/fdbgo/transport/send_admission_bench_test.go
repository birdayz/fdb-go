package transport

import (
	"testing"
)

// BenchmarkWarmLegacyEnqueue measures the legacy deferred-send admission path
// without socket, writer-goroutine, or queue-backpressure costs.
func BenchmarkWarmLegacyEnqueue(b *testing.B) {
	conn := testOwnedConn(1)
	defer conn.cancel()
	body := make([]byte, 32)
	token := UID{First: 262}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := conn.SendFrameDeferred(token, body); err != nil {
			b.Fatal(err)
		}
		<-conn.writeCh
		conn.queued.Add(-1)
	}
}

func TestWarmLegacyEnqueueAllocs(t *testing.T) {
	conn := testOwnedConn(1)
	defer conn.cancel()
	body := make([]byte, 32)
	token := UID{First: 262}
	if n := testing.AllocsPerRun(1000, func() {
		if err := conn.SendFrameDeferred(token, body); err != nil {
			panic(err)
		}
		<-conn.writeCh
		conn.queued.Add(-1)
	}); n != 0 {
		t.Fatalf("warm legacy enqueue allocs = %g, want 0", n)
	}
}
