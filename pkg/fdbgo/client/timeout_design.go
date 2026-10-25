package client

import (
	"os"
	"reflect"
	"time"
	"weak"
)

// Design D4 (docs/labs/single-network-goroutine/DESIGN.md), on with
// FDBGO_NET_DESIGN=1|ng|m: a transaction with TIMEOUT set holds one runtime timer
// for its deadline, as C++ holds one timebomb actor (ReadYourWrites.actor.cpp:
// 1567-1578). No weak pointer, cleanup or shared heap entry per arm. The timer
// is stopped when the deadline is disarmed, including when Transact or
// ReadTransact returns. The timer holds the transaction only weakly, so an
// armed transaction that is dropped is collectable, as C++'s timebomb holds
// only the reset promise. The weak handle is made once per transaction, not
// per arm. TIMEOUT unset arms nothing, with or without D4.
var designTimeout = func() bool {
	v := os.Getenv("FDBGO_NET_DESIGN")
	return v == "1" || v == "ng" || v == "m"
}()

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
	if tx.designTimer == nil {
		w := weak.Make(tx)
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
	arm := tx.designArm
	tx.readErrMu.Unlock()
	tx.fireTimeout(arm) // rejects a stale or disarmed arm under the same lock
}

// stopDesignTimerLocked releases the timer's reference to tx. Caller holds readErrMu.
func (tx *Transaction) stopDesignTimerLocked() {
	if tx.designTimer != nil {
		tx.designTimer.Stop()
	}
}

// timeoutPendingLocked reports whether the current arm is physically pending:
// a queue node, or design D4's timer. Caller holds readErrMu. For tests.
func (tx *Transaction) timeoutPendingLocked() bool {
	return tx.timeoutArmed && (tx.timeoutNode != nil || tx.designTimer != nil)
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
	if tx.designTimer != nil {
		tx.designTimer.Reset(0)
	}
}
