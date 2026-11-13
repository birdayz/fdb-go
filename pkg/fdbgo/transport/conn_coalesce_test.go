package transport

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// coalesceWriteCounter records each Write the connection makes.
type coalesceWriteCounter struct {
	net.Conn
	mu     sync.Mutex
	writes [][]byte
}

func (w *coalesceWriteCounter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, bytes.Clone(p))
	w.mu.Unlock()
	return w.Conn.Write(p)
}

func (w *coalesceWriteCounter) snapshot() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]byte(nil), w.writes...)
}

func dialWriteCounted(t *testing.T, opts ...DialOption) (*Conn, *coalesceWriteCounter) {
	t.Helper()
	return dialWriteCountedRaw(t, append([]DialOption{WithLegacyWriter()}, opts...)...)
}

func dialWriteCountedRaw(t *testing.T, opts ...DialOption) (*Conn, *coalesceWriteCounter) {
	t.Helper()
	s := newSimServer(t)
	counter := &coalesceWriteCounter{Conn: s.cli}
	go func() {
		if s.handshake() == nil {
			s.drainUntilClosed()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := func(context.Context, string, string) (net.Conn, error) { return counter, nil }
	c, err := dialWith(ctx, "sim", dial, nil, append([]DialOption{withMonitorCadence(time.Hour, time.Hour)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, counter
}

// Zero for both delays restores immediate writes; D-NG defaults to 0/0 while
// WithLegacyWriter defaults to libfdb_c's 10/20 µs; the last option wins.
func TestCoalesceDelayOption(t *testing.T) {
	t.Parallel()
	linux := runtime.GOOS == "linux"
	for _, tc := range []struct {
		name     string
		opts     []DialOption
		min, max time.Duration
		waits    bool
		adaptive time.Duration
	}{
		{"default", nil, 0, 0, false, 0},
		{"legacy-default", []DialOption{WithLegacyWriter()}, 10 * time.Microsecond, 20 * time.Microsecond, linux, 0},
		{"design-d-default", []DialOption{withNetDesign(designD)}, 10 * time.Microsecond, 20 * time.Microsecond, linux, 0},
		{"off", []DialOption{WithCoalesceDelay(0, 0)}, 0, 0, false, 0},
		{"negative", []DialOption{WithCoalesceDelay(-time.Second, -time.Second)}, 0, 0, false, 0},
		{"last-wins-off", []DialOption{WithCoalesceDelay(time.Millisecond, time.Millisecond), WithCoalesceDelay(0, 0)}, 0, 0, false, 0},
		{"last-wins-on", []DialOption{WithCoalesceDelay(0, 0), WithCoalesceDelay(3*time.Microsecond, 7*time.Microsecond)}, 3 * time.Microsecond, 7 * time.Microsecond, linux, 0},
		{"adaptive", []DialOption{WithAdaptiveLinger(20 * time.Microsecond)}, 0, 0, false, 20 * time.Microsecond},
		{"adaptive-negative", []DialOption{WithAdaptiveLinger(-time.Second)}, 0, 0, false, 0},
		{"adaptive-overrides-coalesce", []DialOption{WithCoalesceDelay(10*time.Microsecond, 20*time.Microsecond), WithAdaptiveLinger(25 * time.Microsecond)}, 0, 0, false, 25 * time.Microsecond},
		{"coalesce-overrides-adaptive", []DialOption{WithAdaptiveLinger(25 * time.Microsecond), WithCoalesceDelay(3*time.Microsecond, 7*time.Microsecond)}, 3 * time.Microsecond, 7 * time.Microsecond, linux, 0},
		{"coalesce-zero-overrides-adaptive", []DialOption{WithAdaptiveLinger(25 * time.Microsecond), WithCoalesceDelay(0, 0)}, 0, 0, false, 0},
		{"legacy-ignores-adaptive", []DialOption{WithLegacyWriter(), WithAdaptiveLinger(20 * time.Microsecond)}, 0, 0, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := dialWriteCountedRaw(t, tc.opts...)
			lo, hi, waits := c.CoalesceDelay()
			if lo != tc.min || hi != tc.max || waits != tc.waits {
				t.Fatalf("delay %v/%v waits=%t, want %v/%v waits=%t", lo, hi, waits, tc.min, tc.max, tc.waits)
			}
			if got := c.AdaptiveLinger(); got != tc.adaptive {
				t.Fatalf("AdaptiveLinger() = %v, want %v", got, tc.adaptive)
			}
			if c.na != nil {
				if c.na.delay != tc.waits || c.na.adaptive != tc.adaptive {
					t.Fatalf("naWriter delay=%t adaptive=%v, want delay=%t adaptive=%v", c.na.delay, c.na.adaptive, tc.waits, tc.adaptive)
				}
			}
		})
	}
}

func TestCoalesceDelayFormula(t *testing.T) {
	t.Parallel()
	const lo, hi = 10 * time.Microsecond, 20 * time.Microsecond
	for _, tc := range []struct {
		since  time.Duration
		jitter float64
		want   time.Duration
	}{
		{0, 0.5, 20 * time.Microsecond},                    // just wrote: the maximum
		{5 * time.Microsecond, 0.5, 15 * time.Microsecond}, // MAX - since
		{time.Hour, 0.5, 10 * time.Microsecond},            // long idle: the minimum
		{time.Hour, 0, 9 * time.Microsecond},               // jitter 0.9
		{0, 1, 22 * time.Microsecond},                      // jitter 1.1
	} {
		if got := coalesceDelay(lo, hi, tc.since, tc.jitter); got != tc.want {
			t.Errorf("coalesceDelay(since=%v, jitter=%v) = %v, want %v", tc.since, tc.jitter, got, tc.want)
		}
	}
}

// stampObserver records, at each socket write, the stamp the writer left.
type stampObserver struct {
	net.Conn
	c      *Conn
	stamps []time.Time
}

func (o *stampObserver) Write(p []byte) (int, error) {
	o.stamps = append(o.stamps, o.c.lastWrite)
	time.Sleep(time.Millisecond) // separate consecutive stamps
	return len(p), nil
}

// Every socket write under wbuf restamps lastWrite, including the writes of a
// batch that spills the buffer, not only its final flush
// (FlowTransport.actor.cpp:716-718).
func TestCoalesceStampsEverySocketWrite(t *testing.T) {
	t.Parallel()
	c := &Conn{}
	obs := &stampObserver{c: c}
	c.conn = obs
	c.wbuf = bufio.NewWriterSize(stampedWriter{c}, 16)
	before := time.Now()
	for range 3 {
		if _, err := c.wbuf.Write(bytes.Repeat([]byte{'x'}, 20)); err != nil { // larger than the buffer
			t.Fatal(err)
		}
	}
	if err := c.wbuf.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(obs.stamps) < 3 {
		t.Fatalf("precondition: %d socket writes, want a spilling batch", len(obs.stamps))
	}
	prev := before
	for i, s := range obs.stamps {
		if !s.After(prev) {
			t.Fatalf("COALESCE_STAMP: write %d saw stamp %v, not after the previous %v", i, s, prev)
		}
		prev = s
	}
}
