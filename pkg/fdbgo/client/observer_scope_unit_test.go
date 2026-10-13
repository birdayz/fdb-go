package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

type scopeObserver struct {
	enabled func(context.Context) bool
	handle  func(context.Context, slog.Record)
}

func (h *scopeObserver) Enabled(ctx context.Context, _ slog.Level) bool { return h.enabled(ctx) }
func (h *scopeObserver) Handle(ctx context.Context, r slog.Record) error {
	if h.handle != nil {
		h.handle(ctx, r)
	}
	return nil
}
func (h *scopeObserver) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *scopeObserver) WithGroup(string) slog.Handler      { return h }

type scopeCallerKey struct{}

func requireScopeCode(t *testing.T, err error, code int) {
	t.Helper()
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != code {
		t.Errorf("error=%v, want FDB %d", err, code)
	}
}

func TestReadOperationBorrowedPreparationKeepsOwner(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"versionstamp", "commit"} {
		t.Run(kind, func(t *testing.T) {
			tx := &Transaction{}
			defer tx.Cancel()
			ctx, release := tx.opContext(context.Background())
			defer release()
			op := tx.readOperation(ctx)
			var finish func()
			if kind == "versionstamp" {
				_, pending, err := tx.GetVersionstampPending(ctx)
				if err != nil || pending == nil {
					t.Fatalf("prepare versionstamp: %v, %v", pending, err)
				}
				finish = func() { tx.Cancel(); _, err := pending.Resolve(); requireScopeCode(t, err, 1025) }
			} else {
				pending := tx.PrepareCommit(ctx)
				finish = func() {
					if err := pending.Resolve(); err != nil {
						t.Errorf("resolve prepared commit: %v", err)
					}
				}
			}
			tx.readErrMu.Lock()
			active, users := op.lease.active, op.inc.users
			tx.readErrMu.Unlock()
			if !active || users != 1 {
				t.Errorf("BORROWED_PREPARATION_RELEASED_OWNER: active=%v users=%d", active, users)
			}
			op.lease.release() // end execution without ending the retained admission
			finish()
		})
	}
}

// Exercise the actual public retry roots, including the disabled Enabled sink.
// The observer's retained children belong only to the public caller, not the
// operation's cleanup or transaction resetPromise.
func TestOnErrorObserverCallerIdentity(t *testing.T) {
	t.Parallel()
	for _, code := range []int{1007, 1042, 1021, 1020} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/enabled=%v", code, enabled), func(t *testing.T) {
				caller, cancel := context.WithCancelCause(context.WithValue(context.Background(), scopeCallerKey{}, "caller value"))
				defer cancel(nil)
				deadline := time.Now().Add(time.Hour)
				caller, stop := context.WithDeadline(caller, deadline)
				defer stop()
				var seen, children []context.Context
				observe := func(ctx context.Context) {
					seen = append(seen, ctx)
					child, stopChild := context.WithCancel(ctx)
					t.Cleanup(stopChild)
					children = append(children, child)
					if ctx != caller {
						t.Errorf("OBSERVER_CALLER_IDENTITY: got %T, want original public caller %T", ctx, caller)
					}
					if got, ok := ctx.Deadline(); !ok || got != deadline || ctx.Value(scopeCallerKey{}) != "caller value" {
						t.Errorf("observer changed caller deadline/value: %v %v", got, ok)
					}
				}
				h := &scopeObserver{enabled: func(ctx context.Context) bool { observe(ctx); return enabled }}
				h.handle = func(ctx context.Context, r slog.Record) {
					observe(ctx)
					attrs := make(map[string]int64)
					r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Int64(); return true })
					if attrs["fdb_error_code"] != int64(code) || attrs["retry_count"] != 1 {
						t.Errorf("retry event lost actual input/count: %v", attrs)
					}
				}
				tx := &Transaction{db: &database{logger: slog.New(h)}}
				defer tx.Cancel()
				tx.SetMaxRetryDelay(0)
				if err := tx.OnError(caller, &wire.FDBError{Code: code}); err != nil {
					t.Fatal(err)
				}
				want := 1
				if enabled {
					want = 3 // explicit guard, slog.Log's Enabled, Handle
				}
				if len(seen) != want {
					t.Fatalf("observed %d callbacks, want %d", len(seen), want)
				}
				tx.Reset()
				tx.Cancel()
				for i, ctx := range append(seen, children...) {
					if ctx.Err() != nil || context.Cause(ctx) != nil {
						t.Errorf("OBSERVER_CALLER_LIFETIME: retained context %d canceled by operation/transaction: %v / %v", i, ctx.Err(), context.Cause(ctx))
					}
				}
				cause := errors.New("public caller cause")
				cancel(cause)
				for _, ctx := range append(seen, children...) {
					<-ctx.Done()
					if ctx.Err() != context.Canceled || context.Cause(ctx) != cause {
						t.Errorf("OBSERVER_CALLER_CAUSE: %v / %v, want caller sentinel/custom cause", ctx.Err(), context.Cause(ctx))
					}
				}
			})
		}
	}
}
