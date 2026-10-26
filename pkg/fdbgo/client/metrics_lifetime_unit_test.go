package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// Database cancellation alone must end a reply wait; neither the live socket
// nor the independently live caller can supply the competing cancellation arm.
func TestMetricsReplyWaitObservesDatabaseOnlyCancellation(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			dbCtx, stopDB := context.WithCancel(ctx)
			defer stopDB()
			peer := newIdentityPeer(t)
			db, tx := armDatabase(dbCtx, peer)
			db.cancel = stopDB
			conn, err := db.getOrDial(ctx, armAddr)
			if err != nil {
				t.Fatal(err)
			}
			waiting := make(chan struct{})
			beforeWait := func() { close(waiting) }
			db.beforeMetricsReplyWait.Store(&beforeWait)
			var opErr error
			done := make(chan struct{})
			go func() { defer close(done); opErr = arm.operation(ctx, tx, []byte("k"), []byte("l")) }()
			defer func() { cancel(); <-done; _ = (&Database{db: db}).Close() }()
			frame, ok := peer.recv(ctx)
			if !ok {
				t.Fatal("metrics request missing")
			}
			reply := arm.replyToken(t, frame.body)
			select {
			case <-waiting:
			case <-ctx.Done():
				t.Fatal("sender did not reach its reply wait")
			}
			if !conn.HasPendingReply(reply) || conn.IsClosed() {
				t.Fatal("reply must be outstanding on a live connection")
			}
			stopDB()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("reply wait ignored database-only cancellation")
			}
			if !errors.Is(opErr, context.Canceled) || ctx.Err() != nil || conn.IsClosed() || conn.HasPendingReply(reply) {
				t.Fatalf("database-only cancellation: err=%v caller=%v socketClosed=%t pending=%t", opErr, ctx.Err(), conn.IsClosed(), conn.HasPendingReply(reply))
			}
		})
	}
}

