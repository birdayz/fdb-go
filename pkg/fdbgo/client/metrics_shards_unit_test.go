package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

type metricsShardResult struct {
	bytes  int64
	points [][]byte
	err    error
}

// Every shard is dispatched before waiting, and a later shard's error cancels
// the unanswered first shard without closing its connection.
func TestMetricsShardsFailFast(t *testing.T) {
	t.Parallel()
	testMetricsShards(t, true)
}

// Out-of-order replies must still assemble split points in shard order.
func TestMetricsShardsPreserveOrder(t *testing.T) {
	t.Parallel()
	testMetricsShards(t, false)
}

// Initial requests sharing one hot peer retain location order while every reply
// is outstanding. One P exposes an uncoordinated spawn loop's runnext ordering.
func TestMetricsShardsPreserveInitialWireOrder(t *testing.T) {
	// Keep this test exclusive: the scheduler setting controls the interleaving,
	// and parallel tests must not observe it.
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split=%t", split), func(t *testing.T) {
			ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			peer := newIdentityPeer(t)
			db, tx := armDatabase(ctx, peer)
			defer closeIdentityDatabase(db)
			servers := []ServerInfo{{Address: armAddr, Token: armToken}}
			bounds := []string{"a", "m", "t", "z"}
			db.locCache.entries = nil
			for i := range 3 {
				db.locCache.entries = append(db.locCache.entries, locationEntry{
					tenantId: NoTenantID, begin: []byte(bounds[i]), end: []byte(bounds[i+1]), servers: servers,
				})
			}
			if _, err := db.getOrDial(ctx, armAddr); err != nil {
				t.Fatal(err)
			}
			var err error
			done := make(chan struct{})
			go func() {
				defer close(done)
				if split {
					_, err = tx.GetRangeSplitPoints(ctx, []byte("a"), []byte("z"), 1000)
				} else {
					_, err = tx.GetEstimatedRangeSizeBytes(ctx, []byte("a"), []byte("z"))
				}
			}()
			defer func() { stop(); <-done }()
			requests := make([]transport.UID, 0, 3)
			dispatchCtx, cancelDispatch := context.WithTimeout(ctx, 5*time.Second)
			defer cancelDispatch()
			for i := range 3 {
				frame, ok := peer.recv(dispatchCtx)
				if !ok {
					t.Fatal("not all ordered shards were dispatched with their replies withheld")
				}
				var begin, end []byte
				var token transport.UID
				endpoint := EndpointWaitMetrics
				if split {
					endpoint = EndpointGetRangeSplitPoints
					var req types.SplitRangeRequest
					if err := req.UnmarshalFDB(frame.body); err != nil {
						t.Fatal(err)
					}
					if req.TenantInfo.TenantId != NoTenantID || req.ChunkSize != 1000 {
						t.Fatalf("unexpected split request: %+v", req)
					}
					begin, end, token = req.Keys.Begin, req.Keys.End, replyToken(req.Reply.Token)
				} else {
					var req types.WaitMetricsRequest
					if err := req.UnmarshalFDB(frame.body); err != nil {
						t.Fatal(err)
					}
					if req.TenantInfo.TenantId != NoTenantID || req.MinVersion != LatestVersion {
						t.Fatalf("unexpected size request: %+v", req)
					}
					begin, end, token = req.Keys.Begin, req.Keys.End, replyToken(req.Reply.Token)
				}
				if frame.token != getAdjustedEndpoint(armToken, endpoint) || string(begin) != bounds[i] || string(end) != bounds[i+1] {
					t.Fatalf("initial request %d = [%q,%q), want [%q,%q) in location order", i, begin, end, bounds[i], bounds[i+1])
				}
				requests = append(requests, token)
			}
			conn := pooledConn(db, armAddr)
			for i, token := range requests {
				if conn == nil || !conn.HasPendingReply(token) {
					t.Fatalf("ordered shard %d was not concurrently waiting for its withheld reply", i)
				}
			}
			if err := transport.WriteFrame(peer.server, requests[2], (&types.ErrorOrError{ErrorCode: uint16(ErrTenantNotFound)}).MarshalFDB(), false); err != nil {
				t.Fatal(err)
			}
			completionCtx, cancelCompletion := context.WithTimeout(ctx, 5*time.Second)
			defer cancelCompletion()
			select {
			case <-done:
			case <-completionCtx.Done():
				t.Fatal("ordered fanout hid the later shard's terminal error")
			}
			if code, ok := fdbErrorCode(err); !ok || code != ErrTenantNotFound {
				t.Fatalf("ordered fanout returned %v, want tenant_not_found", err)
			}
			for i, token := range requests {
				if conn.HasPendingReply(token) {
					t.Fatalf("completed fanout retained shard %d's waiter", i)
				}
			}
			if ctx.Err() != nil || conn.IsClosed() {
				t.Fatal("ordered fanout cancelled its caller or closed its peer")
			}
		})
	}
}

