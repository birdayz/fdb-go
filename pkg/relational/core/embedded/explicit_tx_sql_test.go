package embedded

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/internal/simgate"
)

// gatedConnector opens connections over a gatedSim, so database/sql's own Tx,
// Rows and pool drive the driver.
type gatedConnector struct{ g *gatedSim }

func (c gatedConnector) Connect(context.Context) (driver.Conn, error) { return c.g.open("S") }
func (gatedConnector) Driver() driver.Driver                          { return gatedDriver{} }

type gatedDriver struct{}

func (gatedDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("gatedDriver: open through the connector")
}

// pinGated returns database/sql's *sql.Conn over a gatedSim and its driver
// connection.
func pinGated(t *testing.T, g *gatedSim) (*sql.Conn, *EmbeddedConnection) {
	t.Helper()
	db := sql.OpenDB(gatedConnector{g})
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var ec *EmbeddedConnection
	if err := conn.Raw(func(dc any) error {
		ec = dc.(*EmbeddedConnection)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return conn, ec
}

// Through database/sql a query context that ends between two pages reaches
// the driver as Rows.Close, never as another page: the transaction ends there
// too, so Commit is 25F01 whether the deadline landed during a read or not.
func TestExplicitTx_SQLRowsClosedByEndedContextEndTransaction(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 541)
	conn, ec := pinGated(t, g)
	ctx := context.Background()
	for _, q := range []string{"INSERT INTO t (id, v) VALUES (1, 1)", "INSERT INTO t (id, v) VALUES (2, 2)", "INSERT INTO t (id, v) VALUES (3, 3)"} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	ec.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 1).Build())
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (600, 600)"); err != nil {
		t.Fatal(err)
	}
	qctx, cancel := context.WithCancel(ctx)
	rows, err := tx.QueryContext(qctx, "SELECT id FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("first row: %v", rows.Err())
	}
	cancel()
	rows.Close() // database/sql may already have closed them for the context
	err = tx.Commit()
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeTransactionInactive || !errors.Is(err, context.Canceled) {
		t.Fatalf("Commit after the query's context ended = %v, want 25F01 caused by context.Canceled", err)
	}
	var n int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE id = 600").Scan(&n); err != nil || n != 0 {
		t.Fatalf("count of the ended transaction's write = %d, %v; want 0", n, err)
	}
}

// database/sql rolls a transaction back when its BeginTx context ends, after
// waiting for the statement in flight (Tx.rollback takes closemu). The driver
// interrupts that statement's FDB read, so the rollback completes.
func TestExplicitTx_SQLBeginTxContextUnblocksRollback(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 545)
	conn, ec := pinGated(t, g)
	ctx := context.Background()
	txCtx, cancel := context.WithCancel(ctx)
	tx, err := conn.BeginTx(txCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (602, 602)"); err != nil {
		t.Fatal(err)
	}
	gate := g.gate.BlockNextIn(simgate.Read, ec.activeTx.rctx.Transaction())
	err = runBlocked(t, gate, cancel, func() error {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM t")
		if err == nil {
			rows.Close()
		}
		return err
	})
	if err == nil {
		t.Fatal("the statement interrupted by the BeginTx context succeeded")
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("Commit after database/sql rolled back succeeded")
	}
	other := sql.OpenDB(gatedConnector{g})
	defer other.Close()
	var n int64
	if err := other.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE id = 602").Scan(&n); err != nil || n != 0 {
		t.Fatalf("count of the rolled-back write = %d, %v; want 0", n, err)
	}
}

// The catalog read a statement makes inside the transaction (its first,
// loading the schema) takes the statement's context too.
func TestExplicitTx_StatementContextInterruptsCatalogRead(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 547)
	c := g.connect(t, "S")
	tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	gate := g.gate.BlockNextIn(simgate.Read, c.activeTx.rctx.Transaction())
	err = runBlocked(t, gate, cancel, func() error {
		rows, err := c.QueryContext(ctx, "SELECT id FROM t", nil)
		if err == nil {
			rows.Close() //nolint:errcheck
		}
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("statement interrupted in its catalog read = %v, want context.Canceled", err)
	}
	if len(c.activeTx.schemaCache) != 0 {
		t.Fatal("the schema load completed: the interrupted read was not the catalog read")
	}
	if err := tx.Rollback(); err != nil || c.activeTx != nil {
		t.Fatalf("Rollback = %v, activeTx %v; want nil, nil", err, c.activeTx)
	}
}

// Close and ResetSession detach the BeginTx context binding and wait for an
// abort it already started, so they return with the transaction cancelled
// and released, never while the abort still runs.
func TestExplicitTx_CloseAndResetWaitForRunningAbort(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(*EmbeddedConnection) error{
		"Close":        func(c *EmbeddedConnection) error { return c.Close() },
		"ResetSession": func(c *EmbeddedConnection) error { return c.ResetSession(context.Background()) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGatedSim(t, 549)
			c := g.connect(t, "S")
			txCtx, cancel := context.WithCancel(context.Background())
			if _, err := c.BeginTx(txCtx, driver.TxOptions{}); err != nil {
				t.Fatal(err)
			}
			mustExec(t, c, "INSERT INTO t (id, v) VALUES (603, 603)")
			gate := g.gate.BlockNextIn(simgate.Cancel, c.activeTx.rctx.Transaction())
			cancel()
			<-gate.Entered()
			done := make(chan error, 1)
			go func() { done <- end(c) }()
			select {
			case err := <-done:
				gate.Release()
				t.Fatalf("%s returned (%v) while the context's abort was still cancelling the transaction", name, err)
			case <-time.After(200 * time.Millisecond):
			}
			gate.Release()
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if c.activeTx != nil {
				t.Fatalf("%s left the transaction on the connection", name)
			}
			if n := countRows(t, g.connect(t, "S"), 603); n != 0 {
				t.Fatalf("the aborted transaction's write was committed (count %d)", n)
			}
		})
	}
}

// Commit detaches the BeginTx context before it commits: a context ending
// while the commit is in flight does not touch it, and the commit's own
// outcome stands.
func TestExplicitTx_ContextEndingDuringCommitKeepsItsOutcome(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 551)
	c := g.connect(t, "S")
	txCtx, cancel := context.WithCancel(context.Background())
	tx, err := c.BeginTx(txCtx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (604, 604)")
	gate := g.gate.BlockNextIn(simgate.Commit, c.activeTx.rctx.Transaction())
	done := make(chan error, 1)
	go func() { done <- tx.Commit() }()
	<-gate.Entered()
	cancel()
	// A binding still attached would cancel the transaction now, which also
	// releases the held commit into transaction_cancelled.
	select {
	case err := <-done:
		t.Fatalf("Commit returned (%v) before the held commit was released: the context reached it", err)
	case <-time.After(200 * time.Millisecond):
	}
	gate.Release()
	if err := <-done; err != nil {
		t.Fatalf("Commit while the BeginTx context ended = %v, want the commit's own success", err)
	}
	if n := countRows(t, c, 604); n != 1 {
		t.Fatalf("committed write: count %d, want 1", n)
	}
}
