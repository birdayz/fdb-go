package client

import (
	"context"
	"errors"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// Design D4 (RFC-282): one runtime timer per transaction, nothing when TIMEOUT is unset.

func TestDesignTimeoutOneTimerPerTransaction(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	ctx, release := tx.opContext(context.Background())
	defer release()
	tx.readErrMu.Lock()
	tx.timeoutArm++
	tx.timeoutArmed = true
	tx.armDesignTimerLocked(time.Hour)
	first := tx.designTimer
	tx.timeoutArm++ // a re-arm, as SetTimeout or a reset makes
	tx.armDesignTimerLocked(time.Millisecond)
	reused := tx.designTimer == first && first != nil
	tx.readErrMu.Unlock()
	if !reused {
		t.Fatal("a re-arm created a second timer")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var fdbErr *wire.FDBError
		if errors.As(tx.readEntryError(ctx), &fdbErr) && fdbErr.Code == 1031 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the transaction's timer never delivered transaction_timed_out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDesignTimeoutStaleArmIsRejected(t *testing.T) {
	t.Parallel()
	tx := &Transaction{creationTime: time.Now()}
	defer tx.Cancel()
	ctx, release := tx.opContext(context.Background())
	defer release()
	tx.SetTimeout(60000)
	tx.SetTimeout(0) // disarm via SetTimeout(0)
	tx.fireDesignTimeout()
	if err := tx.readEntryError(ctx); err != nil {
		t.Fatalf("a disarmed timer poisoned the transaction: %v", err)
	}

	// Re-arm with a future deadline: fireDesignTimeout re-arms for the remaining
	// budget without delivering 1031.
	tx.SetTimeout(60000)
	tx.fireDesignTimeout()
	if err := tx.readEntryError(ctx); err != nil {
		t.Fatalf("an in-flight callback from an earlier arm poisoned a re-armed transaction: %v", err)
	}

	// Reset also disarms the old generation and re-arms the new one with a future deadline.
	release()
	tx.Reset()
	ctx2, release2 := tx.opContext(context.Background())
	defer release2()
	tx.SetTimeout(60000)
	tx.fireDesignTimeout()
	if err := tx.readEntryError(ctx2); err != nil {
		t.Fatalf("an in-flight callback after Reset poisoned the new generation: %v", err)
	}

	// Once designDeadline has elapsed, fireDesignTimeout delivers 1031.
	tx.readErrMu.Lock()
	tx.designDeadline = monoNow()
	tx.readErrMu.Unlock()
	tx.fireDesignTimeout()
	var fdbErr *wire.FDBError
	if err := tx.readEntryError(ctx2); !errors.As(err, &fdbErr) || fdbErr.Code != 1031 {
		t.Fatalf("expired weak timer fire = %v, want FDBError 1031", err)
	}
}

// The timer holds the transaction weakly: an armed transaction that is
// dropped is collectable before its deadline.
func TestDesignTimeoutDoesNotRetainTransaction(t *testing.T) {
	t.Parallel()
	arm := func() weak.Pointer[Transaction] {
		tx := &Transaction{}
		tx.readErrMu.Lock()
		tx.timeoutArm++
		tx.timeoutArmed = true
		tx.armDesignTimerLocked(time.Hour) // still armed when dropped
		tx.readErrMu.Unlock()
		return weak.Make(tx)
	}
	w := arm()
	for i := 0; i < 20 && w.Value() != nil; i++ {
		runtime.GC()
	}
	if w.Value() != nil {
		t.Fatal("an armed timer kept a dropped transaction alive")
	}
}

func TestDesignTimeoutUnsetArmsNothing(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	tx.SetTimeout(0)
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if tx.designTimer != nil || tx.timeoutNode != nil || tx.timeoutArmed {
		t.Fatal("TIMEOUT unset armed a timer")
	}
}

// The production check recognizes an unmodified database queue and rejects
// a queue that a test drives by hand.
func TestDesignTimeoutProductionQueue(t *testing.T) {
	t.Parallel()
	if !newTimeoutQueue().production() {
		t.Fatal("an unmodified queue is not recognized")
	}
	q := newTimeoutQueue()
	q.now = func() int64 { return 0 } // a hand-driven clock
	if q.production() {
		t.Fatal("a manual queue was taken for the production one")
	}
}

func TestDesignTimeoutTransactScopedPoolAndExpiry(t *testing.T) {
	t.Parallel()
	db := &Database{db: &database{timeouts: newTimeoutQueue()}}
	var observedTx *Transaction
	var attempts int
	_, err := db.Transact(context.Background(), func(tx *Transaction) (any, error) {
		observedTx = tx
		attempts++
		tx.SetTimeout(30000)
		tx.readErrMu.Lock()
		hasPooled := tx.transactScoped && tx.designEntry != nil && tx.timeoutNode == nil && tx.designTimer == nil
		tx.readErrMu.Unlock()
		if !hasPooled {
			t.Fatal("Database.Transact with TIMEOUT did not select a pooled designTimerEntry")
		}
		if attempts == 1 {
			return nil, &wire.FDBError{Code: ErrNotCommitted}
		}
		return "ok", nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("Database.Transact = %v after %d attempts, want nil after 2", err, attempts)
	}
	observedTx.readErrMu.Lock()
	releasedAfterTransact := !observedTx.transactScoped && observedTx.designEntry == nil && !observedTx.timeoutArmed
	observedTx.readErrMu.Unlock()
	if !releasedAfterTransact {
		t.Fatal("Database.Transact did not release its pooled designTimerEntry on exit")
	}

	var observedReadTx *Transaction
	_, err = db.ReadTransact(context.Background(), func(tx *Transaction) (any, error) {
		observedReadTx = tx
		tx.SetTimeout(30000)
		tx.readErrMu.Lock()
		hasPooled := tx.transactScoped && tx.designEntry != nil && tx.timeoutNode == nil && tx.designTimer == nil
		tx.readErrMu.Unlock()
		if !hasPooled {
			t.Fatal("Database.ReadTransact with TIMEOUT did not select a pooled designTimerEntry")
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Database.ReadTransact = %v", err)
	}
	observedReadTx.readErrMu.Lock()
	releasedAfterReadTransact := !observedReadTx.transactScoped && observedReadTx.designEntry == nil && !observedReadTx.timeoutArmed && observedReadTx.timeoutNeedsArm
	observedReadTx.readErrMu.Unlock()
	if !releasedAfterReadTransact {
		t.Fatal("Database.ReadTransact did not release its pooled designTimerEntry and preserve timeoutNeedsArm on exit")
	}

	// 1. Escaped active operation (users > 0, issuedReads == 0) hands off to weak timer.
	var inFlightTx *Transaction
	var inFlightCtx context.Context
	var inFlightRelease context.CancelFunc
	_, err = db.ReadTransact(context.Background(), func(tx *Transaction) (any, error) {
		inFlightTx = tx
		tx.SetTimeout(30000)
		inFlightCtx, inFlightRelease = tx.opContext(context.Background())
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Database.ReadTransact with in-flight op = %v", err)
	}
	defer inFlightRelease()
	inFlightTx.readErrMu.Lock()
	handedOffToWeak := !inFlightTx.transactScoped && inFlightTx.designEntry == nil && inFlightTx.designTimer != nil && inFlightTx.timeoutArmed
	inFlightTx.expireTimeoutLocked()
	inFlightTx.readErrMu.Unlock()
	if !handedOffToWeak {
		t.Fatal("ReadTransact exit with in-flight operation did not hand timeout off to weak timer")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var fdbErr *wire.FDBError
		if errors.As(inFlightTx.readEntryError(inFlightCtx), &fdbErr) && fdbErr.Code == 1031 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handed-off weak timer never delivered transaction_timed_out to escaped in-flight operation")
		}
		time.Sleep(time.Millisecond)
	}

	// Set up a pipe-backed database whose server accepts frames and withholds replies,
	// so real StartGetRange and GetPipelined calls remain in flight across ReadTransact exit.
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go func() {
		if _, err := transport.ReadConnectPacket(serverConn); err != nil {
			return
		}
		pkt := transport.ConnectPacket{ProtocolVersion: transport.ProtocolVersion73, ConnectionID: 262}
		if _, err := serverConn.Write(pkt.Marshal()); err != nil {
			return
		}
		for {
			if _, _, err := transport.ReadFrame(serverConn, false); err != nil {
				return
			}
		}
	}()
	const pipeAddr = "127.0.0.1:4599"
	pipeToken := transport.UID{First: 0x4599, Second: 1}
	pipeTransportConn, err := transport.Dial(context.Background(), pipeAddr, nil, func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	})
	if err != nil {
		t.Fatalf("transport.Dial pipe: %v", err)
	}
	defer pipeTransportConn.Close()
	pipeDB := &Database{db: &database{
		ctx:        context.Background(),
		connPool:   map[string]*transport.Conn{pipeAddr: pipeTransportConn},
		dialing:    make(map[string]*dialCall),
		failMon:    newFailureMonitor(),
		queueModel: newQueueModel(),
	}}
	pipeDB.db.locCache.maxSize = 16
	pipeDB.db.locCache.insertRanges([]locationEntry{{
		tenantId: NoTenantID,
		begin:    []byte{},
		end:      []byte{0xff},
		servers:  []ServerInfo{{Address: pipeAddr, Token: pipeToken}},
	}})

	// 2. Escaped StartGetRange under borrowed opContext (issuedReads == 1, users == 0).
	var issuedTx *Transaction
	var escapedRange *PendingRange
	rangeEntered, releaseRange := make(chan struct{}), make(chan struct{})
	openRange := sync.OnceFunc(func() { close(releaseRange) })
	defer openRange()
	_, err = pipeDB.ReadTransact(context.Background(), func(tx *Transaction) (any, error) {
		issuedTx = tx
		tx.SetTimeout(3600000)
		tx.readVersion = 1
		tx.hasReadVersion = true
		tx.beforeRangeGoroutine = func() {
			close(rangeEntered)
			<-releaseRange
		}
		opCtx, opRelease := tx.opContext(context.Background())
		escapedRange = tx.StartGetRange(opCtx, []byte("a"), []byte("z"), 0, 0, false)
		opRelease()
		return "ok", nil
	})
	if err != nil || escapedRange == nil {
		t.Fatalf("Database.ReadTransact with StartGetRange = (%v, %v)", escapedRange, err)
	}
	select {
	case <-rangeEntered:
	case <-escapedRange.done:
		_, _, earlyErr := escapedRange.Get()
		t.Fatalf("StartGetRange completed before entering background goroutine: %v", earlyErr)
	case <-time.After(5 * time.Second):
		t.Fatal("StartGetRange did not enter background goroutine within 5s")
	}
	issuedTx.readErrMu.Lock()
	issuedHandedOff := !issuedTx.transactScoped && issuedTx.designEntry == nil && issuedTx.designTimer != nil && issuedTx.timeoutArmed && issuedTx.readLife.users == 0 && issuedTx.readLife.issuedReads == 1
	issuedTx.expireTimeoutLocked()
	issuedTx.readErrMu.Unlock()
	openRange()
	if !issuedHandedOff {
		t.Fatal("ReadTransact exit with issuedReads > 0 (users == 0) did not hand timeout off to weak timer")
	}
	_, _, rangeErr := escapedRange.Get()
	var rangeFDBErr *wire.FDBError
	if !errors.As(rangeErr, &rangeFDBErr) || rangeFDBErr.Code != 1031 {
		t.Fatalf("escaped StartGetRange.Get() = %v, want 1031", rangeErr)
	}

	// 3. Escaped GetPipelined (hasPendingReadsLocked() == true, users == 0, issuedReads == 0).
	var pipeTx *Transaction
	var escapedGet *PendingGet
	_, err = pipeDB.ReadTransact(context.Background(), func(tx *Transaction) (any, error) {
		pipeTx = tx
		tx.SetTimeout(30000)
		tx.readVersion = 1
		tx.hasReadVersion = true
		var getErr error
		_, escapedGet, getErr = tx.GetPipelined(context.Background(), []byte("k"))
		return nil, getErr
	})
	if err != nil || escapedGet == nil {
		t.Fatalf("ReadTransact with GetPipelined = (%v, %v)", escapedGet, err)
	}
	pipeTx.readErrMu.Lock()
	pipeHandedOff := !pipeTx.transactScoped && pipeTx.designEntry == nil && pipeTx.designTimer != nil && pipeTx.timeoutArmed && pipeTx.readLife.users == 0 && pipeTx.readLife.issuedReads == 0 && pipeTx.hasPendingReadsLocked()
	pipeTx.expireTimeoutLocked()
	pipeTx.readErrMu.Unlock()
	if !pipeHandedOff {
		t.Fatal("ReadTransact exit with PendingGet did not hand timeout off to weak timer")
	}
	_, getResolveErr := escapedGet.Resolve()
	var getFDBErr *wire.FDBError
	if !errors.As(getResolveErr, &getFDBErr) || getFDBErr.Code != 1031 {
		t.Fatalf("escaped PendingGet.Resolve() = %v, want 1031", getResolveErr)
	}
	if escapedGet.replyHandle != nil {
		t.Fatal("escaped PendingGet did not retire its replyHandle on timeout")
	}

	// 4. Escaped pending versionstamp (users == 0, issuedReads == 0) hands off to weak timer.
	var vsTx *Transaction
	var pendingVs *PendingVersionstamp
	_, err = db.ReadTransact(context.Background(), func(tx *Transaction) (any, error) {
		vsTx = tx
		tx.SetTimeout(30000)
		var vsErr error
		_, pendingVs, vsErr = tx.GetVersionstampPending(context.Background())
		return nil, vsErr
	})
	if err != nil || pendingVs == nil {
		t.Fatalf("Database.ReadTransact with GetVersionstampPending = (%v, %v)", pendingVs, err)
	}
	vsTx.readErrMu.Lock()
	vsHandedOff := !vsTx.transactScoped && vsTx.designEntry == nil && vsTx.designTimer != nil && vsTx.timeoutArmed && vsTx.readLife.users == 0 && vsTx.readLife.issuedReads == 0
	vsTx.expireTimeoutLocked()
	vsTx.readErrMu.Unlock()
	if !vsHandedOff {
		t.Fatal("ReadTransact exit with pending versionstamp (users == 0, issuedReads == 0) did not hand timeout off to weak timer")
	}
	_, vsResolveErr := pendingVs.Resolve()
	var vsFDBErr *wire.FDBError
	if !errors.As(vsResolveErr, &vsFDBErr) || vsFDBErr.Code != 1031 {
		t.Fatalf("escaped pending versionstamp Resolve = %v, want 1031", vsResolveErr)
	}

	tx := db.createTransaction(true)
	defer tx.releaseTransactTimeout()
	ctx, release := tx.opContext(context.Background())
	defer release()
	tx.SetTimeout(30000)
	tx.SetTimeout(1) // tighten to 1 ms -> resets pooled timer
	deadline = time.Now().Add(5 * time.Second)
	for {
		var fdbErr *wire.FDBError
		if errors.As(tx.readEntryError(ctx), &fdbErr) && fdbErr.Code == 1031 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("transactScoped pooled timer never delivered transaction_timed_out")
		}
		time.Sleep(time.Millisecond)
	}
}

// Pooled designTimerEntry reuse: an early fire from a prior owner re-arms for
// the new owner's remaining budget without delivering a false 1031.
func TestDesignTimeoutPooledReuseEarlyFireReArmsWithoutFalseTimeout(t *testing.T) {
	t.Parallel()
	// Use a dedicated entry owned by this test so parallel tests pulling from
	// designTimerPool cannot race with our deterministic fire() calls.
	entry := &designTimerEntry{}
	txA := &Transaction{transactScoped: true, designEntry: entry}
	txA.readErrMu.Lock()
	txA.timeoutArm = 1
	txA.timeoutArmed = true
	txA.armDesignTimerLocked(10 * time.Minute)
	txA.stopDesignTimerLocked()
	txA.designEntry = nil
	txA.readErrMu.Unlock()
	if entry.t != nil {
		defer entry.t.Stop()
	}

	// Firing while disarmed (tx == nil) is a no-op and clears nextFire.
	entry.fire()
	entry.mu.Lock()
	if entry.tx != nil || entry.nextFire != 0 {
		entry.mu.Unlock()
		t.Fatalf("disarmed fire left tx=%v nextFire=%d, want nil/0", entry.tx, entry.nextFire)
	}
	// Simulate an entry whose underlying timer is still scheduled for an earlier
	// deadline (10 minutes from now).
	earlyFire := monoNow() + int64(10*time.Minute)
	entry.nextFire = earlyFire
	entry.mu.Unlock()

	txB := &Transaction{transactScoped: true, designEntry: entry}
	defer func() {
		txB.readErrMu.Lock()
		txB.stopDesignTimerLocked()
		txB.designEntry = nil
		txB.readErrMu.Unlock()
	}()
	ctx, release := txB.opContext(context.Background())
	defer release()
	txB.readErrMu.Lock()
	txB.timeoutArm = 1
	txB.timeoutArmed = true
	txB.armDesignTimerLocked(time.Hour) // deadline > earlyFire -> skips Reset
	txB.readErrMu.Unlock()

	entry.mu.Lock()
	skippedReset := entry.nextFire == earlyFire
	wantDeadline := entry.deadline
	entry.mu.Unlock()
	if !skippedReset {
		t.Fatalf("expected armDesignTimerLocked to keep nextFire=%d, got %d", earlyFire, entry.nextFire)
	}

	// Deliver the early fire while txB's deadline is still ~1h in the future.
	entry.fire()
	if err := txB.readEntryError(ctx); err != nil {
		t.Fatalf("early fire from previous owner falsely timed out txB: %v", err)
	}
	entry.mu.Lock()
	rearmed := entry.nextFire == wantDeadline
	// Now advance txB's deadline to the present and fire again.
	entry.deadline = monoNow()
	entry.mu.Unlock()
	if !rearmed {
		t.Fatalf("early fire did not re-arm nextFire to %d", wantDeadline)
	}

	entry.fire()
	var fdbErr *wire.FDBError
	if err := txB.readEntryError(ctx); !errors.As(err, &fdbErr) || fdbErr.Code != 1031 {
		t.Fatalf("expired pooled timer fire = %v, want FDBError 1031", err)
	}

	// A standalone transaction whose weak identity was cleared (as after finalizer
	// resurrection) renews its weak timer callback on Reset/SetTimeout and delivers 1031.
	txR := &Transaction{creationTime: time.Now()}
	defer txR.Cancel()
	txR.SetTimeout(60000)
	txR.readErrMu.Lock()
	oldTimer := txR.designTimer
	txR.designWeak = weak.Pointer[Transaction]{} // cleared weak identity after resurrection
	txR.readErrMu.Unlock()
	if oldTimer == nil {
		t.Fatal("initial SetTimeout did not arm designTimer")
	}
	defer oldTimer.Stop()
	txR.Reset()
	ctxR, releaseR := txR.opContext(context.Background())
	defer releaseR()
	txR.SetTimeout(1)
	txR.readErrMu.Lock()
	renewed := txR.designTimer != nil && txR.designTimer != oldTimer && txR.designWeak.Value() == txR
	txR.readErrMu.Unlock()
	if !renewed {
		t.Fatal("Reset/SetTimeout did not renew cleared designWeak and replace designTimer")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var rErr *wire.FDBError
		if errors.As(txR.readEntryError(ctxR), &rErr) && rErr.Code == 1031 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resurrected standalone transaction never received 1031 after Reset/SetTimeout")
		}
		time.Sleep(time.Millisecond)
	}
}
