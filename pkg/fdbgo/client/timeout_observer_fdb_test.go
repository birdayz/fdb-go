package client

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire/types"
)

type timeoutObserverTimer struct{}

func (timeoutObserverTimer) request(time.Duration) {}

func timeoutObserverDatabase(t *testing.T, ctx context.Context) (*Database, *timeoutQueue, *atomic.Int64) {
	t.Helper()
	db := openTestDB(t, ctx)
	q, clock := attachTimeoutObserverQueue(t, db)
	return db, q, clock
}

func attachTimeoutObserverQueue(t *testing.T, db *Database) (*timeoutQueue, *atomic.Int64) {
	t.Helper()
	original := db.db.timeouts
	original.root.mu.Lock()
	active := original.root.running
	if !active {
		active = len(original.root.ready) != 0
	}
	original.root.mu.Unlock()
	for _, shard := range original.root.shards {
		shard.mu.Lock()
		active = active || len(shard.heap) != 0 || shard.running || shard.requested
		shard.mu.Unlock()
	}
	if active {
		t.Fatal("cannot replace an active database timeout queue")
	}
	clock := &atomic.Int64{}
	clock.Store(int64(time.Hour))
	q := newTimeoutQueue()
	for _, shard := range q.root.shards {
		shard.now = clock.Load
		shard.timer = timeoutObserverTimer{}
	}
	// No test transaction exists yet, so no arm can retain the original queue.
	db.db.timeouts = q
	return q, clock
}

func fireTimeoutObserverSuccessor(t *testing.T, tx *Transaction, old *readIncarnation, q *timeoutQueue, clock *atomic.Int64) {
	t.Helper()
	tx.readErrMu.Lock()
	successor, node, armed := tx.readLife, tx.timeoutNode, tx.timeoutArmed
	if successor == nil || successor == old || successor.cause != nil || !armed || node == nil || node.queue.root != q.root {
		tx.readErrMu.Unlock()
		t.Fatal("successful Commit did not publish a clean, eager, armed successor")
	}
	node.queue.mu.Lock()
	queued := node.index >= 0 && node.index < len(node.queue.heap) && node.queue.heap[node.index] == node
	deadline := node.deadline
	node.queue.mu.Unlock()
	tx.readErrMu.Unlock()
	if !queued {
		t.Fatal("successor timeout was not registered in the manual queue")
	}
	clock.Store(deadline + int64(timeoutSlack))
	q.fire()
	tx.readErrMu.Lock()
	current, cause, armed := tx.readLife, successor.cause, tx.timeoutArmed
	tx.readErrMu.Unlock()
	if current != successor || armed || fdbCodeOf(cause) != ErrTransactionTimedOut {
		t.Fatalf("successor delivery: current=%v armed=%v cause=%v", current == successor, armed, cause)
	}
	if code := fdbCodeOf(context.Cause(successor.ctx)); code != ErrTransactionTimedOut {
		t.Fatalf("successor cancellation code=%d, want %d", code, ErrTransactionTimedOut)
	}
}

