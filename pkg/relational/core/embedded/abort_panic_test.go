package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

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

// wantClosedConnection asserts the connection is out of service the way a
// recovered panic leaves it, and that Close still releases its transaction.
func wantClosedConnection(t *testing.T, c *EmbeddedConnection) {
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
}

// A context's abort runs on the context's own goroutine, outside every entry
// point's recover. A panic there (here in the FDB transaction's Cancel) is
// recovered and closes the connection instead of crashing the process.
func TestExplicitTx_AbortPanicClosesConnection(t *testing.T) {
	t.Parallel()
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
		g.gate.PanicOnce(simgate.Cancel)
		cancel()
		waitClosed(t, c)
		wantEnded(t, "Commit", tx.Commit(), context.Canceled)
		wantClosedConnection(t, c)
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
		ctx, cancel := context.WithCancel(context.Background())
		gate := g.gate.BlockNextIn(simgate.Read, c.activeTx.rctx.Transaction())
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
		wantClosedConnection(t, c)
	})
}