// Start with no other database workers: enrolling the shared dial after it
// finishes would let shutdown pass its zero-count join while this dial is held.
func TestDatabaseCloseJoinsPendingSharedDial(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dbCtx, stopDB := context.WithCancel(ctx)
	defer stopDB()
	db, _ := identityDatabase(dbCtx)
	db.cancel = stopDB
	entered, resume := make(chan struct{}), make(chan struct{})
	var attempts atomic.Int32
	db.dialFn = func(context.Context, string, string) (net.Conn, error) {
		if attempts.Add(1) == 1 {
			close(entered)
		}
		<-resume
		return nil, errors.New("test: held dial released")
	}
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	lookup := make(chan error, 1)
	go func() { _, err := db.getOrDial(ctx, armAddr); lookup <- err }()
	var closeDone chan error
	defer func() {
		release()
		if closeDone != nil {
			<-closeDone
		} else {
			_ = (&Database{db: db}).Close()
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("shared dial did not enter its gate")
	}
	db.connMu.Lock()
	call := db.dialing[armAddr]
	db.connMu.Unlock()
	if call == nil {
		t.Fatal("no pending shared dial cohort")
	}
	select {
	case <-call.done:
		t.Fatal("dial completed before shutdown")
	default:
	}
	joinReached := make(chan struct{})
	beforeJoin := func() { close(joinReached) }
	db.beforeCloseJoin.Store(&beforeJoin)
	closeDone = make(chan error, 1)
	go func() { closeDone <- (&Database{db: db}).Close() }()
	select {
	case <-joinReached:
	case <-ctx.Done():
		t.Fatal("shutdown did not reach its zero-count join boundary")
	}
	if !errors.Is(dbCtx.Err(), context.Canceled) || ctx.Err() != nil {
		t.Fatal("only the database lifetime must be cancelled")
	}
	select {
	case err := <-closeDone:
		closeDone <- err
		t.Fatal("shutdown abandoned a still-pending shared dial")
	case <-time.After(100 * time.Millisecond):
	}
	conn, permit, err := db.getOrDialForSend(ctx, "late:4500")
	if permit != nil {
		permit.Release()
	}
	if conn != nil || !errors.Is(err, context.Canceled) || attempts.Load() != 1 {
		t.Fatalf("late enrollment: conn=%p err=%v attempts=%d", conn, err, attempts.Load())
	}
	release()
	select {
	case err := <-lookup:
		if err == nil {
			t.Fatal("failed dial published success")
		}
	case <-ctx.Done():
		t.Fatal("released dial did not publish failure")
	}
	select {
	case <-call.done:
	case <-ctx.Done():
		t.Fatal("dial result remained unpublished")
	}
	if call.conn != nil || call.err == nil {
		t.Fatalf("released dial result: conn=%p err=%v", call.conn, call.err)
	}
	select {
	case err := <-closeDone:
		closeDone <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("shutdown did not join the released dial owner")
	}
	db.connMu.Lock()
	retained := db.dialing[armAddr] != nil
	db.connMu.Unlock()
	if retained {
		t.Fatal("failed pending dial left a published cohort after shutdown")
	}
}

// Once disconnect is selected, a concurrently published successful reply still
// wins. Gate that exact branch before its ready-reply check, not the select race.
func TestMetricsPublishedReplyWinsDisconnect(t *testing.T) {
	t.Parallel()
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split=%t", split), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			dbCtx, stopDB := context.WithCancel(ctx)
			defer stopDB()
			primary, backup := newIdentityPeer(t), newIdentityPeer(t)
			gate := &closeGateConn{Conn: primary.client, closing: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
			primary.client = gate
			db, tx := armDatabase(dbCtx, primary, backup)
			db.cancel = stopDB
			const backupAddr = "backup:4500"
			conn, err := db.getOrDial(ctx, armAddr)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.getOrDial(ctx, backupAddr); err != nil {
				t.Fatal(err)
			}
			db.failMon.markFailed(backupAddr)
			db.failMon.mu.Lock()
			db.failMon.failed[backupAddr].excludedUntil = nowSeconds() + 600
			db.failMon.mu.Unlock()
			db.locCache.entries = []locationEntry{{tenantId: NoTenantID, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: armAddr, Token: armToken}, {Address: backupAddr, Token: armToken}}}}
			entered, resume := make(chan struct{}), make(chan struct{})
			hook := func() {
				close(entered)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			}
			db.beforeMetricsDisconnectReply.Store(&hook)
			waiting := make(chan struct{})
			var waitOnce sync.Once
			beforeWait := func() { waitOnce.Do(func() { close(waiting) }) }
			db.beforeMetricsReplyWait.Store(&beforeWait)
			var gateOnce, resumeOnce sync.Once
			release := func() { gateOnce.Do(func() { close(gate.release) }); resumeOnce.Do(func() { close(resume) }) }
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
			defer func() { release(); cancel(); <-done; _ = (&Database{db: db}).Close() }()
			frame, ok := primary.recv(ctx)
			if !ok {
				t.Fatal("primary metrics request missing")
			}
			arm := metricsArms()[0]
			if split {
				arm = metricsArms()[1]
			}
			reply := arm.replyToken(t, frame.body)
			if !conn.HasPendingReply(reply) {
				t.Fatal("primary reply was not outstanding")
			}
			select {
			case <-waiting:
			case <-ctx.Done():
				t.Fatal("primary sender did not finish its flush before disconnect")
			}
			db.failMon.markAlive(backupAddr)
			db.handleConnError(armAddr, conn)
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("metrics waiter did not select connection death")
			}
			select {
			case <-gate.closing:
			case <-ctx.Done():
				t.Fatal("socket close was not gated")
			}
			if err := transport.WriteFrame(primary.server, reply, replyVector(t, arm.reply), false); err != nil {
				t.Fatal(err)
			}
			for conn.HasPendingReply(reply) {
				select {
				case <-ctx.Done():
					t.Fatal("primary reply was not published")
				default:
					runtime.Gosched()
				}
			}
			resumeOnce.Do(func() { close(resume) })
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("published primary reply was discarded after disconnect")
			}
			if got.err != nil {
				t.Fatal(got.err)
			}
			if split {
				want := [][]byte{[]byte("a"), []byte("split_a"), []byte("split_b"), []byte("z")}
				if !reflect.DeepEqual(got.points, want) {
					t.Fatalf("primary split points = %q, want %q", got.points, want)
				}
			} else if got.bytes != 123456789 {
				t.Fatalf("primary size = %d, want 123456789", got.bytes)
			}
			select {
			case <-backup.frames:
				t.Fatal("published reply caused an unnecessary backup request")
			default:
			}
			select {
			case <-gate.done:
				t.Fatal("socket close completed before the primary result")
			default:
			}
		})
	}
}