func TestTimeoutSuccessorPendingGet(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
	defer cancel()
	db, q, clock := timeoutObserverDatabase(t, ctx)
	key, value := []byte(t.Name()+"/read"), []byte("server value")
	if _, err := db.Transact(ctx, func(seed *Transaction) (any, error) {
		seed.Set(key, value)
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.SetTimeout(time.Hour.Milliseconds())
	got, pending, err := tx.GetPipelined(ctx, key)
	if err != nil || got != nil || pending == nil {
		t.Fatalf("production pipelined read: value=%q pending=%v err=%v", got, pending != nil, err)
	}
	old := tx.readOperation(pending.ctx).inc
	if pending.settledFlag.Load() {
		t.Fatal("pipelined read settled before Commit's drain")
	}
	tx.Set([]byte(t.Name()+"/write"), []byte("committed"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("write Commit draining pre-issued read: %v", err)
	}
	pending.mu.Lock()
	settled, memo, memoErr := pending.done, pending.memoVal, pending.memoErr
	pending.mu.Unlock()
	if !settled || !pending.settledFlag.Load() || memoErr != nil || !bytes.Equal(memo, value) {
		t.Fatalf("Commit did not drain read: settled=%v value=%q err=%v", settled, memo, memoErr)
	}
	fireTimeoutObserverSuccessor(t, tx, old, q, clock)
	// Successful Commit already settled this production future. This pins its
	// memo, not the later op.inc branch: Resolve's settled fast path bypasses it.
	for range 2 {
		got, err := pending.Resolve()
		if err != nil || !bytes.Equal(got, value) {
			t.Fatalf("old pipelined memo after successor timeout: value=%q err=%v", got, err)
		}
	}
	if _, err := tx.GetReadVersion(ctx); fdbCodeOf(err) != ErrTransactionTimedOut {
		t.Fatalf("first successor admission: %v", err)
	}
}

func TestTimeoutSuccessorVersionstampObservers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
	defer cancel()
	db, q, clock := timeoutObserverDatabase(t, ctx)
	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.SetTimeout(time.Hour.Milliseconds())
	value, pending, err := tx.GetVersionstampPending(ctx)
	if err != nil || value != nil || pending == nil {
		t.Fatalf("pre-Commit stamp: value=%x pending=%v err=%v", value, pending != nil, err)
	}
	defer pending.cleanup()
	old := pending.completion.inc
	key := []byte(t.Name())
	tx.Atomic(MutSetVersionstampedValue, key, make([]byte, 14))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("versionstamped write Commit: %v", err)
	}
	stamp, err := tx.GetVersionstamp()
	if err != nil || len(stamp) != 10 {
		t.Fatalf("new getter before successor timeout: stamp=%x err=%v", stamp, err)
	}
	fireTimeoutObserverSuccessor(t, tx, old, q, clock)
	// This is the old future's first Resolve, after the successor has failed.
	got, err := pending.Resolve()
	if err != nil || !bytes.Equal(got, stamp) {
		t.Fatalf("old committed stamp after successor timeout: stamp=%x want=%x err=%v", got, stamp, err)
	}
	if value, err := tx.GetVersionstamp(); value != nil || fdbCodeOf(err) != ErrTransactionTimedOut {
		t.Fatalf("new getter after successor timeout: stamp=%x err=%v", value, err)
	}
	assertIndependentRead(t, ctx, db, key, stamp)
}

func TestTimeoutSuccessorCommittedWatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
	defer cancel()
	recorder := &sendRecorder{}
	db := newTestDatabase(t, ctx, startProxyFDB(t, ctx), recorder.wrap(plainDial))
	defer db.Close()
	q, clock := attachTimeoutObserverQueue(t, db)
	key := []byte(t.Name() + "/watch")
	if _, err := db.Transact(ctx, func(seed *Transaction) (any, error) {
		seed.Set(key, []byte("before"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed watch key: %v", err)
	}
	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.SetTimeout(time.Hour.Milliseconds())
	value, readVersion, span, watchCtx, watchCancel, err := tx.WatchSetup(ctx, key)
	if err != nil {
		t.Fatalf("WatchSetup: %v", err)
	}
	defer watchCancel()
	pollStarted := false
	defer func() {
		if !pollStarted {
			tx.ReleaseWatch()
		}
	}()
	activation := tx.WatchActivationFor(watchCtx)
	if activation == nil || !bytes.Equal(value, []byte("before")) {
		t.Fatalf("captured watch setup: activation=%v value=%q", activation != nil, value)
	}
	tx.readErrMu.Lock()
	old := tx.readLife
	tx.readErrMu.Unlock()
	tx.Set([]byte(t.Name()+"/write"), []byte("committed"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("write Commit activating watch: %v", err)
	}
	version, err := tx.AwaitWatchCommit(watchCtx, activation, readVersion)
	if err != nil || version <= 0 {
		t.Fatalf("captured committed watch activation: version=%d err=%v", version, err)
	}
	recorder.start()
	pollStarted = true
	pollDone := make(chan error, 1)
	go func() { pollDone <- tx.WatchPoll(watchCtx, watchCancel, key, value, version, span) }()
	pollFinished := false
	defer func() {
		watchCancel()
		if !pollFinished {
			select {
			case <-pollDone:
			case <-time.After(5 * time.Second):
				t.Error("watch poll did not drain during cleanup")
			}
		}
	}()
	waitForRecorded(t, ctx, recorder, "WatchValueRequest")
	recorded := recorder.stop()
	if problems := recorded.readProblems(); len(problems) != 0 {
		t.Fatalf("watch request recording: %v", problems)
	}
	requests := 0
	for _, message := range recorded.msgs {
		if request, ok := message.(*types.WatchValueRequest); ok {
			requests++
			if !bytes.Equal(request.Key, key) || !request.HasValue || !bytes.Equal(request.Value, value) || request.Version != version {
				t.Fatalf("watch request did not preserve captured setup: %+v", request)
			}
		}
	}
	if requests == 0 {
		t.Fatal("no native watch request was written before successor timeout")
	}
	select {
	case err := <-pollDone:
		pollFinished = true
		t.Fatalf("watch completed before successor timeout or key change: %v", err)
	default:
	}
	fireTimeoutObserverSuccessor(t, tx, old, q, clock)
	if err := watchCtx.Err(); err != nil {
		t.Fatalf("successor timeout canceled committed watch: %v", err)
	}
	if tx.WatchActivationFor(watchCtx) != activation {
		t.Fatal("committed watch rebound to the successor activation")
	}
	select {
	case err := <-pollDone:
		pollFinished = true
		t.Fatalf("successor timeout completed the committed watch before key change: %v", err)
	default:
	}
	if _, err := db.Transact(ctx, func(writer *Transaction) (any, error) {
		writer.Set(key, []byte("after"))
		return nil, nil
	}); err != nil {
		t.Fatalf("independent watch-key change: %v", err)
	}
	select {
	case err := <-pollDone:
		pollFinished = true
		if err != nil {
			t.Fatalf("committed watch after successor timeout: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := db.db.outstandingWatches.Load(); got != 0 {
		t.Fatalf("completed watch retained %d slots", got)
	}
	if _, err := tx.GetReadVersion(ctx); fdbCodeOf(err) != ErrTransactionTimedOut {
		t.Fatalf("first successor admission: %v", err)
	}
}
