package client

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
	"fdb.dev/pkg/fdbgo/transport"
)

// Observe actual suspension notifications, including those after the one-shot
// signal fired. A live sentinel keeps NotifySuspend's cheap global gate open.
type sendParkContext struct {
	context.Context
	calls int
	on    func(int, *transport.SuspendSignal)
}

func (c *sendParkContext) Value(key any) any {
	value := c.Context.Value(key)
	if signal, ok := value.(*transport.SuspendSignal); ok {
		c.calls++
		c.on(c.calls, signal)
	}
	return value
}

func sendAwait(t *testing.T, ctx context.Context, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for " + what)
	}
}

func sendPipe(t *testing.T, ctx context.Context, handshake, reads <-chan struct{}) (net.Conn, <-chan string) {
	t.Helper()
	client, server := net.Pipe()
	frames := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(frames)
		defer server.Close()
		if _, err := transport.ReadConnectPacket(server); err != nil {
			return
		}
		if handshake != nil {
			select {
			case <-handshake:
			case <-ctx.Done():
				return
			}
		}
		packet := transport.ConnectPacket{ProtocolVersion: transport.ProtocolVersion73, ConnectionID: 262}
		if _, err := server.Write(packet.Marshal()); err != nil {
			return
		}
		if reads != nil {
			select {
			case <-reads:
			case <-ctx.Done():
				return
			}
		}
		var reader transport.FrameReader
		for {
			_, body, err := reader.Read(server, false)
			if err != nil {
				return
			}
			frames <- string(body)
		}
	}()
	t.Cleanup(func() { client.Close(); server.Close(); <-done })
	return client, frames
}

func sendDatabase(ctx context.Context, dial transport.DialFunc) *database {
	return &database{ctx: ctx, connPool: map[string]*transport.Conn{}, dialing: map[string]*dialCall{}, failMon: newFailureMonitor(), dialFn: dial}
}

func closeSendDatabase(db *database) {
	db.connMu.Lock()
	defer db.connMu.Unlock()
	for _, conn := range db.connPool {
		conn.Close()
	}
}