// A reply already outstanding must observe disconnect/database cancellation
// before pending-reply failures can be published by a blocked socket close.
func TestMetricsReplyWaitEndsBeforeSocketClose(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		for _, shutdown := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shutdown=%t", arm.name, shutdown), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				dbCtx, stopDB := context.WithCancel(ctx)
				defer stopDB()
				first, backup := newIdentityPeer(t), newIdentityPeer(t)
				gate := &closeGateConn{Conn: first.client, closing: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
				first.client = gate
				db, tx := armDatabase(dbCtx, first, backup)
				db.cancel = stopDB
				db.altFailureDelayOverride = 30 * time.Second
				waiting := make(chan struct{})
				var waitOnce sync.Once
				beforeWait := func() { waitOnce.Do(func() { close(waiting) }) }
				db.beforeMetricsReplyWait.Store(&beforeWait)
				const backupAddr = "backup:4500"
				primary, err := db.getOrDial(ctx, armAddr)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.getOrDial(ctx, backupAddr); err != nil {
					t.Fatal(err)
				}
				db.failMon.markFailed(backupAddr)
				db.failMon.mu.Lock()
				db.failMon.failed[backupAddr].excludedUntil = nowSeconds() + 600
				db.failMon.mu.Unlock()
				db.locCache.entries = []locationEntry{{tenantId: NoTenantID, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: armAddr, Token: armToken}, {Address: backupAddr, Token: armToken}}}}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(gate.release) }) }
				opCtx, stop := context.WithCancel(ctx)
				done := make(chan struct{})
				var opErr error
				go func() { defer close(done); opErr = arm.operation(opCtx, tx, []byte("a"), []byte("z")) }()
				var closeDone chan error
				defer func() {
					release()
					stop()
					<-done
					if closeDone != nil {
						<-closeDone
					} else {
						_ = (&Database{db: db}).Close()
					}
				}()
				frame, ok := first.recv(ctx)
				if !ok || frame.token != getAdjustedEndpoint(armToken, arm.endpoint) {
					t.Fatal("initial metrics request missing")
				}
				oldReply := arm.replyToken(t, frame.body)
				if !primary.HasPendingReply(oldReply) {
					t.Fatal("reply must already be outstanding")
				}
				select {
				case <-waiting:
				case <-ctx.Done():
					t.Fatal("sender did not reach its outstanding-reply wait")
				}
				if shutdown {
					closeDone = make(chan error, 1)
					go func() { closeDone <- (&Database{db: db}).Close() }()
				} else {
					db.failMon.markAlive(backupAddr)
					db.handleConnError(armAddr, primary)
				}
				select {
				case <-gate.closing:
				case <-ctx.Done():
					t.Fatal("socket close did not enter its gate")
				}
				if !shutdown {
					waitCtx, stopWait := context.WithTimeout(ctx, time.Second)
					defer stopWait()
					frame, ok := backup.recv(waitCtx)
					if !ok || frame.token != getAdjustedEndpoint(armToken, arm.endpoint) {
						t.Fatal("disconnect did not fail over while close remained blocked")
					}
					if err := transport.WriteFrame(backup.server, arm.replyToken(t, frame.body), replyVector(t, arm.reply), false); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("outstanding metrics reply wait depended on socket close")
				}
				if shutdown {
					if !errors.Is(opErr, context.Canceled) {
						t.Fatalf("shutdown reply wait: %v", opErr)
					}
				} else if opErr != nil {
					t.Fatal(opErr)
				}
				if ctx.Err() != nil || primary.HasPendingReply(oldReply) {
					t.Fatal("reply cleanup must not depend on caller cancellation")
				}
				select {
				case <-gate.done:
					t.Fatal("operation completed only after socket close")
				default:
				}
			})
		}
	}
}

