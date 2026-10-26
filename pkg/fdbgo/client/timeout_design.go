package client

import (
	"os"
	"reflect"
	"sync"
	"time"
	"weak"
)

// Per-transaction timeout timer and Transact-scoped timer pool (RFC-282,
// ReadYourWrites.actor.cpp:1567-1578).
var designTimeout = func() bool {
	switch os.Getenv("FDBGO_NET_DESIGN") {
	case "0", "legacy":
		return false
	default:
		return true
	}
}()

type designTimerEntry struct {
	mu       sync.Mutex
	tx       *Transaction
	arm      uint64
	deadline int64
	nextFire int64
	t        *time.Timer
}

var designTimerPool = sync.Pool{
	New: func() any { return &designTimerEntry{} },
}

func (e *designTimerEntry) fire() {
	e.mu.Lock()
	tx := e.tx
	arm := e.arm
	deadline := e.deadline
	if tx == nil {
		e.nextFire = 0
		e.mu.Unlock()
		return
	}
	now := monoNow()
	if rem := deadline - now; rem > 0 {
		e.nextFire = deadline
		if e.t != nil {
			e.t.Reset(time.Duration(rem))
		}
		e.mu.Unlock()
		return
	}
	e.nextFire = 0
	e.mu.Unlock()
	tx.fireTimeout(arm)
}

var (
	monoNowPC     = reflect.ValueOf(monoNow).Pointer()
	fireTimeoutPC = reflect.ValueOf((*Transaction).fireTimeout).Pointer()
)

// production reports whether q is an unmodified database queue. Tests that
// drive a manual clock, timer or hooks keep the queue path they inspect.
func (q *timeoutQueue) production() bool {
	if _, real := q.timer.(*realTimer); !real {
		return false
	}
	r := q.root
	return q.beforeDeliver == nil && q.beforeEagerSchedule == nil && q.compare == nil &&
		r.afterFrontier == nil && r.beforeScan == nil && r.afterScan == nil && r.afterPop == nil &&
		reflect.ValueOf(q.now).Pointer() == monoNowPC && reflect.ValueOf(q.deliver).Pointer() == fireTimeoutPC
}

// designArmLocked takes the arm when D4 applies. Caller holds tx.readErrMu and
// has advanced timeoutArm and set timeoutArmed.
func (tx *Transaction) designArmLocked(q *timeoutQueue, delay time.Duration) bool {
	if !designTimeout || tx.timeoutNode != nil || !q.production() {
		return false
	}
	tx.armDesignTimerLocked(delay)
	return true
}

// armDesignTimerLocked (re)arms the transaction's one timer for its current arm.
func (tx *Transaction) armDesignTimerLocked(delay time.Duration) {
	tx.designArm = tx.timeoutArm
	if tx.transactScoped && tx.designTimer == nil {
		e := tx.designEntry
		if e == nil {
			e = designTimerPool.Get().(*designTimerEntry)
			tx.designEntry = e
		}
		now := monoNow()
		deadline := now + int64(delay)
		e.mu.Lock()
		e.tx = tx
		e.arm = tx.timeoutArm
		e.deadline = deadline
		if e.t == nil {
			e.nextFire = deadline
			e.t = time.AfterFunc(delay, e.fire)
		} else if e.nextFire <= now || deadline < e.nextFire {
			e.nextFire = deadline
			e.t.Reset(delay)
		}
		e.mu.Unlock()
		return
	}
	tx.armDesignWeakTimerLocked(delay)
}

func (tx *Transaction) armDesignWeakTimerLocked(delay time.Duration) {
	tx.designDeadline = monoNow() + int64(delay)
	if tx.designTimer == nil || tx.designWeak.Value() == nil {
		if tx.designTimer != nil {
			tx.designTimer.Stop()
		}
		w := weak.Make(tx)
		tx.designWeak = w
		tx.designTimer = time.AfterFunc(delay, func() {
			if t := w.Value(); t != nil {
				t.fireDesignTimeout()
			}
		})
	} else {
		tx.designTimer.Reset(delay)
	}
}

