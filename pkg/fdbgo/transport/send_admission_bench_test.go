package transport

import (
	"bytes"
	"context"
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

// Not parallel: AllocsPerRun counts process-wide allocations. No socket or
// writer runs here; this pins the warmed enqueue path, including stack encoding.
func TestWarmDNGEnqueueAllocs(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		tls  bool
	}{
		{"plain-empty", 0, false},
		{"plain-small", 32, false},
		{"plain-stack-limit", 4096, false},
		{"tls-empty", 0, true},
		{"tls-small", 32, true},
		{"tls-stack-limit", 4096, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := testOwnedConn(0)
			defer conn.cancel()
			conn.useTLS = tc.tls
			body := bytes.Repeat([]byte{'x'}, tc.size)
			n := frameSize(body, tc.tls)
			conn.na = &naWriter{q: make([]byte, 0, n), sig: make(chan struct{}, 1)}
			token := UID{First: 282, Second: 7}
			ctx := context.Background()
			if ok, err := conn.SendFrameContext(ctx, token, body); !ok || err != nil {
				t.Fatalf("warm send = (%t, %v), want queued", ok, err)
			}
			gotToken, gotBody, err := ReadFrame(bytes.NewReader(conn.na.q), tc.tls)
			if err != nil || gotToken != token || !bytes.Equal(gotBody, body) {
				t.Fatalf("queued frame = (%v, %q, %v), want (%v, %q, nil)", gotToken, gotBody, err, token, body)
			}
			conn.na.q = conn.na.q[:0]
			<-conn.na.sig
			allocs := testing.AllocsPerRun(1000, func() {
				if ok, err := conn.SendFrameContext(ctx, token, body); !ok || err != nil {
					t.Fatalf("send = (%t, %v), want queued", ok, err)
				}
				if len(conn.na.q) != n {
					t.Fatalf("queued %d bytes, want %d", len(conn.na.q), n)
				}
				select {
				case <-conn.na.sig:
				default:
					t.Fatal("empty-to-nonempty enqueue did not wake the writer")
				}
				conn.na.q = conn.na.q[:0]
			})
			if allocs != 0 {
				t.Fatalf("warm D-NG enqueue allocs = %g, want 0", allocs)
			}
		})
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
