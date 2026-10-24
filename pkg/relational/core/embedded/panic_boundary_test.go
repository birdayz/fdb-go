package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	apiddl "fdb.dev/pkg/relational/api/ddl"
	"fdb.dev/pkg/relational/internal/simgate"
)

// panicStruct is an api.Struct whose every method panics, the shape of a
// broken caller-supplied implementation.
type panicStruct struct{}

func (panicStruct) MetaData() api.StructMetaData { panic("panicStruct.MetaData") }
func (panicStruct) AttributeCount() int          { panic("panicStruct.AttributeCount") }
func (panicStruct) Attribute(int) (any, error)   { panic("panicStruct.Attribute") }
func (panicStruct) AttributeByName(string) (any, error) {
	panic("panicStruct.AttributeByName")
}
func (panicStruct) Attributes() []any { panic("panicStruct.Attributes") }

type countingStatsLogger struct{ n int }

func (l *countingStatsLogger) LogExecutionStats(context.Context, ExecutionStats) { l.n++ }

type panicStatsLogger struct{}

func (panicStatsLogger) LogExecutionStats(context.Context, ExecutionStats) {
	panic("panicStatsLogger")
}

// Every exported entry point that does work turns a panic into the internal
// error (XX000 "internal error") and closes the connection, whose state is
// unknown after it: IsValid reports false so database/sql discards it, and an
// open transaction (DirectAccess's own one included) is cancelled and
// released. ResetSession reports driver.ErrBadConn, the one error database/sql
// acts on there.
func TestExportedEntriesRecoverPanics(t *testing.T) {
	t.Parallel()
	pk := func(id int64) *api.KeySet {
		k, err := api.NewKeySet().SetKeyColumn("ID", id)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	begin := func(t *testing.T, c *EmbeddedConnection) driver.Tx {
		t.Helper()
		tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, c, "INSERT INTO t (id, v) VALUES (7, 7)")
		return tx
	}
	ctx := context.Background()
	cancelPanics := map[string]bool{"Rollback": true, "Close": true, "ResetSession": true}
	for name, call := range map[string]func(*testing.T, *EmbeddedConnection, *simgate.Backend) error{
		"BeginTx": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Begin)
			_, err := c.BeginTx(ctx, driver.TxOptions{})
			return err
		},
		"Commit": func(t *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			tx := begin(t, c)
			b.PanicOnce(simgate.Commit)
			return tx.Commit()
		},
		"Rollback": func(t *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			tx := begin(t, c)
			b.PanicOnce(simgate.Cancel)
			return tx.Rollback()
		},
		"Close": func(t *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			begin(t, c)
			b.PanicOnce(simgate.Cancel)
			return c.Close()
		},
		"ResetSession": func(t *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			begin(t, c)
			b.PanicOnce(simgate.Cancel)
			return c.ResetSession(ctx)
		},
		"Ping": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			return c.Ping(ctx)
		},
		"SetSchema": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			return c.SetSchema("S")
		},
		"PlanExplain": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			_, err := c.PlanExplain(ctx, "SELECT id FROM t")
			return err
		},
		"ExecContext": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			_, err := c.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (1, 1)", nil)
			return err
		},
		"QueryContext": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			_, err := c.QueryContext(ctx, "SELECT id FROM t", nil)
			return err
		},
		"QueryContext metadata observer": func(t *testing.T, c *EmbeddedConnection, _ *simgate.Backend) error {
			stats := &countingStatsLogger{}
			c.SetExecutionStatsLogger(stats)
			observed := api.WithResultSetMetaDataObserver(ctx, func(api.ResultSetMetaData) { panic("observer") })
			rows, err := c.QueryContext(observed, "SELECT id FROM t", nil)
			if rows != nil {
				t.Error("QueryContext returned rows with its recovered panic")
			}
			if stats.n != 1 {
				t.Errorf("the rows made before the panic were not closed (%d execution records)", stats.n)
			}
			return err
		},
		"Rows.Next": func(t *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			for _, sql := range []string{"INSERT INTO t (id, v) VALUES (1, 1)", "INSERT INTO t (id, v) VALUES (2, 2)", "INSERT INTO t (id, v) VALUES (3, 3)"} {
				mustExec(t, c, sql)
			}
			c.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 1).Build())
			rows, err := c.QueryContext(ctx, "SELECT id FROM t", nil)
			if err != nil {
				t.Fatal(err)
			}
			b.PanicOnce(simgate.Read)
			dest := make([]driver.Value, 1)
			for err == nil {
				err = rows.Next(dest)
			}
			return err
		},
		"Rows.Close": func(t *testing.T, c *EmbeddedConnection, _ *simgate.Backend) error {
			c.SetExecutionStatsLogger(panicStatsLogger{})
			rows, err := c.QueryContext(ctx, "SELECT id FROM t", nil)
			if err != nil {
				t.Fatal(err)
			}
			return rows.Close()
		},
		"CollectStatistics": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			_, err := c.CollectStatistics(ctx, recordlayer.CollectOptions{})
			return err
		},
		"ClearStatistics": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			return c.ClearStatistics(ctx)
		},
		"StatisticsStatus": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			_, err := c.StatisticsStatus(ctx)
			return err
		},
		"ApplyMetadataOperation": func(_ *testing.T, c *EmbeddedConnection, _ *simgate.Backend) error {
			return c.ApplyMetadataOperation(ctx, func(apiddl.MetadataOperationsFactory, api.Transaction) error {
				panic("metadata operation")
			})
		},
		"DirectAccess.ExecuteInsert": func(_ *testing.T, c *EmbeddedConnection, _ *simgate.Backend) error {
			_, err := c.DirectAccess().ExecuteInsert(ctx, "T", []api.Struct{panicStruct{}}, nil)
			return err
		},
		"DirectAccess.ExecuteGet": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			_, err := c.DirectAccess().ExecuteGet(ctx, "T", pk(1), nil)
			return err
		},
		"DirectAccess.ExecuteScan": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			_, err := c.DirectAccess().ExecuteScan(ctx, "T", api.EmptyKeySet(), nil)
			return err
		},
		"DirectAccess.ExecuteDelete": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			_, err := c.DirectAccess().ExecuteDelete(ctx, "T", pk(1), nil)
			return err
		},
		"DirectAccess.ExecuteDeleteRange": func(_ *testing.T, c *EmbeddedConnection, b *simgate.Backend) error {
			b.PanicOnce(simgate.Read)
			_, err := c.DirectAccess().ExecuteDeleteRange(ctx, "T", api.EmptyKeySet(), nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGatedSim(t, 521)
			c := g.connect(t, "S")
			err := call(t, c, g.gate)
			if name == "ResetSession" {
				if !errors.Is(err, driver.ErrBadConn) {
					t.Fatalf("ResetSession after a panic = %v, want driver.ErrBadConn", err)
				}
			} else {
				var ae *api.Error
				if !errors.As(err, &ae) || ae.Code != api.ErrCodeInternalError || ae.Message != "internal error" {
					t.Fatalf("%s = %v, want the recovered-panic error (XX000 internal error)", name, err)
				}
			}
			if c.IsValid() {
				t.Fatalf("%s: the connection is still valid after a recovered panic", name)
			}
			if c.activeTx != nil {
				t.Fatalf("%s: a transaction is still open on the poisoned connection", name)
			}
			if _, err := c.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (8, 8)", nil); !errors.Is(err, driver.ErrBadConn) {
				t.Fatalf("%s: a statement on the poisoned connection = %v, want driver.ErrBadConn", name, err)
			}
			// The transaction the panic interrupted was cancelled, unless the
			// panic was its Cancel.
			if open := g.gate.OpenTransactions(); open != 0 && !cancelPanics[name] {
				t.Fatalf("%s: %d transaction(s) left neither committed nor cancelled", name, open)
			}
		})
	}
}

// A panic inside Commit still cancels the FDB transaction: the poisoning runs
// before Commit releases the transaction from the connection.
func TestCommitPanicCancelsTransaction(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 523)
	c := g.connect(t, "S")
	tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (7, 7)")
	fdbTx := c.activeTx.rctx.Transaction()
	g.gate.PanicOnce(simgate.Commit)
	if err := tx.Commit(); err == nil {
		t.Fatal("Commit that panicked succeeded")
	}
	_, err = fdbTx.Get(fdb.Key("k")).Get()
	var fe fdb.Error
	if !errors.As(err, &fe) || fe.Code != 1025 {
		t.Fatalf("read on the transaction after the panicking Commit = %v, want transaction_cancelled (1025)", err)
	}
}
