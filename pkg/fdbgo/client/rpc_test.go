package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

func TestGetPutTimer_Recycle(t *testing.T) {
	t.Parallel()
	// Exercise getTimer/putTimer to cover the pool recycle path.
	// First call creates a timer via pool.New, second reuses it.
	t1 := getTimer(time.Hour)
	putTimer(t1)
	t2 := getTimer(time.Hour)
	putTimer(t2)
}

func TestWaitReply_Success(t *testing.T) {
	t.Parallel()
	ch := make(chan transport.Response, 1)
	ch <- transport.Response{Body: []byte("ok")}

	resp, err := waitReply(ch, context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if string(resp.Body) != "ok" {
		t.Fatalf("expected body 'ok', got %q", resp.Body)
	}
}

func TestWaitReply_Timeout(t *testing.T) {
	t.Parallel()
	ch := make(chan transport.Response) // never sends

	// The internal RPC reply timeout is the errReplyTimeout sentinel, NOT a
	// caller deadline: the read paths re-send on it (libfdb_c has no per-read
	// client timeout) and it must never be confused with ctx cancellation.
	_, err := waitReply(ch, context.Background(), 10*time.Millisecond)
	if !isReplyTimeout(err) {
		t.Fatalf("expected errReplyTimeout, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reply timeout must not be a context.DeadlineExceeded (it is not a caller deadline)")
	}
}

func TestWaitReply_ContextCancelled(t *testing.T) {
	t.Parallel()
	ch := make(chan transport.Response) // never sends
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := waitReply(ch, ctx, 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Canceled, got %v", err)
	}
}

func TestWaitReplyOrProxiesChanged_NormalReply(t *testing.T) {
	t.Parallel()
	ch := make(chan transport.Response, 1)
	ch <- transport.Response{Body: []byte("committed")}
	proxiesChanged := make(chan struct{}) // never fires

	resp, err := waitReplyOrProxiesChanged(ch, context.Background(), 5*time.Second, proxiesChanged)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if string(resp.Body) != "committed" {
		t.Fatalf("expected body 'committed', got %q", resp.Body)
	}
}

func TestWaitReplyOrProxiesChanged_ProxiesChange(t *testing.T) {
	t.Parallel()
	ch := make(chan transport.Response) // never sends
	proxiesChanged := make(chan struct{})
	close(proxiesChanged) // simulate topology change

	start := time.Now()
	_, err := waitReplyOrProxiesChanged(ch, context.Background(), 5*time.Second, proxiesChanged)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded on proxy change, got %v", err)
	}
	// Should return immediately, not wait for the 5s timeout.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("proxy change detection too slow: %v (expected <100ms)", elapsed)
	}
}

func TestWaitReplyOrProxiesChanged_ProxiesChangeMidWait(t *testing.T) {
	t.Parallel()
	ch := make(chan transport.Response) // never sends
	proxiesChanged := make(chan struct{})

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(proxiesChanged) // topology changes after 50ms
	}()

	start := time.Now()
	_, err := waitReplyOrProxiesChanged(ch, context.Background(), 5*time.Second, proxiesChanged)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	// Should wake within ~50ms + scheduling jitter, not 5s timeout.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("mid-wait proxy change detection too slow: %v (expected <200ms)", elapsed)
	}
}

func TestWaitReplyOrProxiesChanged_Timeout(t *testing.T) {
	t.Parallel()
	ch := make(chan transport.Response)   // never sends
	proxiesChanged := make(chan struct{}) // never fires

	_, err := waitReplyOrProxiesChanged(ch, context.Background(), 10*time.Millisecond, proxiesChanged)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded on timeout, got %v", err)
	}
}

func TestWaitReplyOrProxiesChanged_ContextCancelled(t *testing.T) {
	t.Parallel()
	ch := make(chan transport.Response)   // never sends
	proxiesChanged := make(chan struct{}) // never fires
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := waitReplyOrProxiesChanged(ch, ctx, 5*time.Second, proxiesChanged)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Canceled, got %v", err)
	}
}

func TestWaitReplyOrProxiesChanged_ReplyBeforeProxiesChange(t *testing.T) {
	t.Parallel()
	ch := make(chan transport.Response, 1)
	proxiesChanged := make(chan struct{})

	// Reply arrives first.
	ch <- transport.Response{Body: []byte("ok")}
	// Topology changes later (but reply already consumed).
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(proxiesChanged)
	}()

	resp, err := waitReplyOrProxiesChanged(ch, context.Background(), 5*time.Second, proxiesChanged)
	if err != nil {
		t.Fatalf("reply should win over proxy change, got %v", err)
	}
	if string(resp.Body) != "ok" {
		t.Fatalf("expected 'ok', got %q", resp.Body)
	}
}
