package client

import (
	"bytes"
	"context"
	"encoding/binary"
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	prefix := t.Name() + "/"
	sim, draws := newSimDialer(), &readTagDraws{}
	db, rec := openReadTagDB(t, ctx, sharedClusterFile, sim.dial, 0.5, draws.draw)
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
	partialSeen, releasePartial, continued := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce, continuedOnce sync.Once
	unblockPartial := func() { releaseOnce.Do(func() { close(releasePartial) }) }
	defer unblockPartial()
	var injected atomic.Bool
	var beginToken transport.UID
	sim.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
		if rec.replyKind(token) != "GetKeyRequest" {
			return body, false
		}
		if !injected.CompareAndSwap(false, true) {
			// End is held before its first request. A second real reply while
			// that gate is closed positively proves begin2 reached the peer.
			continuedOnce.Do(func() { close(continued) })
			return body, false
		}
		beginToken = token
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
		close(partialSeen)
		select {
		case <-releasePartial:
		case <-ctx.Done():
		}
		return partial, false
	})
	defer sim.setIntercept(nil)
	endEntered, releaseEnd := make(chan struct{}), make(chan struct{})
	var endOnce sync.Once
	unblockEnd := func() { endOnce.Do(func() { close(releaseEnd) }) }
	defer unblockEnd()
	beginAtGate := make(chan struct{})
	var gateOnce sync.Once
	tx.beforeResolveKeyResume = func(selector KeySelector) {
		if string(selector.Key) == prefix+"a" {
			gateOnce.Do(func() { close(beginAtGate) })
		}
	}
	actorCtx := context.WithValue(ctx, resolveKeyWorkerObserver{}, func(selector KeySelector) {
		if string(selector.Key) == prefix+"d" {
			close(endEntered)
			select {
			case <-releaseEnd:
			case <-ctx.Done():
			}
		}
	})
	sim.armEverything()
	draws.script(drawHit, drawMiss, drawHit, drawMiss)
	rec.start()
	done := make(chan struct{})
	var result nativeRangeResult
	var readErr error
	go func() {
		defer close(done)
		result, readErr = tx.nativeGetRangeFallback(actorCtx, KeySelector{Key: []byte(prefix + "a"), Offset: 2}, KeySelector{Key: []byte(prefix + "d"), Offset: 2}, newRangeLimits(0, -1), rv, false)
	}()
	handoffAwait(t, ctx, endEntered, "end resolver entering its startup")
	handoffAwait(t, ctx, partialSeen, "real first getKey reply held before publication")
	if !conn.HasPendingReply(beginToken) {
		t.Fatal("positive control: held begin reply has no registration")
	}
	unblockPartial()
	for conn.HasPendingReply(beginToken) {
		if ctx.Err() != nil {
			t.Fatal("begin partial reply was never published")
		}
		runtime.Gosched()
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
	if len(beforeEnd) != 1 || beforeEnd[0] != prefix+"a" {
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
	if len(keys) != 3 || keys[0] != want[0] || keys[1] != want[1] || keys[2] != want[2] {
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
	checkNewRequests(t, requests, draws.count(), 4, taggedRequest{has: true, tags: string(encodeTagSet([]string{"rt"}))})
}

func handoffAwait(t *testing.T, ctx context.Context, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for " + what)
	}
}
