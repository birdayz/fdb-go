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

// Exporter timeouts span GRV waits, commit and all retries, including dispatched commits
// that outlive cancellation; keep them within the Collector retry budget.
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

// runTxnSlot acquires a write slot before Run so queueing does not consume its timeout.
// It classifies exporter errors and otherwise preserves Run's error.
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

// exporterTxnError preserves domain kinds and caller cancellation after in-transaction retries.
// FDB normal failures (1000..1499) and Record Layer retriable errors are transient; others are operations.
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
