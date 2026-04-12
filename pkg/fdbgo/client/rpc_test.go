package client

import (
	"context"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
)

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

	_, err := waitReply(ch, context.Background(), 10*time.Millisecond)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func TestWaitReply_ContextCancelled(t *testing.T) {
	t.Parallel()
	ch := make(chan transport.Response) // never sends
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := waitReply(ch, ctx, 5*time.Second)
	if err != context.Canceled {
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

	if err != context.DeadlineExceeded {
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

	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	// Should wake within ~50ms + scheduling jitter, not 5s timeout.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("mid-wait proxy change detection too slow: %v (expected <200ms)", elapsed)
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
