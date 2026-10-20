package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func TestFallbackContinuationWaitsForEndStartup(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"unready-reply", "ready-storage-reply", "ready-location-reply", "ready-storage-with-control"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			const attempts = 5
			for attempt := 1; attempt <= attempts; attempt++ {
				var confounds []string
				forced := mode == "ready-storage-with-control" && attempt == 1
				if !t.Run(fmt.Sprintf("attempt-%d", attempt), func(t *testing.T) {
					confounds = checkFallbackContinuationHandoff(t, mode, forced)
				}) {
					return // semantic failures are never retried
				}
				if len(confounds) == 0 {
					if forced {
						t.Fatal("forced control write did not exercise the flush-confound retry")
					}
					t.Logf("attempt %d passed the strict handoff assertions", attempt)
					return
				}
				t.Logf("attempt %d discarded: preceding non-storage writes on begin's connection: %v", attempt, confounds)
			}
			t.Fatalf("control writes confounded all %d attempts", attempts)
		})
	}
}

func checkFallbackContinuationHandoff(t *testing.T, mode string, forced bool) []string {
	t.Helper()
	ready, locationReady := mode != "unready-reply", mode == "ready-location-reply"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	prefix := t.Name() + "/"
	sim, draws, rec := newSimDialer(), &readTagDraws{}, &sendRecorder{}
	dial := rec.wrap(sim.dial)
	var frontends sync.Map
	var control *handoffControlGate
	if forced {
		control = &handoffControlGate{entered: make(chan struct{}), release: make(chan struct{})}
		defer control.resume()
	}
	if ready {
		dial = handoffTCPDialer(t, dial, &frontends, control)
	}
	db, err := OpenDatabaseFromConfig(ctx, sharedClusterFile, WithDialFunc(dial), WithAPIVersion(730), WithCoalesceDelay(0, 0), withReadTagSampling(0.5, draws.draw))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, suffix := range []string{"a", "b", "c", "d", "e"} {
		setKey(t, ctx, db, []byte(prefix+suffix), []byte("v"))
	}
	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.db.hedgeEnabled.Store(false)
	if err := tx.SetAutoThrottleTag("rt"); err != nil {
		t.Fatal(err)
	}
	rv, err := tx.GetReadVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.db.locCache.locateRange(tx.db, ctx, []byte(prefix), []byte(prefix+"\xff"), 100, false, NoTenantID, types.SpanContext{}); err != nil {
		t.Fatal(err)
	}
	location, err := tx.db.locCache.locate(tx.db, ctx, []byte(prefix+"a"), NoTenantID, types.SpanContext{}, false)
	if err != nil || len(location.Servers) == 0 {
		t.Fatalf("cached begin location = %+v, %v", location, err)
	}
	server := location.Servers[0]
	setServers(db, []ServerInfo{server})
	conn, err := tx.db.getOrDial(ctx, server.Address)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		frontend, ok := frontends.Load(server.Address)
		if !ok {
			t.Fatal("storage connection has no bare TCP frontend")
		}
		if _, err := frontend.(*net.TCPConn).SyscallConn(); err != nil {
			t.Fatalf("storage TCP frontend has no raw capability: %v", err)
		}
		if min, max, waits := conn.CoalesceDelay(); min != 0 || max != 0 || waits {
			t.Fatalf("storage connection coalesces: %v/%v/%t", min, max, waits)
		}
	}
	if locationReady {
		for _, proxy := range tx.db.getCommitProxies() {
			if _, err := tx.db.getOrDial(ctx, proxy.Address); err != nil {
				t.Fatal(err)
			}
		}
		tx.db.locCache.invalidate([]byte(prefix+"a"), NoTenantID, false)
	}
	partialSeen, releasePartial, continued := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce, continuedOnce sync.Once
	unblockPartial := func() { releaseOnce.Do(func() { close(releasePartial) }) }
	defer unblockPartial()
	var injected, locationInjected atomic.Bool
	var beginToken transport.UID
	sim.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
		kind := rec.replyKind(token)
		if locationReady && kind == "GetKeyServerLocationsRequest" && locationInjected.CompareAndSwap(false, true) {
			beginToken = token
			close(partialSeen)
			select {
			case <-releasePartial:
			case <-ctx.Done():
			}
			return body, false
		}
		if kind != "GetKeyRequest" {
			return body, false
		}
		beginReply := false
		for _, message := range rec.messages() {
			if request, ok := message.(*types.GetKeyRequest); ok && string(request.Sel.Key) == prefix+"a" {
				beginReply = wire.UIDFromParts(token.First, token.Second) == request.Reply.Token
			}
		}
		if !beginReply || !injected.CompareAndSwap(false, true) {
			// End is held before its first request. A second real reply while
			// that gate is closed positively proves begin2 reached the peer.
			continuedOnce.Do(func() { close(continued) })
			return body, false
		}
		if !locationReady {
			beginToken = token
		}
		key, orEqual, offset, _, err := parseGetKeyReply(body)
		if err != nil || string(key) != prefix+"b" || !orEqual || offset != 0 {
			t.Errorf("first real getKey reply = %q/%t/%d, %v; want complete b", key, orEqual, offset, err)
		}
		partial := bytes.Clone(body)
		var reader wire.Reader
		if err := wire.ReadErrorOrInto(partial, &reader); err != nil {
			t.Error(err)
			return body, false
		}
		selector, err := reader.ReadNestedReader(types.GetKeyReplySlotSel)
		if err != nil {
			t.Error(err)
			return body, false
		}
		equalAt := selector.FieldOffset(types.KeySelectorRefSlotOrEqual)
		offsetAt := selector.FieldOffset(types.KeySelectorRefSlotOffset)
		object := selector.ObjectBytes()
		if equalAt == 0 || offsetAt == 0 || equalAt >= len(object) || offsetAt+4 > len(object) {
			t.Error("real selector reply omitted fields needed for a partial FGE(b)")
			return body, false
		}
		object[equalAt] = 0
		binary.LittleEndian.PutUint32(object[offsetAt:], 1)
		if !locationReady {
			close(partialSeen)
			select {
			case <-releasePartial:
			case <-ctx.Done():
			}
		}
		return partial, false
	})
	defer sim.setIntercept(nil)
	endEntered, releaseEnd := make(chan struct{}), make(chan struct{})
	var endOnce sync.Once
	unblockEnd := func() { endOnce.Do(func() { close(releaseEnd) }) }
	defer unblockEnd()
	beginAtGate, firstFlush := make(chan struct{}), make(chan bool, 1)
	var gateOnce sync.Once
	tx.beforeResolveKeyResume = func(selector KeySelector) {
		if string(selector.Key) == prefix+"a" {
			gateOnce.Do(func() {
				firstFlush <- handoffFlushWait()
				close(beginAtGate)
				if control != nil {
					control.resume()
				}
			})
		}
	}
	beforeNotify, releaseNotify := make(chan (<-chan struct{}), 1), make(chan struct{})
	unblockNotify := sync.OnceFunc(func() { close(releaseNotify) })
	defer unblockNotify()
	if ready {
		var once sync.Once
		tx.beforeResolveKeyReplyWait = func(selector KeySelector, parked <-chan struct{}) {
			if string(selector.Key) == prefix+"a" {
				once.Do(func() {
					beforeNotify <- parked
					select {
					case <-releaseNotify:
					case <-ctx.Done():
					}
				})
			}
		}
	}
	var pingCleanup replyGuard
	defer pingCleanup.close()
	var pingReply <-chan transport.Response
	tx.resolveKeyWorkerObserver = func(selector KeySelector) {
		if forced && string(selector.Key) == prefix+"a" {
			control.addr = server.Address
			control.active.Store(true)
			endpoint := transport.WellKnownToken(1)
			token, replies, handle := conn.PrepareReplyTo(endpoint)
			pingReply, pingCleanup.handle = replies, handle
			// Padding exceeds the tiny frontend buffers, so this real PING keeps
			// the writer busy until begin positively reports its flush wait.
			body := append(transport.BuildPingRequest(token), make([]byte, 1<<20)...)
			if accepted, err := conn.SendFrameDeferredContext(ctx, endpoint, body); !accepted || err != nil {
				t.Errorf("forced PING enqueue = %t, %v", accepted, err)
				return
			}
			select {
			case <-control.entered:
			case <-ctx.Done():
				t.Error("forced control writer never reached the TCP frontend gate")
				return
			}
		}
		if string(selector.Key) == prefix+"d" {
			close(endEntered)
			select {
			case <-releaseEnd:
			case <-ctx.Done():
			}
		}
	}
	sim.armEverything()
	draws.script(drawHit, drawMiss, drawHit, drawMiss)
	rec.start()
	done := make(chan struct{})
	var result nativeRangeResult
	var readErr error
	go func() {
		defer close(done)
		result, readErr = tx.nativeGetRangeFallback(ctx, KeySelector{Key: []byte(prefix + "a"), Offset: 2}, KeySelector{Key: []byte(prefix + "d"), Offset: 2}, newRangeLimits(0, -1), rv, false)
	}()
	defer func() {
		cancel()
		if control != nil {
			control.resume()
		}
		unblockNotify()
		unblockPartial()
		unblockEnd()
		<-done
	}()
	var beginParked <-chan struct{}
	earlyFlush := false
	if ready {
		select {
		case beginParked = <-beforeNotify:
		case <-endEntered:
			select {
			case flush := <-firstFlush:
				if !flush {
					t.Fatal("begin suspended before its controlled post-send reply boundary")
				}
			case <-ctx.Done():
				t.Fatal("begin's early suspension was not observed")
			}
			earlyFlush = true
		case <-ctx.Done():
			t.Fatal("begin never reached its post-send reply boundary")
		}
	} else {
		handoffAwait(t, ctx, endEntered, "end resolver entering its startup")
	}
	handoffAwait(t, ctx, partialSeen, "real first reply held before publication")
	if locationReady {
		tx.db.connMu.Lock()
		for _, candidate := range tx.db.connPool {
			if candidate.HasPendingReply(beginToken) {
				conn = candidate
				break
			}
		}
		tx.db.connMu.Unlock()
	}
	if ready && (ctx.Err() != nil || conn.IsClosed()) {
		t.Fatal("reply publication control lost its live context or connection")
	}
	if !conn.HasPendingReply(beginToken) {
		t.Fatal("positive control: held begin reply has no registration")
	}
	if earlyFlush {
		// The accepted begin frame reached the peer. Its recorder has therefore
		// parsed every earlier frame on that connection, even behind the bridge.
		confounds, err := rec.priorNonStorageWrites(beginToken)
		if err != nil {
			t.Fatal(err)
		}
		if len(confounds) == 0 {
			t.Fatal("begin suspended before its controlled post-send reply boundary")
		}
		if forced {
			pingSeen := false
			for _, kind := range confounds {
				pingSeen = pingSeen || kind == "PING"
			}
			if !pingSeen {
				t.Fatalf("forced PING absent from preceding writes: %v", confounds)
			}
			unblockPartial()
			select {
			case response := <-pingReply:
				value, decodeErr := wire.ReadErrorOr(response.Body)
				if response.Err != nil || decodeErr != nil || value.FileIdentifier() != types.VoidReplyFileID || value.VTableLength() != 2 {
					t.Fatalf("forced control reply = %x, %v/%v; want a real empty VoidReply value", response.Body, response.Err, decodeErr)
				}
				pingCleanup.release()
			case <-ctx.Done():
				t.Fatal("forced PING was not answered by the real peer")
			}
		}
		// Validate every recorded frame, including PING request structure and
		// padding, even off the target connection or after its frame. Only the
		// forced fixture also checks a real PING response; ordinary attempts
		// rely on validated request bytes, not an observed control reply.
		_ = taggedRequests(t, rec.stop())
		if ctx.Err() != nil || conn.IsClosed() {
			t.Fatal("control write coincided with cancellation or connection failure")
		}
		return confounds
	}
	unblockPartial()
	for conn.HasPendingReply(beginToken) {
		if ctx.Err() != nil {
			t.Fatal("begin's first reply was never published")
		}
		runtime.Gosched()
	}
	if ready {
		if ctx.Err() != nil || conn.IsClosed() {
			t.Fatal("reply publication control lost its live context or connection")
		}
		select {
		case <-beginParked:
			t.Fatal("begin suspended before its already-published reply boundary")
		default:
		}
		unblockNotify()
		handoffAwait(t, ctx, endEntered, "end resolver entering after begin's ready reply")
	}
	// Observe the actual unready resume gate, not a timed absence of begin2.
	// Without the handoff, begin instead receives a second real FDB reply
	// while end remains held. Transport channels cannot belong to a synctest
	// bubble because outside connection loops also use them.
	select {
	case <-beginAtGate:
	case <-continued:
		t.Error("begin2 reached the real peer before end startup")
	case <-ctx.Done():
		t.Fatal("begin neither entered its handoff nor received its continuation")
	}
	var beforeEnd []string
	for _, message := range rec.messages() {
		if request, ok := message.(*types.GetKeyRequest); ok {
			beforeEnd = append(beforeEnd, string(request.Sel.Key))
		}
	}
	if locationReady {
		if len(beforeEnd) != 0 || nrCount(rec.messages(), "GetKeyServerLocationsRequest") != 1 {
			t.Errorf("begin continued before end startup: %s; want only begin's location request", nrDescribe(rec.messages()))
		}
	} else if len(beforeEnd) != 1 || beforeEnd[0] != prefix+"a" {
		t.Errorf("begin continued before end startup: getKey frames %q, want only begin1", beforeEnd)
	}
	unblockEnd()
	handoffAwait(t, ctx, done, "both selector resolutions and exact range")
	if readErr != nil || len(result.kvs) != 3 || string(result.kvs[0].Key) != prefix+"b" || string(result.kvs[2].Key) != prefix+"d" {
		t.Fatalf("fallback result = %+v, %v; want b,c,d", result.kvs, readErr)
	}
	got := rec.stop()
	if err := nrProblems(got); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, message := range got.msgs {
		if request, ok := message.(*types.GetKeyRequest); ok {
			keys = append(keys, string(request.Sel.Key))
		}
	}
	want := []string{prefix + "a", prefix + "d", prefix + "b"}
	if locationReady {
		// Both location waits precede storage sends; their replies may resume
		// either worker first. Begin's continuation must still follow begin1.
		positions := make(map[string]int)
		for i, key := range keys {
			positions[key] = i
		}
		_, begin := positions[want[0]]
		_, end := positions[want[1]]
		_, continuation := positions[want[2]]
		if len(keys) != 3 || len(positions) != 3 || !begin || !end || !continuation || positions[want[0]] >= positions[want[2]] {
			t.Errorf("location-first getKey requests = %q, want each of %q with begin1 before begin2", keys, want)
		}
	} else if len(keys) != 3 || keys[0] != want[0] || keys[1] != want[1] || keys[2] != want[2] {
		t.Errorf("wire order = %q, want begin1,end1,begin2 = %q", keys, want)
	}
	requests := taggedRequests(t, got)
	wantKinds := []string{"GetKeyRequest", "GetKeyRequest", "GetKeyRequest", "GetKeyValuesRequest"}
	if len(requests) != len(wantKinds) {
		t.Fatalf("physical read requests = %v, want exactly three getKeys and one range", requests)
	}
	for i := range requests {
		if requests[i].kind != wantKinds[i] {
			t.Errorf("request %d kind = %q, want %q", i, requests[i].kind, wantKinds[i])
		}
		requests[i].kind = ""
	}
	tags := string(encodeTagSet([]string{"rt"}))
	if locationReady {
		hits := 0
		for _, request := range requests {
			if request.has {
				hits++
				if request.tags != tags {
					t.Errorf("sampled request tags = %q, want %q", request.tags, tags)
				}
			} else if request.tags != "" {
				t.Errorf("unsampled request retained tags %q", request.tags)
			}
		}
		if draws.count() != 4 || hits != 2 {
			t.Errorf("location-first sampling = %d draws/%d sampled requests, want 4/2", draws.count(), hits)
		}
	} else {
		checkNewRequests(t, requests, draws.count(), 4, taggedRequest{has: true, tags: tags})
	}
	return nil
}

