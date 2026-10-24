package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// metricsRetryDB opens a database whose connections run through a simDialer and a
// send recorder, with keys under prefix written.
func metricsRetryDB(t *testing.T, ctx context.Context, prefix string) (*Database, *simDialer, *sendRecorder) {
	t.Helper()
	sim, rec := newSimDialer(), &sendRecorder{}
	db := newTestDatabase(t, ctx, startProxyFDB(t, ctx), rec.wrap(sim.dial))
	t.Cleanup(func() { db.Close() })
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := range 50 {
			tx.Set([]byte(fmt.Sprintf("%s%04d", prefix, i)), bytes.Repeat([]byte{'v'}, 1024))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db, sim, rec
}

// Size estimates and split points retry wrong_shard_server past the old 50-attempt
// cap to success, as C++ does (NativeAPI.actor.cpp:7799-7811, 8162, 8204-8208).
func TestMetricsRetryWrongShardWithoutCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	prefix := t.Name() + "/"
	db, sim, rec := metricsRetryDB(t, ctx, prefix)
	begin, end := []byte(prefix), []byte(prefix+"\xff")
	const injections = MaxWrongShardRetries + 5
	for _, tc := range []struct {
		kind string
		run  func(tx *Transaction) error
	}{
		{"WaitMetricsRequest", func(tx *Transaction) error {
			_, err := tx.GetEstimatedRangeSizeBytes(ctx, begin, end)
			return err
		}},
		{"SplitRangeRequest", func(tx *Transaction) error {
			_, err := tx.GetRangeSplitPoints(ctx, begin, end, 1000)
			return err
		}},
	} {
		var injected atomic.Int32
		sim.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
			if rec.replyKind(token) == tc.kind && injected.Add(1) <= injections {
				return (&types.ErrorOrError{ErrorCode: 1001}).MarshalFDB(), false
			}
			return body, false
		})
		sim.armEverything()
		rec.start()
		err := tc.run(db.CreateTransaction())
		rec.stop()
		if err != nil || injected.Load() <= injections {
			t.Fatalf("%s: %v after %d replies; want success after %d wrong_shard_server replies", tc.kind, err, injected.Load(), injections)
		}
	}
}

// A size estimate whose every reply is lost retries without a cap, paced by the
// all-alternatives-failed delay rather than spinning on the 10 ms wrong-shard delay.
func TestMetricsRetryIsPaced(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	prefix := t.Name() + "/"
	db, sim, rec := metricsRetryDB(t, ctx, prefix)
	var rounds atomic.Int32
	sim.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
		if rec.replyKind(token) == "WaitMetricsRequest" {
			rounds.Add(1)
			return severConn, true
		}
		return body, false
	})
	sim.armEverything()
	rec.start()
	defer rec.stop()
	opCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	_, err := db.CreateTransaction().GetEstimatedRangeSizeBytes(opCtx, []byte(prefix), []byte(prefix+"\xff"))
	// At least 45 ms per round; unpaced, a round is the 10 ms delay plus its RPCs.
	t.Logf("%d rounds in 3 s: %v", rounds.Load(), err)
	if n := rounds.Load(); err == nil || n < 2 || n > 40 {
		t.Fatalf("%d rounds in 3 s, %v; want 2 to 40 rounds and the deadline", n, err)
	}
}

// The positive-skip mapped reads re-send a timed-out request at most
// maxReadTimeoutRetries times, then report a retryable transaction_too_old.
func TestMappedSelectorReadsCapReplyTimeouts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sim, rec := newSimDialer(), &sendRecorder{}
	db := newTestDatabase(t, ctx, startProxyFDB(t, ctx), rec.wrap(sim.dial))
	defer db.Close()
	f := newMappedFixture(t, 4)
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := range f.n {
			tx.Set(f.indexEntryKey(i), nil)
			tx.Set(f.recordKey(i), []byte(fmt.Sprintf("record-%d", i)))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	begin, end := f.indexRange()
	for _, tc := range []struct {
		name string
		read func(tx *Transaction, rv int64) error
	}{
		{"selectors", func(tx *Transaction, rv int64) error {
			_, err := tx.getMappedRangeSelectors(ctx, mappedSelectorFGE(begin), mappedSelectorFGE(end), f.getValueMapper(), newMappedRangeLimits(0, ByteLimitUnlimited), rv, false)
			return err
		}},
		{"exact", func(tx *Transaction, rv int64) error {
			_, err := tx.getMappedRangeSelectorExact(ctx, begin, end, f.getValueMapper(), newMappedRangeLimits(0, ByteLimitUnlimited), rv, false)
			return err
		}},
	} {
		tx := db.CreateTransaction()
		tx.rpcTimeoutOverride = 20 * time.Millisecond
		rv, err := tx.GetReadVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.locCache.locateRange(db.db, ctx, begin, end, 100, false, NoTenantID, types.SpanContext{}); err != nil {
			t.Fatal(err)
		}
		var dropped atomic.Int32
		sim.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
			if rec.replyKind(token) == "GetMappedKeyValuesRequest" {
				dropped.Add(1)
				return nil, true
			}
			return body, false
		})
		sim.armEverything()
		rec.start()
		err = tc.read(tx, rv)
		rec.stop()
		var fdbErr *wire.FDBError
		if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionTooOld || dropped.Load() != maxReadTimeoutRetries+1 {
			t.Fatalf("%s: %v after %d lost replies; want transaction_too_old after %d", tc.name, err, dropped.Load(), maxReadTimeoutRetries+1)
		}
	}
}
