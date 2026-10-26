package client

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
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