func TestColdPeerSendOrderThroughEnqueue(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, sentinel := transport.WithSuspendSignal(ctx)
	defer sentinel.Fire()
	handshake, reads, resumeBegin := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var handshakeOnce, readsOnce, resumeOnce sync.Once
	unblock := func() {
		handshakeOnce.Do(func() { close(handshake) })
		readsOnce.Do(func() { close(reads) })
		resumeOnce.Do(func() { close(resumeBegin) })
	}
	defer unblock()
	peer, frames := sendPipe(t, ctx, handshake, reads)
	other, otherFrames := sendPipe(t, ctx, nil, nil)
	var dials atomic.Int32
	db := sendDatabase(ctx, func(_ context.Context, _, addr string) (net.Conn, error) {
		if addr == "other" {
			return other, nil
		}
		dials.Add(1)
		return peer, nil
	})
	defer closeSendDatabase(db)
	beginCtx, beginSignal := transport.WithSuspendSignal(ctx)
	defer beginSignal.Fire()
	beginParked := make(chan struct{})
	beginCtx = &sendParkContext{Context: beginCtx, on: func(n int, signal *transport.SuspendSignal) {
		if n == 1 {
			signal.Fire()
			close(beginParked)
			<-resumeBegin
		}
	}}
	endCtx, endSignal := transport.WithSuspendSignal(ctx)
	defer endSignal.Fire()
	endAfterDial, endQueued := make(chan struct{}), make(chan struct{})
	endCtx = &sendParkContext{Context: endCtx, on: func(n int, _ *transport.SuspendSignal) {
		if n == 2 {
			close(endAfterDial)
		}
		if n == 3 {
			close(endQueued)
		}
	}}
	results := make(chan error, 3)
	send := func(ctx context.Context, body string) {
		conn, permit, err := db.getOrDialForSend(ctx, "peer")
		if err == nil {
			defer permit.Release()
			_, err = conn.SendFrameContextOrdered(ctx, transport.UID{First: 262}, []byte(body), permit)
		}
		results <- err
	}
	go send(beginCtx, "begin")
	sendAwait(t, ctx, beginParked, "begin's cold-dial suspension")
	go send(endCtx, "end")
	sendAwait(t, ctx, endSignal.Done(), "end's shared-dial suspension")
	db.connMu.Lock()
	call := db.dialing["peer"]
	registered := call != nil && call.sends.Pending()
	db.connMu.Unlock()
	if !registered {
		t.Error("positive control: cold send positions were not registered before suspension")
	}
	handshakeOnce.Do(func() { close(handshake) })
	sendAwait(t, ctx, endAfterDial, "end's post-dial wait")
	thirdCtx, thirdSignal := transport.WithSuspendSignal(ctx)
	defer thirdSignal.Fire()
	go send(thirdCtx, "third")
	sendAwait(t, ctx, thirdSignal.Done(), "new pooled-connection caller joining the cold order")

	// An unrelated peer must progress while the shared cold peer is held.
	independent, permit, err := db.getOrDialForSend(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := independent.SendFrameContextOrdered(ctx, transport.UID{First: 263}, []byte("independent"), permit); err != nil {
		t.Fatal(err)
	}
	if got := <-otherFrames; got != "independent" {
		t.Fatalf("other peer frame = %q", got)
	}
	resumeOnce.Do(func() { close(resumeBegin) })
	if registered && transport.NetDesign() == "" {
		// The peer still cannot read: end reaches its flush wait only if begin
		// releases the cold turn on enqueue, not after the blocked flush. Design D
		// has no flush wait (DESIGN.md T1); its order and results are checked below.
		sendAwait(t, ctx, endQueued, "end enqueue before begin's flush completes")
	}
	readsOnce.Do(func() { close(reads) })
	for _, want := range []string{"begin", "end", "third"} {
		select {
		case got := <-frames:
			if got != want {
				t.Errorf("same-peer wire order: got %q, want %q", got, want)
			}
		case <-ctx.Done():
			t.Fatal("missing same-peer wire frame")
		}
	}
	for range 3 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("shared peer dials = %d, want 1", got)
	}
	warm, warmPermit, err := db.getOrDialForSend(ctx, "peer")
	if err != nil || warm == nil || warmPermit != nil {
		t.Fatalf("drained cold generation did not become warm: conn=%v permit=%v err=%v", warm, warmPermit, err)
	}
	db.connMu.Lock()
	retained := db.dialing["peer"]
	db.connMu.Unlock()
	if retained != nil {
		t.Fatal("drained successful generation was not removed lazily")
	}
}