// handoffFlushWait observes the accepted-frame wait, not dial, FIFO, or reply
// suspension. Only that wait can be confounded by an earlier background write.
func handoffFlushWait() bool {
	var pcs [32]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.Function == "fdb.dev/pkg/fdbgo/transport.NotifySuspend" {
			caller, _ := frames.Next()
			return caller.Function == "fdb.dev/pkg/fdbgo/transport.(*Conn).writeAdmittedContext"
		}
		if !more {
			return false
		}
	}
}

func TestHandoffFlushWaitRejectsUnrelatedStack(t *testing.T) {
	t.Parallel()
	if handoffFlushWait() {
		t.Fatal("ordinary test stack was classified as an accepted-frame flush wait")
	}
}

func TestHandoffFlushWaitRejectsFIFO(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	client, peer := net.Pipe()
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		defer peer.Close()
		if _, err := transport.ReadConnectPacket(peer); err != nil {
			return
		}
		packet := transport.ConnectPacket{ProtocolVersion: transport.ProtocolVersion73, ConnectionID: 969}
		if _, err := peer.Write(packet.Marshal()); err != nil {
			return
		}
		<-ctx.Done() // no socket reads: the real writer cannot drain its full queue
	}()
	defer func() { cancel(); client.Close(); peer.Close(); <-peerDone }()
	conn, err := transport.Dial(ctx, "fifo-observer", nil, func(context.Context, string, string) (net.Conn, error) { return client, nil }, transport.WithCoalesceDelay(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	resumeCh := make(chan struct{})
	resume := sync.OnceFunc(func() { close(resumeCh) })
	fillCtx, fillSignal := transport.WithSuspendHandoff(ctx, resumeCh, nil)
	defer fillSignal.Fire()
	observed := make(chan bool, 1)
	headCtx, headSignal := transport.WithSuspendHandoff(ctx, resumeCh, func() { observed <- handoffFlushWait() })
	defer headSignal.Fire()
	var workers sync.WaitGroup
	defer func() { cancel(); resume(); conn.Close(); workers.Wait() }()
	workers.Add(1)
	go func() {
		defer workers.Done()
		body := bytes.Repeat([]byte("f"), 64<<10)
		for range 1024 {
			if _, err := conn.SendFrameDeferredContext(fillCtx, transport.UID{First: 1}, body); err != nil {
				return
			}
		}
	}()
	handoffAwait(t, ctx, fillSignal.Done(), "full queue retaining its first admission owner")
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, _ = conn.SendFrameContext(headCtx, transport.UID{First: 2}, []byte("FIFO follower"))
	}()
	select {
	case flush := <-observed:
		if flush {
			t.Fatal("FIFO suspension beneath writeAdmittedContext was classified as an accepted-frame flush")
		}
	case <-ctx.Done():
		t.Fatal("full-queue SendFrameContext never reported its FIFO suspension")
	}
}