// A retryable size error belongs to that shard's worker, not the parent fanout:
// restarting the whole range could cancel and indefinitely hide a sibling error.
func TestMetricsShardRetryKeepsSibling(t *testing.T) {
	t.Parallel()
	for _, code := range []int{ErrWrongShardServer, ErrAllAlternativesFailed, ErrFutureVersion} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			peer := newIdentityPeer(t)
			db, tx := armDatabase(ctx, peer)
			defer closeIdentityDatabase(db)
			tx.tenantId = 42
			servers := []ServerInfo{{Address: armAddr, Token: armToken}}
			db.locCache.entries = []locationEntry{
				{tenantId: 42, begin: []byte("a"), end: []byte("split_m"), servers: servers},
				{tenantId: 42, begin: []byte("split_m"), end: []byte("z"), servers: servers},
			}
			var err error
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err = tx.GetEstimatedRangeSizeBytes(ctx, []byte("a"), []byte("z"))
			}()
			defer func() { stop(); <-done }()
			requests := make(map[string]transport.UID)
			waitCtx, cancelWait := context.WithTimeout(ctx, 5*time.Second)
			defer cancelWait()
			for range 2 {
				frame, ok := peer.recv(waitCtx)
				if !ok {
					t.Fatal("not all shards were dispatched")
				}
				var req types.WaitMetricsRequest
				if err := req.UnmarshalFDB(frame.body); err != nil {
					t.Fatal(err)
				}
				requests[string(req.Keys.Begin)] = replyToken(req.Reply.Token)
			}
			if len(requests) != 2 || requests["a"] == (transport.UID{}) || requests["split_m"] == (transport.UID{}) {
				t.Fatalf("unexpected initial shards: %v", requests)
			}
			if err := transport.WriteFrame(peer.server, requests["a"], (&types.ErrorOrError{ErrorCode: uint16(code)}).MarshalFDB(), false); err != nil {
				t.Fatal(err)
			}
			frame, ok := peer.recv(waitCtx)
			if !ok {
				t.Fatal("failed shard did not retry")
			}
			var retryToken transport.UID
			if code == ErrFutureVersion {
				var req types.WaitMetricsRequest
				if err := req.UnmarshalFDB(frame.body); err != nil {
					t.Fatal(err)
				}
				if frame.token != getAdjustedEndpoint(armToken, EndpointWaitMetrics) || string(req.Keys.Begin) != "a" || string(req.Keys.End) != "split_m" || req.TenantInfo.TenantId != 42 || req.MinVersion != 1 {
					t.Fatalf("future-version retry did not retain its shard: %+v", req.Keys)
				}
				retryToken = replyToken(req.Reply.Token)
			} else {
				var req types.GetKeyServerLocationsRequest
				if err := req.UnmarshalFDB(frame.body); err != nil {
					t.Fatal(err)
				}
				if frame.token != getAdjustedEndpoint(armToken, EndpointGetKeyServerLocations) || string(req.Begin) != "a" || !req.HasEnd || string(req.End) != "split_m" || req.Tenant.TenantId != 42 {
					t.Fatalf("relocation did not retain its shard: [%q,%q)", req.Begin, req.End)
				}
				retryToken = replyToken(req.Reply.Token)
			}
			conn := pooledConn(db, armAddr)
			if conn == nil || !conn.HasPendingReply(requests["split_m"]) {
				t.Fatal("retrying one shard cancelled its sibling")
			}
			db.locCache.mu.RLock()
			_, siblingCached := db.locCache.lookupLocked(42, []byte("split_n"), false)
			db.locCache.mu.RUnlock()
			if !siblingCached {
				t.Fatal("retrying one shard invalidated its sibling's location")
			}
			if !conn.HasPendingReply(retryToken) {
				t.Fatal("failed shard is not waiting for its withheld relocation or retry reply")
			}
			if err := transport.WriteFrame(peer.server, requests["split_m"], (&types.ErrorOrError{ErrorCode: uint16(ErrTenantNotFound)}).MarshalFDB(), false); err != nil {
				t.Fatal(err)
			}
			completionCtx, cancelCompletion := context.WithTimeout(ctx, 5*time.Second)
			defer cancelCompletion()
			select {
			case <-done:
			case <-completionCtx.Done():
				t.Fatal("retrying shard hid its sibling's terminal error")
			}
			if got, ok := fdbErrorCode(err); !ok || got != ErrTenantNotFound {
				t.Fatalf("got %v, want tenant_not_found", err)
			}
			if ctx.Err() != nil || conn.HasPendingReply(retryToken) || conn.IsClosed() {
				t.Fatal("terminal error must cancel/join only the retry waiter, not its operation context or connection")
			}
		})
	}
}

