package embedded

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/internal/simgate"
	"fdb.dev/pkg/simfdb"
)

// gatedSim is a SimFDB database behind a simgate.Backend, bootstrapped with
// /FRL/SIMDB, schema S and table t (id, v).
type gatedSim struct {
	gate *simgate.Backend
	env  *dst.Env
}

func newGatedSim(t *testing.T, seed uint64) *gatedSim {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	g := &gatedSim{gate: simgate.New(simfdb.New(env)), env: env}
	c := g.connect(t, "")
	for _, stmt := range []string{
		"CREATE DATABASE /FRL/simdb",
		"CREATE SCHEMA TEMPLATE tmpl CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA /FRL/simdb/s WITH TEMPLATE tmpl",
	} {
		if _, err := c.ExecContext(context.Background(), stmt, nil); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return g
}

// connect opens a connection over its own FDBDatabase (fresh resolver and
// store-state caches), the way a Connector would.
func (g *gatedSim) connect(t *testing.T, schema string) *EmbeddedConnection {
	t.Helper()
	c, err := g.open(schema)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (g *gatedSim) open(schema string) (*EmbeddedConnection, error) {
	fdbDB := recordlayer.NewFDBDatabaseWithBackend(g.gate).SetEnv(g.env)
	fdbDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		return nil, err
	}
	c := New("/FRL/SIMDB", fdbDB, cat, ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks), ks)
	if schema != "" {
		c.SetDefaultSchema(schema)
	}
	return c, nil
}

func mustExec(t *testing.T, c *EmbeddedConnection, sql string) {
	t.Helper()
	if _, err := c.ExecContext(context.Background(), sql, nil); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// countRows counts the rows of t with id = id in a fresh auto-commit read.
func countRows(t *testing.T, c *EmbeddedConnection, id int64) int64 {
	t.Helper()
	rows, err := c.QueryContext(context.Background(), fmt.Sprintf("SELECT COUNT(*) FROM t WHERE id = %d", id), nil)
	if err != nil {
		t.Fatalf("count %d: %v", id, err)
	}
	defer rows.Close() //nolint:errcheck
	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != nil {
		t.Fatalf("count %d: %v", id, err)
	}
	return dest[0].(int64)
}

// wantEnded asserts err is the 25F01 of a transaction a context ended, with
// the context's error as its cause.
func wantEnded(t *testing.T, what string, err, cause error) {
	t.Helper()
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeTransactionInactive || !errors.Is(err, cause) {
		t.Fatalf("%s = %v, want 25F01 caused by %v", what, err, cause)
	}
}

// runBlocked runs stmt while gate holds its first read in the transaction;
// once the read is blocked it calls end. A statement that never returns
// (the context does not reach the read) fails the test instead of hanging it.
func runBlocked(t *testing.T, gate *simgate.Gate, end func(), stmt func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- stmt() }()
	select {
	case <-gate.Entered():
	case err := <-done:
		t.Fatalf("statement finished (%v) without reading through the transaction", err)
	}
	end()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		gate.Release()
		t.Fatal("the statement's FDB read was not interrupted when its context ended")
		return nil
	}
}

// A statement's context that ends while the statement waits on an FDB read in
// an explicit transaction interrupts the read: the statement returns the
// context's error and the transaction is over (later statements and Commit
// 25F01), its writes discarded. Rollback releases it, and the connection
// opens the next transaction.
func TestExplicitTx_StatementContextInterruptsRead(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 501)
	c := g.connect(t, "S")
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (1, 1)")

	tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (100, 100)")

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
		t.Fatalf("interrupted SELECT = %v, want context.Canceled", err)
	}
	_, err = c.ExecContext(context.Background(), "INSERT INTO t (id, v) VALUES (101, 101)", nil)
	wantEnded(t, "next statement", err, context.Canceled)
	wantEnded(t, "Commit", tx.Commit(), context.Canceled)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if c.activeTx != nil {
		t.Fatal("Rollback left the ended transaction on the connection")
	}
	if n := countRows(t, c, 100); n != 0 {
		t.Fatalf("a write of the ended transaction was committed (count %d)", n)
	}
	next, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx after the ended transaction: %v", err)
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (102, 102)")
	if err := next.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, c, 102); n != 1 {
		t.Fatalf("next transaction's write: count %d, want 1", n)
	}
}