type handoffControlGate struct {
	addr        string // published by active before any intercepted write
	active      atomic.Bool
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

func (g *handoffControlGate) resume() { g.releaseOnce.Do(func() { close(g.release) }) }

type handoffControlWriter struct {
	io.Writer
	frontend, peer *net.TCPConn
	addr           string
	gate           *handoffControlGate
}

func (w *handoffControlWriter) Write(body []byte) (int, error) {
	var restoreErr error
	if w.gate.active.Load() && w.addr == w.gate.addr {
		w.gate.once.Do(func() {
			close(w.gate.entered)
			<-w.gate.release
			// Restore throughput once begin has positively entered its flush wait.
			if restoreErr = w.peer.SetReadBuffer(1 << 20); restoreErr == nil {
				restoreErr = w.frontend.SetWriteBuffer(1 << 20)
			}
		})
	}
	if restoreErr != nil {
		return 0, restoreErr
	}
	return w.Writer.Write(body)
}

// The bridge returns a bare TCP socket, so the real sender can finish without a
// flush suspension. Recording and reply rewriting stay behind that socket.
func handoffTCPDialer(t *testing.T, dial transport.DialFunc, frontends *sync.Map, control *handoffControlGate) transport.DialFunc {
	t.Helper()
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		backend, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		recorded := backend.(*recordingConn)
		simulated := recorded.Conn.(*simConn)
		closeBackend := func() { simulated.pr.Close(); backend.Close() }
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			closeBackend()
			return nil, err
		}
		frontend, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
		if err != nil {
			listener.Close()
			closeBackend()
			return nil, err
		}
		peer, err := listener.AcceptTCP()
		listener.Close()
		if err != nil {
			frontend.Close()
			closeBackend()
			return nil, err
		}
		stop := sync.OnceFunc(func() {
			frontend.Close()
			peer.Close()
			closeBackend() // also unblock a simulator forwarding into its pipe
		})
		var upstream io.Writer = backend
		if control != nil {
			if err := frontend.SetWriteBuffer(1024); err != nil {
				stop()
				return nil, err
			}
			if err := peer.SetReadBuffer(1024); err != nil {
				stop()
				return nil, err
			}
			upstream = &handoffControlWriter{Writer: backend, frontend: frontend, peer: peer, addr: addr, gate: control}
		}
		up, down := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(up)
			defer stop()
			_, _ = io.Copy(upstream, peer)
		}()
		go func() {
			defer close(down)
			defer stop()
			_, _ = io.Copy(peer, backend)
		}()
		t.Cleanup(func() { stop(); <-up; <-down })
		frontends.Store(addr, frontend)
		return frontend, nil
	}
}