// Relocation can split one worker while an original sibling remains in flight.
// Replacement locations may extend beyond the worker, but its requests must not.
func TestMetricsShardRelocationSplitsWorker(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	peer, replacementPeer := newIdentityPeer(t), newIdentityPeer(t)
	db, tx := armDatabase(ctx, peer, replacementPeer)
	defer closeIdentityDatabase(db)
	servers := []ServerInfo{{Address: armAddr, Token: armToken}}
	db.locCache.entries = []locationEntry{
		{tenantId: NoTenantID, begin: []byte("a"), end: []byte("m"), servers: servers},
		{tenantId: NoTenantID, begin: []byte("m"), end: []byte("z"), servers: servers},
	}
	var total int64
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		total, err = tx.GetEstimatedRangeSizeBytes(ctx, []byte("d"), []byte("z"))
	}()
	defer func() { stop(); <-done }()

	initial := make(map[string]transport.UID)
	dispatchCtx, cancelDispatch := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDispatch()
	for range 2 {
		frame, ok := peer.recv(dispatchCtx)
		if !ok {
			t.Fatal("not all original shards were dispatched")
		}
		var req types.WaitMetricsRequest
		if err := req.UnmarshalFDB(frame.body); err != nil {
			t.Fatal(err)
		}
		wantEnd := map[string]string{"d": "m", "m": "z"}[string(req.Keys.Begin)]
		if frame.token != getAdjustedEndpoint(armToken, EndpointWaitMetrics) || wantEnd == "" || string(req.Keys.End) != wantEnd || req.TenantInfo.TenantId != NoTenantID || req.MinVersion != LatestVersion {
			t.Fatalf("unexpected original metrics request: %+v", req)
		}
		if _, duplicate := initial[string(req.Keys.Begin)]; duplicate {
			t.Fatalf("duplicate original shard %q", req.Keys.Begin)
		}
		initial[string(req.Keys.Begin)] = replyToken(req.Reply.Token)
	}
	if err := transport.WriteFrame(peer.server, initial["d"], (&types.ErrorOrError{ErrorCode: uint16(ErrWrongShardServer)}).MarshalFDB(), false); err != nil {
		t.Fatal(err)
	}
	frame, ok := peer.recv(dispatchCtx)
	if !ok {
		t.Fatal("failed worker did not request relocation")
	}
	var relocation types.GetKeyServerLocationsRequest
	if err := relocation.UnmarshalFDB(frame.body); err != nil {
		t.Fatal(err)
	}
	if frame.token != getAdjustedEndpoint(armToken, EndpointGetKeyServerLocations) || string(relocation.Begin) != "d" || !relocation.HasEnd || string(relocation.End) != "m" || relocation.Tenant.TenantId != NoTenantID {
		t.Fatalf("relocation widened the failed worker: %+v", relocation)
	}
	conn := pooledConn(db, armAddr)
	if conn == nil || !conn.HasPendingReply(initial["m"]) {
		t.Fatal("relocation cancelled the original sibling")
	}
	db.locCache.mu.RLock()
	sibling, cached := db.locCache.lookupLocked(NoTenantID, []byte("y"), false)
	db.locCache.mu.RUnlock()
	if !cached || !reflect.DeepEqual(sibling.Servers, servers) {
		t.Fatal("relocation invalidated the original sibling's location")
	}

	// Both replacement outer bounds exceed [d,m); only [d,h) and [h,m)
	// belong to this worker. The original sibling still owns its [m,z) reply.
	replacement := metricsReplacementLocationsReply(t, [2]string{"a", "h"}, [2]string{"h", "n"})
	entries, parseErr := parseGetKeyServerLocationsReply(replacement)
	if parseErr != nil || len(entries) != 2 || len(entries[0].servers) != 1 || len(entries[1].servers) != 1 {
		t.Fatalf("replacement fixture = %+v, %v", entries, parseErr)
	}
	if err := transport.WriteFrame(peer.server, replyToken(relocation.Reply.Token), replacement, false); err != nil {
		t.Fatal(err)
	}
	children := make(map[string]transport.UID)
	childCtx, cancelChildren := context.WithTimeout(ctx, 5*time.Second)
	defer cancelChildren()
	for range 2 {
		frame, ok := replacementPeer.recv(childCtx)
		if !ok {
			t.Fatal("relocation did not dispatch both replacement children")
		}
		var req types.WaitMetricsRequest
		if err := req.UnmarshalFDB(frame.body); err != nil {
			t.Fatal(err)
		}
		wantEnd := map[string]string{"d": "h", "h": "m"}[string(req.Keys.Begin)]
		if frame.token != getAdjustedEndpoint(entries[0].servers[0].Token, EndpointWaitMetrics) || wantEnd == "" || string(req.Keys.End) != wantEnd || req.TenantInfo.TenantId != NoTenantID || req.MinVersion != LatestVersion {
			t.Fatalf("replacement child escaped the failed worker: %+v", req)
		}
		if _, duplicate := children[string(req.Keys.Begin)]; duplicate {
			t.Fatalf("duplicate replacement child %q", req.Keys.Begin)
		}
		children[string(req.Keys.Begin)] = replyToken(req.Reply.Token)
	}
	if !conn.HasPendingReply(initial["m"]) {
		t.Fatal("replacement fanout cancelled the original sibling")
	}
	db.locCache.mu.RLock()
	sibling, cached = db.locCache.lookupLocked(NoTenantID, []byte("y"), false)
	db.locCache.mu.RUnlock()
	if !cached || !reflect.DeepEqual(sibling.Servers, servers) {
		t.Fatal("replacement locations discarded the original sibling's unaffected cache")
	}
	metricsReply := func(n uint64) []byte {
		body := replyVector(t, "StorageMetrics_basic")
		var r wire.Reader
		if err := wire.ReadErrorOrInto(body, &r); err != nil {
			t.Fatal(err)
		}
		if !r.FieldPresent(types.StorageMetricsSlotBytes) {
			t.Fatal("metrics fixture has no bytes field")
		}
		binary.LittleEndian.PutUint64(body[r.ObjectPos()+r.FieldOffset(types.StorageMetricsSlotBytes):], n)
		return body
	}
	// Complete the children out of order, then release the original sibling.
	if err := transport.WriteFrame(replacementPeer.server, children["h"], metricsReply(17), false); err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteFrame(replacementPeer.server, children["d"], metricsReply(11), false); err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteFrame(peer.server, initial["m"], metricsReply(29), false); err != nil {
		t.Fatal(err)
	}
	completionCtx, cancelCompletion := context.WithTimeout(ctx, 5*time.Second)
	defer cancelCompletion()
	select {
	case <-done:
	case <-completionCtx.Done():
		t.Fatal("split worker did not aggregate its children and original sibling")
	}
	if err != nil || total != 57 {
		t.Fatalf("size = %d, %v; want 57 (11 + 17 + 29)", total, err)
	}
	if ctx.Err() != nil || conn.IsClosed() {
		t.Fatal("successful replacement fanout ended its caller or original connection")
	}
}

