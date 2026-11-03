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
				var p *PendingValue
				if snapshot {
					p = tx.Snapshot().StartGet(context.Background(), []byte("k"))
				} else {
					p = tx.StartGet(context.Background(), []byte("k"))
				}
				<-g.calls // the server read is under way
				mutate(tx)
				close(g.gate)
				v, err := p.Get()
				if err != nil || string(v) != want {
					t.Errorf("%s after issue, snapshot=%v pendingAtomic=%v: got %q, %v; want %q", name, snapshot, pendingAtomic, v, err, want)
				}
			}
		}
	}
}

// A never-awaited Get releases its lease when its read ends, so Reset (and Commit/OnError) does
// not wait for the caller; C++ runs the read's actor regardless (RYW:1690).
func TestUnawaitedStartGetDoesNotBlockReset(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		tx := &Transaction{}
		g := newGatedGet(nil) // never opened: only the reset ends the read
		tx.nativeGet = g.get
		if snapshot {
			tx.Snapshot().StartGet(context.Background(), []byte("k"))
		} else {
			tx.StartGet(context.Background(), []byte("k"))
		}
		<-g.calls
		done := make(chan struct{})
		go func() {
			tx.Reset()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Fatalf("snapshot=%v: Reset blocked on a started Get nobody awaited", snapshot)
		}
	}
}

// Commit waits for an un-awaited Get and fails with its error, as C++ commit waits on
// `reading` (RYW:1358-1359, 1690-1691).
func TestCommitWaitsForStartedGet(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		tx := &Transaction{}
		g := newGatedGet(map[string]string{"k": "s"})
		if fail {
			g.err = &wire.FDBError{Code: ErrFutureVersion}
		}
		tx.nativeGet = g.get
		p := tx.Snapshot().StartGet(context.Background(), []byte("k"))
		<-g.calls
		tx.beforeIssuedReadWait = func(n int) {
			if n == 1 {
				close(g.gate) // the commit is waiting for exactly the started Get
			}
		}
		err := tx.Commit(context.Background())
		select {
		case <-g.gate:
		default:
			t.Fatalf("fail=%v: Commit returned %v without waiting for the started Get", fail, err)
		}
		var fe *wire.FDBError
		if fail {
			if !errors.As(err, &fe) || fe.Code != ErrFutureVersion {
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
