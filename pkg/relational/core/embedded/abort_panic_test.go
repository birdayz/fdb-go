package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/internal/simgate"
)

// waitClosed waits for the connection to be closed by a goroutine of its own.
func waitClosed(t *testing.T, c *EmbeddedConnection) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for c.IsValid() {
		if time.Now().After(deadline) {
			t.Fatal("the connection was not closed after its transaction's abort panicked")
		}
		time.Sleep(time.Millisecond)
	}
}

// wantClosedAndCancelled asserts the connection is out of service the way a
// recovered panic leaves it, and that releasing the transaction retried the
// FDB cancel the panic interrupted.
func wantClosedAndCancelled(t *testing.T, c *EmbeddedConnection, fdbTx fdb.ReadTransaction) {
	t.Helper()
	if c.IsValid() {
		t.Fatal("the connection is still valid after its abort panicked")
	}
	if _, err := c.ExecContext(context.Background(), "INSERT INTO t (id, v) VALUES (9, 9)", nil); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("statement on the closed connection = %v, want driver.ErrBadConn", err)
	}
	if err := c.ResetSession(context.Background()); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("ResetSession = %v, want driver.ErrBadConn", err)
	}
	if err := c.Close(); err != nil || c.activeTx != nil {
		t.Fatalf("Close = %v, activeTx %v; want nil, nil", err, c.activeTx)
	}
	_, err := fdbTx.Get(fdb.Key("k")).Get()
	var fe fdb.Error
	if !errors.As(err, &fe) || fe.Code != 1025 {
		t.Fatalf("read on the transaction after release = %v, want transaction_cancelled (1025)", err)
	}
}

// A context's abort runs on the context's own goroutine, outside every entry
// point's recover. A panic there (here in the FDB transaction's Cancel) is
// logged and closes the connection instead of crashing the process; releasing
// the transaction cancels it. Not parallel: it captures the seriousLog global.
func TestExplicitTx_AbortPanicClosesConnection(t *testing.T) {
	var mu sync.Mutex
	logged := map[string]int{}
	orig := seriousLog
	seriousLog = func(msg string, attrs ...any) {
		mu.Lock()
		defer mu.Unlock()
		for i := 0; i+1 < len(attrs); i += 2 {
			if attrs[i] == "panic" {
				if s, ok := attrs[i+1].(string); ok {
					logged[s]++
				}
			}
		}
	}
	t.Cleanup(func() {
		seriousLog = orig
		if n := logged["simgate: Cancel"]; n != 2 {
			t.Errorf("recovered Cancel panics logged %d times, want once per subtest (2)", n)
		}
	})
	t.Run("BeginTx context", func(t *testing.T) {
		t.Parallel()
		g := newGatedSim(t, 561)
		c := g.connect(t, "S")
		ctx, cancel := context.WithCancel(context.Background())
		tx, err := c.BeginTx(ctx, driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, c, "INSERT INTO t (id, v) VALUES (800, 800)")
		fdbTx := c.activeTx.rctx.Transaction()
		g.gate.PanicOnce(simgate.Cancel)
		cancel()
		waitClosed(t, c)
		wantEnded(t, "Commit", tx.Commit(), context.Canceled)
		wantClosedAndCancelled(t, c, fdbTx)
		if n := countRows(t, g.connect(t, "S"), 800); n != 0 {
			t.Fatalf("the aborted transaction's write was committed (count %d)", n)
		}
	})
	t.Run("statement context", func(t *testing.T) {
		t.Parallel()
		g := newGatedSim(t, 563)
		c := g.connect(t, "S")
		if _, err := c.BeginTx(context.Background(), driver.TxOptions{}); err != nil {
			t.Fatal(err)
		}
		mustExec(t, c, "INSERT INTO t (id, v) VALUES (801, 801)")
		fdbTx := c.activeTx.rctx.Transaction()
		ctx, cancel := context.WithCancel(context.Background())
		gate := g.gate.BlockNextIn(simgate.Read, fdbTx)
		done := make(chan error, 1)
		go func() {
			rows, err := c.QueryContext(ctx, "SELECT id FROM t", nil)
			if err == nil {
				rows.Close() //nolint:errcheck
			}
			done <- err
		}()
		<-gate.Entered()
		g.gate.PanicOnce(simgate.Cancel)
		cancel()
		waitClosed(t, c)
		gate.Release() // the panicking Cancel did not reach the held read
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("statement whose context ended = %v, want context.Canceled", err)
		}
		wantClosedAndCancelled(t, c, fdbTx)
	})
}

// On the connection's own goroutine (a statement whose context had already
// ended) the same panic reaches the entry point's recoverInto: the statement
// returns the internal error and the connection is poisoned.
func TestExplicitTx_SynchronousAbortPanicIsInternalError(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 565)
	c := g.connect(t, "S")
	if _, err := c.BeginTx(context.Background(), driver.TxOptions{}); err != nil {
		t.Fatal(err)
	}
	fdbTx := c.activeTx.rctx.Transaction()
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	g.gate.PanicOnce(simgate.Cancel)
	_, err := c.ExecContext(ended, "INSERT INTO t (id, v) VALUES (802, 802)", nil)
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeInternalError || !strings.Contains(ae.Message, "internal error") {
		t.Fatalf("statement whose abort panicked = %v, want XX000 internal error", err)
	}
	if c.activeTx != nil {
		t.Fatal("the poisoned connection kept its transaction")
	}
	wantClosedAndCancelled(t, c, fdbTx)
}