func TestColdPeerCanceledLastWaiterKeepsDial(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	peer, _ := sendPipe(t, ctx, release, nil)
	var dials atomic.Int32
	entered := make(chan struct{})
	db := sendDatabase(ctx, func(context.Context, string, string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			close(entered)
		}
		return peer, nil
	})
	defer closeSendDatabase(db)
	ownerCtx, ownerCancel := context.WithCancel(ctx)
	ownerCtx, ownerSignal := transport.WithSuspendSignal(ownerCtx)
	defer ownerSignal.Fire()
	ownerResult := make(chan error, 1)
	go func() { _, permit, err := db.getOrDialForSend(ownerCtx, "peer"); permit.Release(); ownerResult <- err }()
	sendAwait(t, ctx, ownerSignal.Done(), "owner's dial wait")
	sendAwait(t, ctx, entered, "the single shared dial starting")
	ownerCancel()
	if err := <-ownerResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled owner = %v", err)
	}
	db.connMu.Lock()
	original := db.dialing["peer"]
	retained := original != nil && !original.sends.Pending()
	db.connMu.Unlock()
	if !retained {
		t.Fatal("canceling the last sender discarded its running dial")
	}
	waitCtx, waitSignal := transport.WithSuspendSignal(ctx)
	defer waitSignal.Fire()
	result := make(chan error, 1)
	go func() { _, permit, err := db.getOrDialForSend(waitCtx, "peer"); permit.Release(); result <- err }()
	sendAwait(t, ctx, waitSignal.Done(), "new waiter joining the existing dial")
	db.connMu.Lock()
	same := db.dialing["peer"] == original
	db.connMu.Unlock()
	if !same || dials.Load() != 1 {
		t.Fatalf("new waiter replaced the running dial: same=%t dials=%d", same, dials.Load())
	}
	once.Do(func() { close(release) })
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestColdPeerFailedGenerationDoesNotTrapNewSenders(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	releaseDial, resume := make(chan struct{}), make(chan struct{})
	var dialOnce, resumeOnce sync.Once
	defer func() { dialOnce.Do(func() { close(releaseDial) }); resumeOnce.Do(func() { close(resume) }) }()
	var dials atomic.Int32
	firstFailure, secondFailure := errors.New("first generation failed"), errors.New("second generation failed")
	db := sendDatabase(ctx, func(context.Context, string, string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			<-releaseDial
			return nil, firstFailure
		}
		return nil, secondFailure
	})
	ownerCtx, signal := transport.WithSuspendSignal(ctx)
	defer signal.Fire()
	parked := make(chan struct{})
	ownerCtx = &sendParkContext{Context: ownerCtx, on: func(n int, signal *transport.SuspendSignal) {
		if n == 1 {
			signal.Fire()
			close(parked)
			<-resume
		}
	}}
	result := make(chan error, 1)
	go func() { _, permit, err := db.getOrDialForSend(ownerCtx, "peer"); permit.Release(); result <- err }()
	sendAwait(t, ctx, parked, "old generation's parked owner")
	db.connMu.Lock()
	old := db.dialing["peer"]
	db.connMu.Unlock()
	if old == nil || !old.sends.Pending() {
		t.Fatal("positive control: failed dial's old permit was not held")
	}
	dialOnce.Do(func() { close(releaseDial) })
	sendAwait(t, ctx, old.done, "failed dial completion")
	_, permit, err := db.getOrDialForSend(ctx, "peer")
	permit.Release()
	if !errors.Is(err, secondFailure) || dials.Load() != 2 {
		t.Fatalf("new caller joined failed generation: err=%v dials=%d", err, dials.Load())
	}
	resumeOnce.Do(func() { close(resume) })
	if err := <-result; !errors.Is(err, firstFailure) {
		t.Fatalf("old waiter migrated generations: %v", err)
	}
}

func TestColdPeerClosedGenerationReleasesWaiter(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handshake := make(chan struct{})
	var handshakeOnce sync.Once
	defer handshakeOnce.Do(func() { close(handshake) })
	firstPeer, _ := sendPipe(t, ctx, handshake, nil)
	secondPeer, _ := sendPipe(t, ctx, nil, nil)
	var dials atomic.Int32
	db := sendDatabase(ctx, func(context.Context, string, string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return firstPeer, nil
		}
		return secondPeer, nil
	})
	defer closeSendDatabase(db)
	ownerCtx, ownerSignal := transport.WithSuspendSignal(ctx)
	defer ownerSignal.Fire()
	var first *transport.Conn
	var held *sendqueue.Permit
	ownerResult := make(chan error, 1)
	go func() {
		var err error
		first, held, err = db.getOrDialForSend(ownerCtx, "peer")
		ownerResult <- err
	}()
	sendAwait(t, ctx, ownerSignal.Done(), "owner's cold dial wait")
	waitCtx, signal := transport.WithSuspendSignal(ctx)
	defer signal.Fire()
	result := make(chan error, 1)
	go func() { _, permit, err := db.getOrDialForSend(waitCtx, "peer"); permit.Release(); result <- err }()
	sendAwait(t, ctx, signal.Done(), "waiter joining the unfinished cold dial")
	handshakeOnce.Do(func() { close(handshake) })
	if err := <-ownerResult; err != nil || held == nil {
		t.Fatalf("first cold owner: permit=%v err=%v", held, err)
	}
	defer held.Release()
	first.Close()
	select {
	case err := <-result:
		var closed *transport.ConnClosedError
		code, coded := fdbErrorCode(err)
		if !errors.Is(err, transport.ErrConnClosed) || !errors.As(err, &closed) || !coded || code != ErrRequestMaybeDelivered {
			t.Fatalf("closed-generation waiter = %v, code=%d coded=%t", err, code, coded)
		}
	case <-ctx.Done():
		t.Fatal("connection close stranded a waiter behind the still-held owner")
	}
	second, replacement, err := db.getOrDialForSend(ctx, "peer")
	if err != nil || second == first || replacement == nil || dials.Load() != 2 {
		t.Fatalf("closed-generation replacement: same=%t permit=%v err=%v dials=%d", second == first, replacement, err, dials.Load())
	}
	defer replacement.Release()
	db.connMu.Lock()
	generation := db.dialing["peer"]
	db.connMu.Unlock()
	held.Release()
	db.connMu.Lock()
	retained := db.dialing["peer"] == generation
	db.connMu.Unlock()
	if generation == nil || !retained || !generation.sends.Pending() {
		t.Fatal("old owner's late release removed or drained the replacement generation")
	}
}

