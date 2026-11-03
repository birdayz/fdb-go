package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// gatedGet is a point-read seam that answers from values once gate is closed,
// or fails with err, and gives up when its context ends.
type gatedGet struct {
	gate   chan struct{}
	values map[string]string
	err    error
	calls  chan string
}

func newGatedGet(values map[string]string) *gatedGet {
	return &gatedGet{gate: make(chan struct{}), values: values, calls: make(chan string, 16)}
}

func (g *gatedGet) get(ctx context.Context, key []byte) ([]byte, error) {
	g.calls <- string(key)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if g.err != nil {
		return nil, g.err
	}
	v, ok := g.values[string(key)]
	if !ok {
		return nil, nil
	}
	return []byte(v), nil
}

// issuedReadCount is the current incarnation's count in Commit's barrier.
func issuedReadCount(tx *Transaction) int {
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if tx.readLife == nil {
		return 0
	}
	return tx.readLife.issuedReads
}

func startGet(tx *Transaction, key []byte, snapshot bool) *PendingValue {
	if snapshot {
		return tx.Snapshot().StartGet(context.Background(), key)
	}
	return tx.StartGet(context.Background(), key)
}

// A Get reads the transaction as of its call (ThreadSafeTransaction.cpp:426-434): the server's
// value merged with the writes then, not later ones (RYW:1631-1693).
func TestStartGetIsIssuedInTheCall(t *testing.T) {
	t.Parallel()
	later := map[string]func(tx *Transaction){
		"set":    func(tx *Transaction) { tx.Set([]byte("k"), []byte("late")) },
		"clear":  func(tx *Transaction) { tx.Clear([]byte("k")) },
		"atomic": func(tx *Transaction) { tx.Atomic(MutAppendIfFits, []byte("k"), []byte("+late")) },
	}
	for name, mutate := range later {
		for _, snapshot := range []bool{false, true} {
			for _, pendingAtomic := range []bool{false, true} {
				tx := &Transaction{}
				g := newGatedGet(map[string]string{"k": "s"})
				tx.nativeGet = g.get
				want := "s"
				if pendingAtomic {
					tx.Atomic(MutAppendIfFits, []byte("k"), []byte("+a")) // merged over the server's value
					want = "s+a"
				}
				key := []byte("k")
				p := startGet(tx, key, snapshot)
				// Before anything can yield: a Get issued on a goroutine is not in the barrier yet.
				if n := issuedReadCount(tx); n != 1 {
					t.Errorf("%s, snapshot=%v pendingAtomic=%v: %d reads in Commit's barrier at return, want 1", name, snapshot, pendingAtomic, n)
				}
				key[0] = 'x' // the read keeps its own key
				mutate(tx)
				if k := <-g.calls; k != "k" {
					t.Errorf("%s, snapshot=%v pendingAtomic=%v: server read of %q, want \"k\"", name, snapshot, pendingAtomic, k)
				}
				close(g.gate)
				v, err := p.Get()
				if err != nil || string(v) != want {
					t.Errorf("%s after issue, snapshot=%v pendingAtomic=%v: got %q, %v; want %q", name, snapshot, pendingAtomic, v, err, want)
				}
			}
		}
	}
}

// A never-awaited Get does not hold Reset, Cancel or OnError, and fails with
// transaction_cancelled as RYW resets fail reads in flight (RYW:2726-2733).
func TestUnawaitedStartGetEndsWithTheTransaction(t *testing.T) {
	t.Parallel()
	ends := map[string]func(tx *Transaction){
		"reset":   func(tx *Transaction) { tx.Reset() },
		"cancel":  func(tx *Transaction) { tx.Cancel() },
		"onError": func(tx *Transaction) { _ = tx.OnError(context.Background(), &wire.FDBError{Code: 1020}) },
	}
	for name, end := range ends {
		for _, snapshot := range []bool{false, true} {
			tx := &Transaction{}
			tx.backoffJitter = func() float64 { return 0 }
			g := newGatedGet(nil) // never opened: only the transaction's end finishes the read
			tx.nativeGet = g.get
			p := startGet(tx, []byte("k"), snapshot)
			<-g.calls
			done := make(chan struct{})
			go func() {
				end(tx)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Minute):
				t.Fatalf("%s, snapshot=%v: blocked on a started Get nobody awaited", name, snapshot)
			}
			if _, err := p.Get(); issueCode(err) != ErrTransactionCancelled {
				t.Errorf("%s, snapshot=%v: started Get = %v, want transaction_cancelled", name, snapshot, err)
			}
		}
	}
}

// Commit waits for an un-awaited Get and fails with its error, as C++ commit waits on
// `reading` (RYW:1359-1360, 1690-1691).
func TestCommitWaitsForStartedGet(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		tx := &Transaction{}
		g := newGatedGet(map[string]string{"k": "s"})
		if fail {
			g.err = &wire.FDBError{Code: ErrFutureVersion}
		}
		tx.nativeGet = g.get
		waited := false
		tx.beforeIssuedReadWait = func(n int) {
			if n == 1 && !waited {
				waited = true
				close(g.gate) // the commit is waiting for exactly the started Get
			}
		}
		// Commit right after the call: a Get issued on a goroutine may not be in the barrier yet.
		p := tx.Snapshot().StartGet(context.Background(), []byte("k"))
		err := tx.Commit(context.Background())
		if !waited {
			t.Fatalf("fail=%v: Commit returned %v without waiting for the started Get", fail, err)
		}
		if fail {
			if issueCode(err) != ErrFutureVersion {
				t.Fatalf("Commit after a failed started Get = %v, want the read's 1009", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("read-only Commit = %v", err)
		}
		if v, err := p.Get(); err != nil || string(v) != "s" {
			t.Fatalf("started Get after the commit = %q, %v; want \"s\"", v, err)
		}
	}
}

// A panic in a started Get's read fails the read and the commit, as for a range read
// (TestStartGetRange_PanicFailsTheRead), and leaves Commit's barrier.
func TestStartGet_PanicFailsTheRead(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		tx := &Transaction{}
		tx.nativeGet = func(context.Context, []byte) ([]byte, error) { panic("injected") }
		p := startGet(tx, []byte("k"), snapshot)
		if _, err := p.Get(); issueCode(err) != ErrInternalError {
			t.Fatalf("snapshot=%v: read = %v, want internal_error", snapshot, err)
		}
		if n := issuedReadCount(tx); n != 0 {
			t.Fatalf("snapshot=%v: %d reads left in Commit's barrier", snapshot, n)
		}
		if err := tx.Commit(context.Background()); issueCode(err) != ErrInternalError {
			t.Fatalf("snapshot=%v: commit = %v, want the failed read's internal_error", snapshot, err)
		}
	}
}

func issueCode(err error) int {
	var fe *wire.FDBError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return 0
}