// Reuse the ground-truth server object while supplying replacement range pairs.
func metricsReplacementLocationsReply(t *testing.T, ranges ...[2]string) []byte {
	t.Helper()
	vector := locationsReplyVector(t)
	_, server := firstLocation(t, vector)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[4:], types.GetKeyServerLocationsReplyFileID)
	b, root := appendObject(b, 0, 9, 8, 4)
	b[root+8] = 2 // ErrorOr value arm
	b, reply := appendObject(b, root+4, int(types.GetKeyServerLocationsReplyVTable[1]), types.GetKeyServerLocationsReplyVTable[2:]...)
	results := len(b)
	binary.LittleEndian.PutUint32(b[reply+int(types.GetKeyServerLocationsReplyVTable[2]):], uint32(results-reply-int(types.GetKeyServerLocationsReplyVTable[2])))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(ranges)))
	slots := len(b)
	b = append(b, make([]byte, 4*len(ranges))...)
	for i, bounds := range ranges {
		var pair, keyRange int
		b, pair = appendObject(b, slots+4*i, int(types.LocationPairVTable[1]), types.LocationPairVTable[2:]...)
		b, keyRange = appendObject(b, pair+int(types.LocationPairVTable[2]), int(types.KeyRangeRefVTable[1]), types.KeyRangeRefVTable[2:]...)
		b = appendString(b, keyRange+int(types.KeyRangeRefVTable[2]), bounds[0])
		b = appendString(b, keyRange+int(types.KeyRangeRefVTable[3]), bounds[1])
		servers := len(b)
		binary.LittleEndian.PutUint32(b[pair+int(types.LocationPairVTable[3]):], uint32(servers-pair-int(types.LocationPairVTable[3])))
		b = binary.LittleEndian.AppendUint32(b, 1)
		b = binary.LittleEndian.AppendUint32(b, uint32(4+server.ObjectPos()))
		b = append(b, vector...)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
	}
	return b
}

