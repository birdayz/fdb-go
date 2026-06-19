package client

import (
	"context"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// Metric-op (getRangeSplitPoints / getEstimatedRangeSizeBytes) early-return precedence — RFC-126.
// These ops bypass ensureReadVersion (so they don't get its checkTimeout), and libfdb_c constructs a
// KeyRangeRef (inverted_range) before the op and checks resetPromise (cancel/timeout) before the maxKey
// check. So the order must be: inverted (2005) → cancelled (1025) → timed_out (1031) → maxKey (2004).
// Deterministic (forces the timeout state; the synchronous checks return before the locate loop, so no
// DB is needed).
func TestMetricOps_EarlyReturnPrecedence(t *testing.T) {
	t.Parallel()
	timedOut := func() *Transaction {
		tx := newTestTx()
		tx.timeout = 1
		tx.deadline = time.Now().Add(-time.Second) // already expired → checkTimeout returns 1031
		return tx
	}

	// getRangeSplitPoints: timed-out AND key past maxReadKey → transaction_timed_out (1031) wins over
	// key_outside_legal_range (2004) — C++ resetPromise check precedes the maxKey check.
	_, err := timedOut().getRangeSplitPointsImpl(context.Background(), []byte("a"), []byte("\xff\xff\xff"), 1000)
	if got := crCode(t, err); got != ErrTransactionTimedOut {
		t.Errorf("getRangeSplitPoints timed-out+>maxKey: code=%d, want %d (timeout beats maxKey)", got, ErrTransactionTimedOut)
	}
	// Inverted still beats timeout (KeyRangeRef construction precedes the op entirely).
	_, err = timedOut().getRangeSplitPointsImpl(context.Background(), []byte("z"), []byte("a"), 1000)
	if got := crCode(t, err); got != ErrInvertedRange {
		t.Errorf("getRangeSplitPoints inverted+timed-out: code=%d, want %d (inverted beats timeout)", got, ErrInvertedRange)
	}
	// getEstimatedRangeSizeBytes (no maxKey check): timed-out → 1031; inverted → 2005.
	_, err = timedOut().getEstimatedRangeSizeBytesImpl(context.Background(), []byte("a"), []byte("b"))
	if got := crCode(t, err); got != ErrTransactionTimedOut {
		t.Errorf("getEstimatedRangeSizeBytes timed-out: code=%d, want %d", got, ErrTransactionTimedOut)
	}
	_, err = timedOut().getEstimatedRangeSizeBytesImpl(context.Background(), []byte("z"), []byte("a"))
	if got := crCode(t, err); got != ErrInvertedRange {
		t.Errorf("getEstimatedRangeSizeBytes inverted+timed-out: code=%d, want %d", got, ErrInvertedRange)
	}

	// Poison (client_invalid_operation 2000, RFC-059) out-ranks the timeout — same order as
	// ensureReadVersion (rywPoisonErr before checkTimeout). A txn that is BOTH poisoned AND timed out
	// returns 2000, not 1031 (codex catch).
	const clientInvalidOperation = 2000
	poisonedAndTimedOut := func() *Transaction {
		tx := timedOut()
		tx.rywPoisonErr = &wire.FDBError{Code: clientInvalidOperation}
		return tx
	}
	_, err = poisonedAndTimedOut().getRangeSplitPointsImpl(context.Background(), []byte("a"), []byte("\xff\xff\xff"), 1000)
	if got := crCode(t, err); got != clientInvalidOperation {
		t.Errorf("getRangeSplitPoints poisoned+timed-out: code=%d, want %d (poison beats timeout)", got, clientInvalidOperation)
	}
	_, err = poisonedAndTimedOut().getEstimatedRangeSizeBytesImpl(context.Background(), []byte("a"), []byte("b"))
	if got := crCode(t, err); got != clientInvalidOperation {
		t.Errorf("getEstimatedRangeSizeBytes poisoned+timed-out: code=%d, want %d (poison beats timeout)", got, clientInvalidOperation)
	}
}
