package otel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

func TestTxnPolicyContext(t *testing.T) {
	t.Parallel()
	if got := txnTimeout(context.Background()); got != benchTxnTimeout || isExporterTxn(context.Background()) {
		t.Fatalf("default timeout %v exporter=%v, want the 1.5s benchmark bound", got, isExporterTxn(context.Background()))
	}
	ctx := withExporterTxns(withTxnLimiter(t.Context(), make(chan struct{}, 1)), 3*time.Second)
	if got := txnTimeout(ctx); got != 3*time.Second || !isExporterTxn(ctx) {
		t.Fatalf("exporter timeout %v exporter=%v", got, isExporterTxn(ctx))
	}
	if _, ok := ctx.Value(txnLimiterKey{}).(chan struct{}); !ok {
		t.Fatal("exporter policy dropped the limiter")
	}
}

func TestExporterTxnError(t *testing.T) {
	t.Parallel()
	live := t.Context()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	admission := fail("admission", "q", "bad span")
	budget := wrap("budget", "q", fdb.Error{Code: 1031})
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		kind string // "" means err is returned unchanged
		code int    // FDB code the result must still carry, 0 for none
	}{
		{"admission kept", live, admission, "", 0},
		{"corruption kept", live, fail("corruption", "q", "bad row"), "", 0},
		{"ownership kept", live, fail("ownership", "q", "not ours"), "", 0},
		{"budget kept", live, budget, "", 1031},
		{"canceled kept", live, wrap("canceled", "q", context.Canceled), "", 0},
		{"timeout", live, fdb.Error{Code: 1031}, "transient", 1031},
		{"timeout pointer", live, &fdb.Error{Code: 1031}, "transient", 1031},
		{"wrapped timeout", live, fmt.Errorf("commit: %w", fdb.Error{Code: 1031}), "transient", 1031},
		{"transaction_too_old", live, fdb.Error{Code: 1007}, "transient", 1007},
		{"not_committed", live, fdb.Error{Code: 1020}, "transient", 1020},
		{"commit_unknown_result", live, fdb.Error{Code: 1021}, "transient", 1021},
		{"transaction_cancelled", live, fdb.Error{Code: 1025}, "transient", 1025},
		{"process_behind", live, fdb.Error{Code: 1037}, "transient", 1037},
		{"proxy_tag_throttled", live, fdb.Error{Code: 1223}, "transient", 1223},
		{"record layer retriable", live, &recordlayer.RecordCoreRetriableTransactionError{Message: "retry"}, "transient", 0},
		{"platform_error", live, fdb.Error{Code: 1500}, "operation", 1500},
		{"transaction_too_large", live, fdb.Error{Code: 2101}, "operation", 2101},
		{"key_too_large", live, fdb.Error{Code: 2102}, "operation", 2102},
		{"internal_error", live, fdb.Error{Code: 4100}, "operation", 4100},
		{"plain error", live, errors.New("boom"), "operation", 0},
		{"deadline error", live, context.DeadlineExceeded, "canceled", 0},
		{"canceled caller, FDB error", canceled, fdb.Error{Code: 1025}, "canceled", 1025},
		{"canceled caller, plain error", canceled, errors.New("boom"), "canceled", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := exporterTxnError(tc.ctx, "op", tc.err)
			if tc.kind == "" {
				if got != tc.err {
					t.Fatalf("domain error changed: %v -> %v", tc.err, got)
				}
			} else {
				requireOtelKind(t, got, tc.kind)
				if !errors.Is(got, tc.err) {
					t.Fatalf("classification lost the cause: %v", got)
				}
			}
			if code, ok := fdbErrorCode(got); tc.code != 0 && (!ok || code != tc.code) {
				t.Fatalf("code %d, %v; want %d", code, ok, tc.code)
			}
			if tc.ctx == canceled && !errors.Is(got, context.Canceled) {
				t.Fatalf("canceled caller missing from %v", got)
			}
		})
	}
	var transient *TransientError
	if !errors.As(exporterTxnError(live, "op", fdb.Error{Code: 1031}), &transient) {
		t.Fatal("timeout is not a TransientError")
	}
	if exporterTxnError(live, "op", nil) != nil {
		t.Fatal("nil error classified")
	}
}

func TestWithTxnLimiterNil(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	if withTxnLimiter(ctx, nil) != ctx {
		t.Fatal("nil slots attached a limiter")
	}
}

// retryInjector fails the first k executions of every transaction (k < 0:
// every execution) with code after the body succeeded, as a commit would.
type retryInjector struct {
	fdb.Database
	code, k         int
	waitForDeadline bool

	mu           sync.Mutex
	transactions int
	minimum      int // fewest executions of a transaction that succeeded
	bodies       int // successful live transaction bodies, including injected failures
}

func (r *retryInjector) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return r.TransactCtx(context.Background(), fn)
}

func (r *retryInjector) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	executions := 0
	result, err := r.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		executions++
		result, err := fn(tx)
		if err == nil {
			r.mu.Lock()
			r.bodies++
			r.mu.Unlock()
		}
		if err == nil && (r.k < 0 || executions <= r.k) {
			if r.waitForDeadline {
				<-ctx.Done()
			}
			return nil, fdb.Error{Code: r.code}
		}
		return result, err
	})
	if err == nil {
		r.mu.Lock()
		if r.transactions == 0 || executions < r.minimum {
			r.minimum = executions
		}
		r.transactions++
		r.mu.Unlock()
	}
	return result, err
}