func testMetricsShards(t *testing.T, terminalError bool) {
	t.Helper()
	for _, split := range []bool{false, true} {
		name := "size"
		if split {
			name = "split points"
		}
		t.Run(name, func(t *testing.T) {
			ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			peer := newIdentityPeer(t)
			db, tx := armDatabase(ctx, peer)
			defer closeIdentityDatabase(db)
			tx.tenantId = 42
			servers := []ServerInfo{{Address: armAddr, Token: armToken}}
			db.locCache.entries = []locationEntry{
				{tenantId: 42, begin: []byte("a"), end: []byte("split_m"), servers: servers},
				{tenantId: 42, begin: []byte("split_m"), end: []byte("z"), servers: servers},
			}
			var got metricsShardResult
			done := make(chan struct{})
			go func() {
				defer close(done)
				if split {
					got.points, got.err = tx.GetRangeSplitPoints(ctx, []byte("a"), []byte("z"), 1000)
				} else {
					got.bytes, got.err = tx.GetEstimatedRangeSizeBytes(ctx, []byte("a"), []byte("z"))
				}
			}()
			defer func() { stop(); <-done }()

			// Withhold both replies until both requests arrive. Serial dispatch
			// cannot pass this gate, even if the later error happens to be ready.
			requests := make(map[string]transport.UID)
			dispatchCtx, cancelDispatch := context.WithTimeout(ctx, 5*time.Second)
			defer cancelDispatch()
			for range 2 {
				frame, ok := peer.recv(dispatchCtx)
				if !ok {
					t.Fatal("not all shards were dispatched before waiting for replies")
				}
				var begin, end []byte
				var token transport.UID
				if split {
					var req types.SplitRangeRequest
					if err := req.UnmarshalFDB(frame.body); err != nil {
						t.Fatal(err)
					}
					if req.TenantInfo.TenantId != 42 {
						t.Fatalf("tenant = %d, want 42", req.TenantInfo.TenantId)
					}
					begin, end, token = req.Keys.Begin, req.Keys.End, replyToken(req.Reply.Token)
				} else {
					var req types.WaitMetricsRequest
					if err := req.UnmarshalFDB(frame.body); err != nil {
						t.Fatal(err)
					}
					if req.TenantInfo.TenantId != 42 || req.MinVersion != 1 {
						t.Fatalf("tenant/version = %d/%d, want 42/1", req.TenantInfo.TenantId, req.MinVersion)
					}
					begin, end, token = req.Keys.Begin, req.Keys.End, replyToken(req.Reply.Token)
				}
				wantEnd := map[string]string{"a": "split_m", "split_m": "z"}[string(begin)]
				if wantEnd == "" || string(end) != wantEnd {
					t.Fatalf("unexpected shard range [%q,%q)", begin, end)
				}
				if _, duplicate := requests[string(begin)]; duplicate {
					t.Fatalf("duplicate request for shard %q", begin)
				}
				requests[string(begin)] = token
			}
			conn := pooledConn(db, armAddr)
			if conn == nil || !conn.HasPendingReply(requests["a"]) {
				t.Fatal("first shard is not waiting for its withheld reply")
			}
			if terminalError {
				errBody := (&types.ErrorOrError{ErrorCode: uint16(ErrTenantNotFound)}).MarshalFDB()
				if err := transport.WriteFrame(peer.server, requests["split_m"], errBody, false); err != nil {
					t.Fatal(err)
				}
			} else {
				fixture := "StorageMetrics_basic"
				if split {
					fixture = "SplitRangeReply_two_points"
				}
				first := replyVector(t, fixture)
				second := first
				if split {
					second = bytes.ReplaceAll(first, []byte("split_a"), []byte("split_x"))
					second = bytes.ReplaceAll(second, []byte("split_b"), []byte("split_y"))
				}
				if err := transport.WriteFrame(peer.server, requests["split_m"], second, false); err != nil {
					t.Fatal(err)
				}
				if err := transport.WriteFrame(peer.server, requests["a"], first, false); err != nil {
					t.Fatal(err)
				}
			}
			completionCtx, cancelCompletion := context.WithTimeout(ctx, 5*time.Second)
			defer cancelCompletion()
			select {
			case <-done:
			case <-completionCtx.Done():
				t.Fatal("metrics operation did not finish with the first shard's reply withheld or delivered")
			}
			if ctx.Err() != nil {
				t.Fatalf("operation context ended before the result: %v", ctx.Err())
			}
			if terminalError {
				if code, ok := fdbErrorCode(got.err); !ok || code != ErrTenantNotFound {
					t.Fatalf("got %v, want tenant_not_found", got.err)
				}
				if conn.HasPendingReply(requests["a"]) {
					t.Fatal("failed operation left the first shard's reply waiter registered")
				}
			} else if got.err != nil {
				t.Fatal(got.err)
			} else if split {
				want := [][]byte{[]byte("a"), []byte("split_a"), []byte("split_b"), []byte("split_m"), []byte("split_x"), []byte("split_y"), []byte("z")}
				if !reflect.DeepEqual(got.points, want) {
					t.Fatalf("points = %q, want %q", got.points, want)
				}
			} else if got.bytes != 246913578 {
				t.Fatalf("bytes = %d, want 246913578", got.bytes)
			}
			if conn.IsClosed() {
				t.Fatal("cancelling a shard waiter closed the shared connection")
			}
		})
	}
}
