package client

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Lab switch (docs/labs/single-network-goroutine): FDBGO_EXP_TIMEOUT_LAZY=1 arms a
// far TIMEOUT in two stages. The first holds the transaction strongly in a
// sharded list for at most lazyTimeoutPeriod; Transact/ReadTransact disarm a
// finished transaction, so a short transaction never builds the weak timeout
// node or touches the heap. A sweep promotes still-armed transactions to the
// ordinary queue, long before their deadline (delay > 2 periods).
var timeoutLazy = os.Getenv("FDBGO_EXP_TIMEOUT_LAZY") == "1"

// FDBGO_EXP_TIMEOUT_LAZY_MS sets the first stage's length (default 20 ms).
var lazyTimeoutPeriod = func() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("FDBGO_EXP_TIMEOUT_LAZY_MS")); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return 20 * time.Millisecond
}()

type lazyTimeoutEntry struct {
	tx       *Transaction
	arm      uint64
	deadline int64
}

type lazyTimeoutShard struct {
	mu      sync.Mutex
	list    []lazyTimeoutEntry
	running bool
}

var (
	lazyShards [16]lazyTimeoutShard
	lazyNext   atomic.Uint32
)

// lazyArmLocked reports whether it took the arm. Caller holds tx.readErrMu and
// has already advanced timeoutArm and set timeoutArmed.
func (tx *Transaction) lazyArmLocked(deadline int64, delay time.Duration) bool {
	if !timeoutLazy || tx.timeoutNode != nil || delay <= 2*lazyTimeoutPeriod {
		return false
	}
	s := &lazyShards[lazyNext.Add(1)%uint32(len(lazyShards))]
	s.mu.Lock()
	s.list = append(s.list, lazyTimeoutEntry{tx: tx, arm: tx.timeoutArm, deadline: deadline})
	start := !s.running
	s.running = true
	s.mu.Unlock()
	if start {
		time.AfterFunc(lazyTimeoutPeriod, s.sweep)
	}
	return true
}

func (s *lazyTimeoutShard) sweep() {
	s.mu.Lock()
	list := s.list
	s.list = nil
	s.mu.Unlock()
	for i := range list {
		e := &list[i]
		tx := e.tx
		tx.readErrMu.Lock()
		if tx.timeoutArmed && tx.timeoutArm == e.arm && tx.timeoutNode == nil {
			n := tx.timeoutQueue().newNode(tx)
			tx.timeoutNode = n
			n.queue.schedule(n, e.deadline)
		}
		tx.readErrMu.Unlock()
		*e = lazyTimeoutEntry{}
	}
	s.mu.Lock()
	if len(s.list) == 0 {
		s.running = false
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	time.AfterFunc(lazyTimeoutPeriod, s.sweep)
}

// finishTimeoutLab disarms a transaction Transact has finished with.
func (tx *Transaction) finishTimeoutLab() {
	if !timeoutLazy && !designTimeout {
		return
	}
	tx.readErrMu.Lock()
	tx.disarmTimeoutLocked()
	tx.readErrMu.Unlock()
}