// A connection removed from the pool still belongs to shutdown until its socket
// and transport loops finish; late reports must not register a new owner.
func TestDatabaseCloseJoinsDetachedConnection(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dbCtx, stopDB := context.WithCancel(ctx)
	defer stopDB()
	peer := newIdentityPeer(t)
	gate := &closeGateConn{Conn: peer.client, closing: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	peer.client = gate
	db, _ := identityDatabase(dbCtx, peer)
	db.cancel = stopDB
	conn, err := db.getOrDial(ctx, armAddr)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	done := make(chan error, 1)
	shutdownStarted := false
	defer func() {
		release()
		if shutdownStarted {
			<-done
		} else {
			_ = (&Database{db: db}).Close()
		}
		conn.Close()
	}()
	db.handleConnError(armAddr, conn)
	select {
	case <-gate.closing:
	case <-ctx.Done():
		t.Fatal("detached close did not start")
	}
	if pooledConn(db, armAddr) != nil {
		t.Fatal("connection was not detached")
	}
	joinReached := make(chan struct{})
	beforeJoin := func() { close(joinReached) }
	db.beforeCloseJoin.Store(&beforeJoin)
	shutdownStarted = true
	go func() { done <- (&Database{db: db}).Close() }()
	select {
	case <-joinReached:
	case <-ctx.Done():
		t.Fatal("shutdown did not reach its drained-pool join barrier")
	}
	select {
	case err := <-done:
		done <- err
		t.Fatal("shutdown returned before detached teardown completed")
	case <-time.After(100 * time.Millisecond):
	}
	// A report after the pool drain must neither escape ownership nor Add after Wait.
	for range 20 {
		db.handleConnError(armAddr, conn)
	}
	release()
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("shutdown did not join detached teardown")
	}
	select {
	case <-gate.done:
	default:
		t.Fatal("shutdown returned before socket close completed")
	}
}

// Both unpublished-connection branches must publish failure without retaining
// connMu during Close, while their dial owner remains joined by shutdown.
func TestDialTeardownPublishesBeforeClose(t *testing.T) {
	t.Parallel()
	for _, panicPool := range []bool{false, true} {
		t.Run(fmt.Sprintf("panic=%t", panicPool), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			dbCtx, stopDB := context.WithCancel(ctx)
			defer stopDB()
			peer := newIdentityPeer(t)
			gate := &closeGateConn{Conn: peer.client, closing: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
			peer.client = gate
			db, _ := identityDatabase(dbCtx, peer)
			db.cancel = stopDB
			if panicPool {
				db.connPool = nil
			} else {
				hook := func() { stopDB() }
				db.afterDialBeforePool.Store(&hook)
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate.release) }) }
			lookup := make(chan error, 1)
			go func() { _, err := db.getOrDial(ctx, armAddr); lookup <- err }()
			var closeDone chan error
			defer func() {
				release()
				if closeDone != nil {
					<-closeDone
				} else {
					_ = (&Database{db: db}).Close()
				}
			}()
			select {
			case <-gate.closing:
			case <-ctx.Done():
				t.Fatal("unpublished connection did not enter gated close")
			}
			select {
			case err := <-lookup:
				if err == nil || (!panicPool && !errors.Is(err, context.Canceled)) {
					t.Fatalf("dial result = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("dial failure publication waited for socket close")
			}
			locked := make(chan struct{})
			go func() { db.connMu.Lock(); _ = len(db.connPool); db.connMu.Unlock(); close(locked) }()
			select {
			case <-locked:
			case <-time.After(time.Second):
				t.Fatal("unpublished close held connMu")
			}
			joinReached := make(chan struct{})
			beforeJoin := func() { close(joinReached) }
			db.beforeCloseJoin.Store(&beforeJoin)
			closeDone = make(chan error, 1)
			go func() { closeDone <- (&Database{db: db}).Close() }()
			select {
			case <-joinReached:
			case <-ctx.Done():
				t.Fatal("shutdown did not reach the unpublished-dial join barrier")
			}
			select {
			case err := <-closeDone:
				closeDone <- err
				t.Fatal("shutdown abandoned an unpublished dial's teardown")
			case <-time.After(100 * time.Millisecond):
			}
			release()
		})
	}
}