func TestReadFramePreservedReadyReplyNotifies(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	key := []byte(t.Name() + "/key")
	db, rec := openReadTagDB(t, ctx, sharedClusterFile, plainDial, 0, func() float64 { return 1 })
	setKey(t, ctx, db, key, []byte("v"))
	tx := db.CreateTransaction()
	defer tx.Cancel()
	rv, err := tx.GetReadVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	location, err := tx.db.locCache.locate(tx.db, ctx, key, NoTenantID, types.SpanContext{}, false)
	if err != nil || len(location.Servers) == 0 {
		t.Fatalf("getKey location = %+v, %v", location, err)
	}
	server := location.Servers[0]
	conn, err := tx.db.getOrDial(ctx, server.Address)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := getAdjustedEndpoint(server.Token, EndpointGetKey)
	token, reply, handle := conn.PrepareReplyTo(endpoint)
	cleanup := replyGuard{handle: handle}
	defer cleanup.close()
	body, buffer := buildGetKeyRequest(key, false, 1, rv, false, NoTenantID, types.SpanContext{}, readTagSample{}, token)
	defer getKeyBufPool.Put(buffer)
	rec.start()
	if _, err := conn.SendFrameContext(ctx, endpoint, body); err != nil {
		t.Fatal(err)
	}
	for conn.HasPendingReply(token) {
		if ctx.Err() != nil {
			t.Fatal("real getKey reply was never published")
		}
		runtime.Gosched()
	}
	if len(reply) != 1 || conn.IsClosed() || ctx.Err() != nil {
		t.Fatalf("ready response control: buffered=%d closed=%t ctx=%v", len(reply), conn.IsClosed(), ctx.Err())
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	notifications := 0
	err = sendReadFrame(canceled, conn, endpoint, body, handle, nil, func(context.Context) { notifications++ })
	if err != nil || notifications != 1 || len(reply) != 1 {
		t.Fatalf("preserved-ready send = %v, notifications=%d buffered=%d; want nil/1/1", err, notifications, len(reply))
	}
	response := <-reply
	cleanup.release()
	got, equal, offset, _, err := parseGetKeyReply(response.Body)
	if response.Err != nil || err != nil || !bytes.Equal(got, key) || !equal || offset != 0 {
		t.Fatalf("preserved real reply = %q/%t/%d, %v/%v", got, equal, offset, err, response.Err)
	}
	recorded := rec.stop()
	if err := nrProblems(recorded); err != nil {
		t.Fatal(err)
	}
	if len(recorded.msgs) != 1 || nrCount(recorded.msgs, "GetKeyRequest") != 1 {
		t.Fatalf("canceled send reached peer: %s", nrDescribe(recorded.msgs))
	}
}

func handoffAwait(t *testing.T, ctx context.Context, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for " + what)
	}
}