// The BeginTx context is used until the transaction ends: when it ends, a
// statement blocked in an FDB read (running under a context of its own that
// never ends) is interrupted and the transaction is over.
func TestExplicitTx_BeginTxContextInterruptsRead(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 503)
	c := g.connect(t, "S")
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (1, 1)")

	ctx, cancel := context.WithCancel(context.Background())
	tx, err := c.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (200, 200)")
	gate := g.gate.BlockNextIn(simgate.Read, c.activeTx.rctx.Transaction())
	err = runBlocked(t, gate, cancel, func() error {
		rows, err := c.QueryContext(context.Background(), "SELECT id FROM t", nil)
		if err == nil {
			rows.Close() //nolint:errcheck
		}
		return err
	})
	wantEnded(t, "SELECT interrupted by the BeginTx context", err, context.Canceled)
	wantEnded(t, "Commit", tx.Commit(), context.Canceled)
	if err := tx.Rollback(); err != nil || c.activeTx != nil {
		t.Fatalf("Rollback = %v, activeTx %v; want nil, nil", err, c.activeTx)
	}
	if n := countRows(t, c, 200); n != 0 {
		t.Fatalf("a write of the ended transaction was committed (count %d)", n)
	}
}

// Whether a statement's deadline lands during an FDB read or between two
// pages does not decide whether the transaction survives: a page fetched
// after the context ended also ends it.
func TestExplicitTx_ContextEndedBetweenPagesEndsTransaction(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 505)
	c := g.connect(t, "S")
	for i := 0; i < 8; i++ {
		mustExec(t, c, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i))
	}
	c.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 2).Build())
	tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (300, 300)")
	ctx, cancel := context.WithCancel(context.Background())
	rows, err := c.QueryContext(ctx, "SELECT id FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck
	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != nil {
		t.Fatalf("first row: %v", err)
	}
	cancel()
	for err == nil {
		err = rows.Next(dest)
	}
	if err == io.EOF {
		t.Fatal("the result set finished after its context ended: no later page was fetched")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("page after the context ended = %v, want context.Canceled", err)
	}
	wantEnded(t, "Commit", tx.Commit(), context.Canceled)
	if err := tx.Rollback(); err != nil || c.activeTx != nil {
		t.Fatalf("Rollback = %v, activeTx %v; want nil, nil", err, c.activeTx)
	}
	if n := countRows(t, c, 300); n != 0 {
		t.Fatalf("a write of the ended transaction was committed (count %d)", n)
	}
}

// Commit detaches the BeginTx context before committing, so a context ending
// concurrently either ends the transaction before the commit (25F01, nothing
// written) or does not reach it (committed): a commit's outcome is never
// replaced by the context's error.
func TestExplicitTx_CommitRacingBeginTxContext(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 507)
	c := g.connect(t, "S")
	for i := int64(0); i < 40; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		tx, err := c.BeginTx(ctx, driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		id := 1000 + i
		mustExec(t, c, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, 0)", id))
		go cancel()
		err = tx.Commit()
		if c.activeTx != nil {
			t.Fatalf("iteration %d: Commit left the transaction on the connection", i)
		}
		n := countRows(t, c, id)
		switch {
		case err == nil && n == 1:
		case err != nil && n == 0:
			wantEnded(t, fmt.Sprintf("iteration %d: Commit", i), err, context.Canceled)
		default:
			t.Fatalf("iteration %d: Commit = %v but the row count is %d", i, err, n)
		}
		cancel()
	}
}

// A stale transaction handle cannot end the transaction that replaced it on
// the connection: Commit on an ended transaction is 25F01 and commits nothing,
// Rollback on one is a no-op.
func TestStaleTxHandleLeavesNewerTransaction(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 509)
	c := g.connect(t, "S")

	stale, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (400, 400)")
	var ae *api.Error
	if err := stale.Commit(); !errors.As(err, &ae) || ae.Code != api.ErrCodeTransactionInactive {
		t.Fatalf("stale Commit = %v, want 25F01", err)
	}
	if err := stale.Rollback(); err != nil {
		t.Fatalf("stale Rollback = %v, want nil", err)
	}
	if c.activeTx != current.(*embeddedTx) {
		t.Fatal("a stale handle removed the newer transaction from the connection")
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (401, 401)")
	if err := current.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{400, 401} {
		if n := countRows(t, c, id); n != 1 {
			t.Fatalf("row %d of the newer transaction: count %d, want 1", id, n)
		}
	}

	// The same through SQL: COMMIT ends the BeginTx transaction, START
	// TRANSACTION opens the next, and the stale handle's Rollback leaves it.
	stale, err = c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "COMMIT")
	mustExec(t, c, "START TRANSACTION")
	if err := stale.Rollback(); err != nil {
		t.Fatalf("stale Rollback = %v, want nil", err)
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (402, 402)")
	mustExec(t, c, "ROLLBACK")
	if n := countRows(t, c, 402); n != 0 {
		t.Fatalf("row 402 survived ROLLBACK (count %d): the stale Rollback ended the SQL transaction", n)
	}
}

