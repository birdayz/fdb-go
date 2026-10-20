package client

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
	"fdb.dev/pkg/fdbgo/transport"
)

func TestColdPeerGenerationIsBoundedUnderLoad(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const senders, passes = 64, 8
	handshake, releaseHead := make(chan struct{}), make(chan struct{})
	var handshakeOnce, headOnce sync.Once
	unblock := func() {
		handshakeOnce.Do(func() { close(handshake) })
		headOnce.Do(func() { close(releaseHead) })
	}
	defer unblock()
	peer, frames := sendPipe(t, ctx, handshake, nil)
	var dials, warmPasses atomic.Int32
	db := sendDatabase(ctx, func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return peer, nil
	})
	defer closeSendDatabase(db)
	firstOwner := make(chan struct{})
	results := make(chan error, 2*senders)
	for sender := 0; sender < senders; sender++ {
		firstCtx, signal := transport.WithSuspendSignal(ctx)
		t.Cleanup(signal.Fire)
		go func(sender int) {
			for pass := 0; pass < passes; pass++ {
				requestCtx := ctx
				if pass == 0 {
					requestCtx = firstCtx
				}
				conn, permit, err := db.getOrDialForSend(requestCtx, "peer")
				if err != nil {
					results <- err
					return
				}
				if pass == 0 && permit == nil {
					results <- fmt.Errorf("pre-dial sender %d had no prefix permit", sender)
					return
				}
				if pass != 0 {
					if permit != nil {
						permit.Release()
						results <- fmt.Errorf("post-dial sender %d/%d extended the cold generation", sender, pass)
						return
					}
					warmPasses.Add(1)
				}
				if sender == 0 && pass == 0 {
					close(firstOwner)
					select {
					case <-releaseHead:
					case <-ctx.Done():
						permit.Release()
						results <- ctx.Err()
						return
					}
				}
				_, err = conn.SendFrameContextOrdered(requestCtx, transport.UID{First: uint64(sender + 1)}, []byte(fmt.Sprintf("%02d/%d", sender, pass)), permit)
				permit.Release()
				if err != nil {
					results <- err
					return
				}
			}
			results <- nil
		}(sender)
		// Establish a literal initial-prefix order while all senders coexist on
		// the held dial; later rounds remain concurrent and keep the peer busy.
		sendAwait(t, ctx, signal.Done(), "pre-dial sender reservation")
	}
	db.connMu.Lock()
	generation := db.dialing["peer"]
	db.connMu.Unlock()
	if generation == nil || !generation.sends.Pending() {
		t.Fatal("positive control: no live cold prefix")
	}
	handshakeOnce.Do(func() { close(handshake) })
	sendAwait(t, ctx, firstOwner, "head acquiring the completed dial")
	sendAwait(t, ctx, generation.done, "dial completion publication")
	if permit, _, accepted := generation.sends.TryReserve(); accepted {
		permit.Release()
		t.Error("completed dial generation was not sealed while its head was held")
	}
	select {
	case <-generation.sends.Drained():
		t.Fatal("held prefix already reported drainage")
	default:
	}
	// Later lookups must return without joining the held prefix. Actual frames
	// acquire outbox positions before their transport waits, not another permit.
	lookups := make(chan *sendqueue.Permit, senders)
	for sender := 0; sender < senders; sender++ {
		go func(sender int) {
			conn, permit, err := db.getOrDialForSend(ctx, "peer")
			lookups <- permit
			if err == nil {
				_, err = conn.SendFrameContextOrdered(ctx, transport.UID{First: uint64(1000 + sender)}, []byte(fmt.Sprintf("late-%02d", sender)), permit)
			}
			permit.Release()
			results <- err
		}(sender)
	}
	for range senders {
		select {
		case permit := <-lookups:
			if permit != nil {
				t.Error("a post-completion lookup allocated a cold permit")
			}
		case <-ctx.Done():
			t.Fatal("post-dial lookups waited for the held cold prefix")
		}
	}
	headOnce.Do(func() { close(releaseHead) })
	const total = senders*passes + senders
	for frame := 0; frame < total; frame++ {
		select {
		case body := <-frames:
			if frame < senders && body != fmt.Sprintf("%02d/0", frame) {
				t.Errorf("prefix wire frame %d = %q, want %02d/0 before every later packet", frame, body, frame)
			}
		case <-ctx.Done():
			t.Fatalf("peer received only %d/%d frames", frame, total)
		}
	}
	for range 2 * senders {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	sendAwait(t, ctx, generation.sends.Drained(), "sealed cohort drainage under continued load")
	if generation.sends.Pending() || warmPasses.Load() != senders*(passes-1) || dials.Load() != 1 {
		t.Fatalf("generation after load: pending=%t warm=%d dials=%d", generation.sends.Pending(), warmPasses.Load(), dials.Load())
	}
	conn, permit, err := db.getOrDialForSend(ctx, "peer")
	if err != nil || conn == nil || permit != nil {
		t.Fatalf("warm lookup after load = %v, permit=%v err=%v", conn, permit, err)
	}
	db.connMu.Lock()
	retained := db.dialing["peer"]
	db.connMu.Unlock()
	if retained != nil {
		t.Fatal("drained generation was not detached on the warm lookup")
	}
}
