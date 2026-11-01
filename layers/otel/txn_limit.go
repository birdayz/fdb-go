package otel

import (
	"context"
	"errors"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

const (
	defaultMaxInFlightTransactions = 32
	maxInFlightTransactionsLimit   = 1024

	// maxQueryRetries bounds fresh calls of a read-only Exporter query after a 1031.
	maxQueryRetries = 3
)

type txnLimiterKey struct{}

// Transaction timeouts run from creation, so they include the GRV wait. Under
// ratekeeper throttling that wait is FDB's backpressure and lasts seconds; the
// span-ingest benchmark's 1.5s bound would turn it into transaction_timed_out
// (measured: GRV 0.5-1.7s at a full storage write queue) instead of latency.
// Exporter transactions therefore get 10s: up to ~5s of GRV queueing plus the
// 5s MVCC window. An export call's own deadline is the Collector's timeout.
const (
	benchTxnTimeout    = 1500 * time.Millisecond
	exporterTxnTimeout = 10 * time.Second
)

type txnTimeoutKey struct{}

// withExporterTxns marks ctx as carrying Exporter transactions: they use
// exporterTxnTimeout, and the ingest guard clock starts after the read version.
func withExporterTxns(ctx context.Context) context.Context {
	return context.WithValue(ctx, txnTimeoutKey{}, exporterTxnTimeout)
}

// txnTimeout is the transaction timeout for work under ctx.
func txnTimeout(ctx context.Context) time.Duration {
	if ctx != nil {
		if d, ok := ctx.Value(txnTimeoutKey{}).(time.Duration); ok {
			return d
		}
	}
	return benchTxnTimeout
}

func isExporterTxn(ctx context.Context) bool {
	return ctx != nil && ctx.Value(txnTimeoutKey{}) != nil
}

// withTxnLimiter attaches an Exporter's write-slot semaphore; nil attaches none.
func withTxnLimiter(ctx context.Context, slots chan struct{}) context.Context {
	if slots == nil {
		return ctx
	}
	return context.WithValue(ctx, txnLimiterKey{}, slots)
}

// runTxnSlot is db.Run holding one of ctx's write slots, if ctx carries a
// limiter. The slot is taken before Run so waiting for it does not count
// against the transaction timeout, and is released when Run returns.
func runTxnSlot(ctx context.Context, db *recordlayer.FDBDatabase, op string, fn func(*recordlayer.FDBRecordContext) (any, error)) (any, error) {
	if slots, _ := ctx.Value(txnLimiterKey{}).(chan struct{}); slots != nil {
		if err := ctx.Err(); err != nil {
			return nil, wrap("canceled", op, err)
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil, wrap("canceled", op, ctx.Err())
		}
		defer func() { <-slots }()
	}
	return db.Run(ctx, fn)
}

// runIdempotentTxn is runTxnSlot with fresh Runs on 1031, which OnError treats
// as terminal and which may have committed. Callers' fn must be an idempotent
// replace that rereads its state (blob chunk 0, deterministic FTS rows and
// metadata, reverse span rows, prior log rows) or writes the same blind sets,
// so redoing a maybe-committed attempt is safe. The slot is free during backoff.
func runIdempotentTxn(ctx context.Context, db *recordlayer.FDBDatabase, op string, fn func(*recordlayer.FDBRecordContext) (any, error)) error {
	var last error
	for run := 0; run < maxSourceRuns; run++ {
		if run > 0 {
			if err := waitSourceBackoff(ctx, sourceRunBackoff(run)); err != nil {
				return wrap("canceled", op, err)
			}
		}
		if _, last = runTxnSlot(ctx, db, op, fn); !isSourceTimeout(last) {
			return last
		}
	}
	return wrap("operation", op, last)
}

// isTransientReadError reports whether a read-only call may succeed on a
// fresh transaction. Besides 1031 these are the read- and GRV-side codes
// OnError retries, which surface once a transaction's retry limit runs out:
// measured, process_behind while storage servers catch up with the first
// write burst on a fresh cluster. Commit-side codes cannot occur on reads.
func isTransientReadError(err error) bool {
	if isSourceTimeout(err) {
		return true
	}
	var code int
	var value fdb.Error
	var pointer *fdb.Error
	switch {
	case errors.As(err, &value):
		code = value.Code
	case errors.As(err, &pointer) && pointer != nil:
		code = pointer.Code
	default:
		return false
	}
	switch code {
	case 1007, // transaction_too_old
		1009, // future_version
		1037, // process_behind
		1038, // database_locked
		1051, // batch_transaction_throttled
		1078, // grv_proxy_memory_limit_exceeded
		1213, // tag_throttled
		1223: // proxy_tag_throttled
		return true
	}
	return false
}

// retryQuery repeats a read-only query call after a transient read error, at
// most maxQueryRetries times; every other outcome is returned unchanged.
func retryQuery[T any](ctx context.Context, op string, call func(context.Context) (T, error)) (T, error) {
	qctx := withExporterTxns(ctx)
	for retry := 0; ; retry++ {
		out, err := call(qctx)
		if !isTransientReadError(err) {
			return out, err
		}
		if retry == maxQueryRetries {
			return out, wrap("operation", op, err)
		}
		if werr := waitSourceBackoff(ctx, sourceRunBackoff(retry+1)); werr != nil {
			return out, wrap("canceled", op, werr)
		}
	}
}