// TxOptions.ReadOnly is refused (0A000) rather than ignored: database/sql
// requires a driver that does not enforce it to say so.
func TestBeginTx_ReadOnlyRefused(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 511)
	c := g.connect(t, "S")
	_, err := c.BeginTx(context.Background(), driver.TxOptions{ReadOnly: true})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeUnsupportedOperation {
		t.Fatalf("BeginTx(ReadOnly) = %v, want 0A000", err)
	}
	if c.activeTx != nil {
		t.Fatal("a refused BeginTx left a transaction on the connection")
	}
	tx, err := c.BeginTx(context.Background(), driver.TxOptions{Isolation: driver.IsolationLevel(sql.LevelSerializable)})
	if err != nil {
		t.Fatalf("BeginTx(serializable): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// BeginTx with a context that has already ended opens nothing; one that is
// live becomes the transaction's record context, which bounds the reads and
// child transactions that take their context from it. START TRANSACTION,
// which outlives its statement, carries none.
func TestBeginTx_RecordContextCarriesBeginTxContext(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 513)
	c := g.connect(t, "S")
	ended, cancelEnded := context.WithCancel(context.Background())
	cancelEnded()
	if _, err := c.BeginTx(ended, driver.TxOptions{}); !errors.Is(err, context.Canceled) || c.activeTx != nil {
		t.Fatalf("BeginTx(ended context) = %v, activeTx %v; want context.Canceled and no transaction", err, c.activeTx)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx, err := c.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.activeTx.rctx.Context(); got != ctx {
		t.Fatalf("record context's Context() = %v, want the BeginTx context", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "START TRANSACTION")
	if got := c.activeTx.rctx.Context(); got != context.Background() {
		t.Fatalf("START TRANSACTION's record context carries %v, want context.Background()", got)
	}
	mustExec(t, c, "ROLLBACK")
}

// A statement whose context ended before it reached FDB (planning checks the
// context first, DirectAccess resolves the schema in a transaction of its own
// first) also ends the explicit transaction, through each entry point.
func TestExplicitTx_ContextEndedBeforeFDBEndsTransaction(t *testing.T) {
	t.Parallel()
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	// DirectAccess runs on a connection that has not resolved the schema yet,
	// so the ended context fails that resolution, before the transaction.
	for name, stmt := range map[string]func(*EmbeddedConnection) error{
		"QueryContext": func(c *EmbeddedConnection) error {
			_, err := c.QueryContext(ended, "SELECT id FROM t", nil)
			return err
		},
		"ExecContext": func(c *EmbeddedConnection) error {
			_, err := c.ExecContext(ended, "INSERT INTO t (id, v) VALUES (501, 501)", nil)
			return err
		},
		"DirectAccess": func(c *EmbeddedConnection) error {
			key, err := api.NewKeySet().SetKeyColumn("ID", int64(500))
			if err != nil {
				return err
			}
			_, err = c.DirectAccess().ExecuteGet(ended, "T", key, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGatedSim(t, 515)
			c := g.connect(t, "S")
			tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if name != "DirectAccess" {
				mustExec(t, c, "INSERT INTO t (id, v) VALUES (500, 500)")
			}
			if err := stmt(c); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s with an ended context = %v, want context.Canceled", name, err)
			}
			wantEnded(t, "Commit", tx.Commit(), context.Canceled)
			if err := tx.Rollback(); err != nil || c.activeTx != nil {
				t.Fatalf("Rollback = %v, activeTx %v; want nil, nil", err, c.activeTx)
			}
			if n := countRows(t, c, 500); n != 0 {
				t.Fatalf("a write of the ended transaction was committed (count %d)", n)
			}
		})
	}
}

// A result set read to its end has completed its statement: closing it after
// its context ended leaves the transaction alone, whether it ended by
// exhaustion or at MAX_ROWS.
func TestExplicitTx_ResultSetReadToEndKeepsTransaction(t *testing.T) {
	t.Parallel()
	for name, maxRows := range map[string]int{"exhausted": 0, "MAX_ROWS": 1} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGatedSim(t, 517)
			c := g.connect(t, "S")
			mustExec(t, c, "INSERT INTO t (id, v) VALUES (1, 1)")
			mustExec(t, c, "INSERT INTO t (id, v) VALUES (2, 2)")
			if maxRows > 0 {
				if err := c.SetOption(api.OptMaxRows, maxRows); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, c, "INSERT INTO t (id, v) VALUES (700, 700)")
			ctx, cancel := context.WithCancel(context.Background())
			rows, err := c.QueryContext(ctx, "SELECT id FROM t", nil)
			if err != nil {
				t.Fatal(err)
			}
			dest := make([]driver.Value, 1)
			n := 0
			for err = rows.Next(dest); err == nil; err = rows.Next(dest) {
				n++
			}
			if err != io.EOF || (maxRows > 0 && n != maxRows) {
				t.Fatalf("read %d rows ending with %v, want io.EOF (MAX_ROWS %d)", n, err, maxRows)
			}
			cancel()
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit after a result set read to its end: %v", err)
			}
			if n := countRows(t, c, 700); n != 1 {
				t.Fatalf("committed write: count %d, want 1", n)
			}
		})
	}
}