// Split-point retries restart the entire fanout, unlike size's shard-local
// retries. Completed old points and an unanswered old sibling cannot survive.
func TestSplitPointsRetryRestartsAllShards(t *testing.T) {
	t.Parallel()
	for _, code := range []int{1001, 1006} { // C++ wrong_shard_server, all_alternatives_failed.
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			old, fresh := newIdentityPeer(t), newIdentityPeer(t)
			db, tx := armDatabase(ctx, old, fresh)
			defer closeIdentityDatabase(db)
			bounds := []string{"a", "m", "t", "z"}
			oldRetained := make(chan struct{})
			var retainedOnce sync.Once
			afterResult := func(i int) {
				if i == 0 {
					retainedOnce.Do(func() { close(oldRetained) })
				}
			}
			db.afterMetricsShardResult.Store(&afterResult)
			servers := []ServerInfo{{Address: armAddr, Token: armToken}}
			db.locCache.entries = nil
			for i := range 3 {
				db.locCache.entries = append(db.locCache.entries, locationEntry{tenantId: NoTenantID, begin: []byte(bounds[i]), end: []byte(bounds[i+1]), servers: servers})
			}
			var points [][]byte
			var err error
			done := make(chan struct{})
			go func() { defer close(done); points, err = tx.GetRangeSplitPoints(ctx, []byte("a"), []byte("z"), 1000) }()
			defer func() { cancel(); <-done }()
			initial := make([]transport.UID, 3)
			for i := range 3 {
				frame, ok := old.recv(ctx)
				if !ok {
					t.Fatal("initial split fanout missing")
				}
				var req types.SplitRangeRequest
				if err := req.UnmarshalFDB(frame.body); err != nil {
					t.Fatal(err)
				}
				if string(req.Keys.Begin) != bounds[i] || string(req.Keys.End) != bounds[i+1] {
					t.Fatalf("initial range = %+v", req.Keys)
				}
				initial[i] = replyToken(req.Reply.Token)
			}
			splitReply := func(a, b string) []byte {
				body := replyVector(t, "SplitRangeReply_two_points")
				body = bytes.ReplaceAll(body, []byte("split_a"), []byte(a))
				return bytes.ReplaceAll(body, []byte("split_b"), []byte(b))
			}
			if err := transport.WriteFrame(old.server, initial[0], splitReply("elder_a", "elder_b"), false); err != nil {
				t.Fatal(err)
			}
			select {
			case <-oldRetained:
			case <-ctx.Done():
				t.Fatal("collector did not retain the completed old shard before retry")
			}
			if err := transport.WriteFrame(old.server, initial[1], (&types.ErrorOrError{ErrorCode: uint16(code)}).MarshalFDB(), false); err != nil {
				t.Fatal(err)
			}
			frame, ok := old.recv(ctx)
			if !ok {
				t.Fatal("whole-range relocation missing")
			}
			var loc types.GetKeyServerLocationsRequest
			if err := loc.UnmarshalFDB(frame.body); err != nil {
				t.Fatal(err)
			}
			if frame.token != getAdjustedEndpoint(armToken, EndpointGetKeyServerLocations) || string(loc.Begin) != "a" || !loc.HasEnd || string(loc.End) != "z" {
				t.Fatalf("retry did not relocate the whole range: %+v", loc)
			}
			conn := pooledConn(db, armAddr)
			if conn == nil || conn.HasPendingReply(initial[2]) {
				t.Fatal("retry retained its unanswered old sibling")
			}
			replacement := metricsReplacementLocationsReply(t, [2]string{"a", "m"}, [2]string{"m", "t"}, [2]string{"t", "z"})
			if err := transport.WriteFrame(old.server, replyToken(loc.Reply.Token), replacement, false); err != nil {
				t.Fatal(err)
			}
			requests := make([]transport.UID, 3)
			for i := range 3 {
				frame, ok := fresh.recv(ctx)
				if !ok {
					t.Fatal("retry did not reissue every shard")
				}
				var req types.SplitRangeRequest
				if err := req.UnmarshalFDB(frame.body); err != nil {
					t.Fatal(err)
				}
				if string(req.Keys.Begin) != bounds[i] || string(req.Keys.End) != bounds[i+1] {
					t.Fatalf("fresh range = %+v", req.Keys)
				}
				requests[i] = replyToken(req.Reply.Token)
			}
			for _, i := range []int{2, 1, 0} {
				names := [][2]string{{"fresh_a", "fresh_b"}, {"nresh_a", "nresh_b"}, {"vresh_a", "vresh_b"}}
				if err := transport.WriteFrame(fresh.server, requests[i], splitReply(names[i][0], names[i][1]), false); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("fresh split fanout did not finish")
			}
			want := [][]byte{[]byte("a"), []byte("fresh_a"), []byte("fresh_b"), []byte("m"), []byte("nresh_a"), []byte("nresh_b"), []byte("t"), []byte("vresh_a"), []byte("vresh_b"), []byte("z")}
			if err != nil || !reflect.DeepEqual(points, want) {
				t.Fatalf("fresh points = %q, %v; want %q", points, err, want)
			}
		})
	}
}