func TestDialPoolingPanicClosesUnpublishedConn(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	peer, frames := sendPipe(t, ctx, nil, nil)
	db := sendDatabase(ctx, func(context.Context, string, string) (net.Conn, error) { return peer, nil })
	db.connPool = nil // panic after Dial starts its loops, before pool publication
	call := &dialCall{done: make(chan struct{})}
	permit, _ := call.sends.Reserve()
	defer permit.Release()
	db.dialing["peer"] = call
	db.dialAndPool("peer", call)
	sendAwait(t, ctx, call.done, "pooling panic result publication")
	if call.err == nil || db.dialing["peer"] != nil {
		t.Fatalf("pooling panic was not rolled back: err=%v retained=%t", call.err, db.dialing["peer"] != nil)
	}
	if late, _, ok := call.sends.TryReserve(); ok {
		late.Release()
		t.Error("pooling panic left its cold generation open")
	}
	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("unpublished connection emitted a frame instead of closing")
		}
	case <-ctx.Done():
		t.Fatal("pooling panic leaked the successfully dialed connection")
	}
}

func TestFallbackReadyBeginStartsEndBeforeConsumingError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reads, startEnd := make(chan struct{}), make(chan struct{})
	var readsOnce, startOnce sync.Once
	defer func() { readsOnce.Do(func() { close(reads) }); startOnce.Do(func() { close(startEnd) }) }()
	peer, frames := sendPipe(t, ctx, nil, reads)
	db := sendDatabase(ctx, func(context.Context, string, string) (net.Conn, error) { return peer, nil })
	defer closeSendDatabase(db)
	conn, err := db.getOrDial(ctx, "peer")
	if err != nil {
		t.Fatal(err)
	}
	beforeStartupWait := make(chan struct{})
	ctx = context.WithValue(ctx, resolveKeyStartObserver{}, func() { close(beforeStartupWait) })
	beginError := errors.New("begin is already an error")
	tx := &Transaction{db: db}
	tx.resolveKeyFn = func(ctx context.Context, selector KeySelector, _ int64) ([]byte, error) {
		if string(selector.Key) == "begin" {
			return nil, beginError
		}
		select {
		case <-startEnd:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		_, err := conn.SendFrameContext(ctx, transport.UID{First: 262}, []byte("end-started"))
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		_, err := tx.nativeGetRangeFallback(ctx, sel("begin", false, 2), sel("end", false, 2), newRangeLimits(1, -1), 1, false)
		done <- err
	}()
	select {
	case <-beforeStartupWait:
		// A positive event from the unready startup branch, not a timed
		// assertion that the parent probably had time to consume begin.
	case err := <-done:
		t.Fatalf("fallback consumed ready begin before starting end: %v", err)
	case <-ctx.Done():
		t.Fatal("fallback neither waited for end startup nor completed")
	}
	startOnce.Do(func() { close(startEnd) })
	select {
	case err := <-done:
		if !errors.Is(err, beginError) {
			t.Fatalf("fallback = %v, want ready begin error", err)
		}
	case <-ctx.Done():
		t.Fatal("fallback did not join end after its first transport wait")
	}
	readsOnce.Do(func() { close(reads) })
	select {
	case got := <-frames:
		if got != "end-started" {
			t.Fatalf("end startup wire frame = %q", got)
		}
	case <-ctx.Done():
		t.Fatal("end never queued its request before begin's ready error canceled it")
	}
}

func TestFallbackReadyEndDoesNotWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var observations atomic.Int32
	ctx = context.WithValue(ctx, resolveKeyStartObserver{}, func() { observations.Add(1) })
	failure := errors.New("begin error")
	tx := &Transaction{}
	tx.resolveKeyFn = func(context.Context, KeySelector, int64) ([]byte, error) { return nil, failure }
	_, err := tx.nativeGetRangeFallback(ctx, sel("begin", false, 2), firstGreaterOrEqual([]byte("end")), newRangeLimits(1, -1), 1, false)
	if !errors.Is(err, failure) || observations.Load() != 0 {
		t.Fatalf("ready end startup: err=%v wait observations=%d", err, observations.Load())
	}
}

func TestFallbackStartupCancellationAndEndPanic(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cancel", "end-panic"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			entered, resumed := make(chan struct{}), make(chan struct{})
			tx := &Transaction{}
			tx.resolveKeyFn = func(ctx context.Context, selector KeySelector, _ int64) ([]byte, error) {
				if string(selector.Key) == "begin" {
					notifyParked(ctx)
					close(resumed)
					return []byte("b"), ctx.Err()
				}
				close(entered)
				if mode == "end-panic" {
					panic("end startup panic")
				}
				// Hold the test's pre-start gate, not a production network wait.
				// Cancellation must release begin even before end can announce one.
				<-ctx.Done()
				return nil, ctx.Err()
			}
			done := make(chan error, 1)
			go func() {
				_, err := tx.nativeGetRangeFallback(ctx, sel("begin", false, 2), sel("end", false, 2), newRangeLimits(1, -1), 1, false)
				done <- err
			}()
			sendAwait(t, ctx, entered, "end entering startup")
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled startup = %v", err)
				}
				if mode == "end-panic" {
					if code, _ := fdbErrorCode(err); code != ErrInternalError {
						t.Fatalf("end panic = %v, want internal_error", err)
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("startup cancellation/panic stranded a resolver")
			}
			select {
			case <-resumed:
			default:
				t.Fatal("begin was not resumed and joined after startup teardown")
			}
		})
	}
}

func TestSleepHandoffStartsTimerBeforeSuspension(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resume := make(chan struct{})
		ctx, signal := transport.WithSuspendHandoff(context.Background(), resume, nil)
		defer signal.Fire()
		done := make(chan error, 1)
		go func() { done <- sleepCtx(ctx, time.Second) }()
		<-signal.Done()
		time.Sleep(2 * time.Second)
		close(resume)
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Error("sleep timer started after the startup handoff instead of before its wait")
		}
	})
}

func TestWarmPeerSendAdmissionAllocs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	peer, _ := sendPipe(t, ctx, nil, nil)
	db := sendDatabase(ctx, func(context.Context, string, string) (net.Conn, error) { return peer, nil })
	defer closeSendDatabase(db)
	conn, permit, err := db.getOrDialForSend(ctx, "peer")
	permit.Release()
	if err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(1000, func() {
		warm, permit, err := db.getOrDialForSend(ctx, "peer")
		if err != nil || warm != conn || permit != nil {
			panic("warm acquisition changed its connection or reserved a cold place")
		}
	}); n != 0 {
		t.Fatalf("warm peer send admission allocs = %g, want 0", n)
	}
}