func (tx *Transaction) fireDesignTimeout() {
	tx.readErrMu.Lock()
	if !tx.timeoutArmed || tx.timeoutArm != tx.designArm {
		tx.readErrMu.Unlock()
		return
	}
	if rem := tx.designDeadline - monoNow(); rem > 0 {
		if tx.designTimer != nil {
			tx.designTimer.Reset(time.Duration(rem))
		}
		tx.readErrMu.Unlock()
		return
	}
	arm := tx.designArm
	tx.readErrMu.Unlock()
	tx.fireTimeout(arm) // rejects a stale or disarmed arm under the same lock
}

// stopDesignTimerLocked releases the timer's reference to tx. Caller holds readErrMu.
func (tx *Transaction) stopDesignTimerLocked() {
	if e := tx.designEntry; e != nil {
		e.mu.Lock()
		e.tx = nil
		e.mu.Unlock()
	}
	if tx.designTimer != nil {
		tx.designTimer.Stop()
	}
}

// releaseTransactTimeout disarms or detaches the pooled timeout timer when
// Database.Transact or Database.ReadTransact exits.
func (tx *Transaction) releaseTransactTimeout() {
	if !designTimeout {
		return
	}
	tx.readErrMu.Lock()
	tx.transactScoped = false
	e := tx.designEntry
	tx.designEntry = nil
	inFlight := tx.hasPendingReadsLocked() ||
		(tx.readLife != nil && (tx.readLife.users > 0 || tx.readLife.issuedReads > 0 ||
			(tx.readLife.versionstamp != nil && !tx.readLife.versionstamp.ready) ||
			len(tx.readLife.versionstamps) > 0))
	if inFlight {
		if e != nil {
			e.mu.Lock()
			deadline := e.deadline
			e.tx = nil
			e.mu.Unlock()
			designTimerPool.Put(e)
			if tx.timeoutArmed {
				tx.armDesignWeakTimerLocked(time.Duration(max(deadline-monoNow(), 0)))
			}
		}
		tx.readErrMu.Unlock()
		return
	}
	if tx.timeoutArmed && tx.timeoutNs.Load() > 0 {
		tx.timeoutNeedsArm = true
	}
	tx.timeoutArmed = false
	if tx.designTimer != nil {
		tx.designTimer.Stop()
	}
	if e != nil {
		e.mu.Lock()
		e.tx = nil
		e.mu.Unlock()
	}
	tx.readErrMu.Unlock()
	if e != nil {
		designTimerPool.Put(e)
	}
}

// timeoutPendingLocked reports whether the current arm is physically pending:
// a queue node, or design D4's timer. Caller holds readErrMu. For tests.
func (tx *Transaction) timeoutPendingLocked() bool {
	return tx.timeoutArmed && (tx.timeoutNode != nil || tx.designTimer != nil || tx.designEntry != nil)
}

// withdrawTimeoutLocked takes the pending arm out of its queue or stops its timer,
// so a test can deliver it by hand. Caller holds readErrMu. For tests.
func (tx *Transaction) withdrawTimeoutLocked() {
	if n := tx.timeoutNode; n != nil {
		n.queue.remove(n)
		return
	}
	tx.stopDesignTimerLocked()
}

// expireTimeoutLocked makes the pending arm due now. Caller holds readErrMu. For tests.
func (tx *Transaction) expireTimeoutLocked() {
	if n := tx.timeoutNode; n != nil {
		n.queue.schedule(n, n.queue.now()-int64(timeoutSlack))
		return
	}
	if e := tx.designEntry; e != nil {
		e.mu.Lock()
		e.deadline = monoNow()
		e.nextFire = e.deadline
		e.t.Reset(0)
		e.mu.Unlock()
		return
	}
	if tx.designTimer != nil {
		tx.designDeadline = monoNow()
		tx.designTimer.Reset(0)
	}
}
