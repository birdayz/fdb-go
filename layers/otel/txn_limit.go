package otel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

const (
	defaultMaxInFlightTransactions = 32
	maxInFlightTransactionsLimit   = 1024
)

// The span-ingest benchmark's transaction policy: a 1.5s timeout and, for store
// transactions, at most 10 OnError retries (configureStoreContext).
const (
	benchTxnTimeout    = 1500 * time.Millisecond
	benchTxnRetryLimit = 10
)

// Exporter transactions have no retry limit; ExporterConfig.TransactionTimeout
// and the caller's context bound them. A timeout runs from transaction creation
// and spans every OnError retry, so it includes GRV waits, which under
// ratekeeper throttling last seconds (measured: GRV 0.5-1.7s at a full storage
// write queue). 10s covers about 5s of GRV queueing plus FDB's 5s MVCC window.
// It must be set: a dispatched commit outlives the caller's context
// (rfcs/280-client-retry-and-close-bounds.md §4), so the timeout is its only
// bound. Below 1s ordinary GRV backpressure would fail transactions; above 5m,
// the Collector's default retry_on_failure.max_elapsed_time, one transaction
// would outlast a whole retry budget.
const (
	defaultExporterTxnTimeout = 10 * time.Second
	minExporterTxnTimeout     = time.Second
	maxExporterTxnTimeout     = 5 * time.Minute
	// noRetryLimit disables the OnError retry limit (FDB_TR_OPTION_RETRY_LIMIT -1).
	noRetryLimit = -1
)

type txnTimeoutKey struct{}

type txnLimiterKey struct{}

// withExporterTxns marks ctx as carrying Exporter transactions: they run with
// timeout and no retry limit, and the ingest guard does not bound their elapsed time.
func withExporterTxns(ctx context.Context, timeout time.Duration) context.Context {
	return context.WithValue(ctx, txnTimeoutKey{}, timeout)
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
// against the transaction timeout, and is released when Run returns. An
// Exporter transaction's error is classified by exporterTxnError; other
// callers receive Run's error unchanged.
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
	out, err := db.Run(ctx, fn)
	if isExporterTxn(ctx) {
		err = exporterTxnError(ctx, op, err)
	}
	return out, err
}

// exporterTxnError classifies an Exporter transaction's final error for a
// caller deciding whether to retry the call. FDB's OnError loop has already
// retried every retryable code within the transaction timeout, so what is left
// is permanent for this transaction but not necessarily for a new call:
//   - an error with a domain kind keeps it (admission, ownership, corruption,
//     budget, canceled, ...);
//   - an ended caller context is "canceled";
//   - FDB's 1xxx "normal failure" codes (flow/include/flow/error_definitions.h:
//     31-32 at 7.3.77), which include transaction_timed_out (1031) and every
//     code OnError retries, are "transient", as is a Record Layer retriable error;
//   - any other error, including FDB's 15xx platform, 2xxx illegal-operation
//     and 4xxx internal codes (:156, :187-188, :391), is "operation".
func exporterTxnError(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var domain *OtelError
	if errors.As(err, &domain) {
		return err
	}
	if cerr := ctx.Err(); cerr != nil {
		if !errors.Is(err, cerr) {
			err = fmt.Errorf("%w (last error: %w)", cerr, err)
		}
		return wrap("canceled", op, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return wrap("canceled", op, err)
	}
	var retriable *recordlayer.RecordCoreRetriableTransactionError
	if code, ok := fdbErrorCode(err); ok && code >= 1000 && code < 1500 || errors.As(err, &retriable) {
		return wrap("transient", op, err)
	}
	return wrap("operation", op, err)
}

// fdbErrorCode returns the FDB error code err carries, in either form.
func fdbErrorCode(err error) (int, bool) {
	var value fdb.Error
	if errors.As(err, &value) {
		return value.Code, true
	}
	var pointer *fdb.Error
	if errors.As(err, &pointer) && pointer != nil {
		return pointer.Code, true
	}
	return 0, false
}
