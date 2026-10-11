package client

import (
	"sync"
	"time"
)

// The retained native commit shares the GRV, not the public resetPromise or
// transaction. Reset replaces this state after dropping the old actor.
type readVersionState struct {
	// Lock order: readVersionMu → traceMu, never the reverse.
	readVersionMu      sync.Mutex
	readVersion        int64
	hasReadVersion     bool
	userSetReadVersion bool
	// Total latency spans OnError retries; the MVCC instant belongs to one GRV.
	metricStart        time.Time
	readVersionInstant time.Time
}

// Turnover has drained public state users and cancelled retained native work.
func (tx *Transaction) resetReadVersionState(preserveMetricStart bool) {
	next := &readVersionState{}
	if preserveMetricStart {
		tx.readVersionMu.Lock()
		next.metricStart = tx.metricStart
		tx.readVersionMu.Unlock()
	}
	tx.readErrMu.Lock()
	tx.readVersionState = next
	tx.readErrMu.Unlock()
}
