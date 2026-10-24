package sqltest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// loopValuer's Value returns itself.
type loopValuer struct{}

func (v loopValuer) Value() (driver.Value, error) { return v, nil }

type countingStats struct{ n int }

func (c *countingStats) LogExecutionStats(context.Context, embedded.ExecutionStats) { c.n++ }

func apiCode(err error) api.ErrorCode {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func countIDs(t *testing.T, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64,
) int64 {
	t.Helper()
	var n int64
	if err := q.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM Item WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("count %d: %v", id, err)
	}
	return n
}

// The database/sql surface of the driver over real FDB: explicit-transaction
// contexts, TxOptions.ReadOnly, per-borrow settings, the parser's nesting
// guard and parameter typing.
func TestFDB_DriverRobustness(t *testing.T) {
	t.Parallel()
	db := testkit.SetupErrorDB(t, "/FRL/testdb_driver_robust", "drvrobust",
		"CREATE TABLE Item (id BIGINT, label STRING, PRIMARY KEY (id))")
	ctx := context.Background()
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO Item VALUES (1, 'one'), (2, 'two'), (3, 'three')")

	// A statement whose context ends inside an explicit transaction ends the
	// transaction: Commit reports 25F01 and the transaction's earlier write
	// is not committed. The statement timeout is such a context.
	t.Run("statement timeout ends the explicit transaction", func(t *testing.T) {
		conn := testkit.PinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO Item VALUES (100, 'in tx')"); err != nil {
			t.Fatal(err)
		}
		if err := conn.Raw(func(dc any) error {
			dc.(*embedded.EmbeddedConnection).SetStatementTimeout(time.Nanosecond)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.QueryContext(ctx, "SELECT id FROM Item")
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		if apiCode(err) != api.ErrCodeExecutionLimitReached {
			t.Fatalf("SELECT past the statement timeout = %v, want 54F01", err)
		}
		if err := tx.Commit(); apiCode(err) != api.ErrCodeTransactionInactive || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Commit after the timed-out statement = %v, want 25F01 caused by the deadline", err)
		}
		if n := countIDs(t, db, 100); n != 0 {
			t.Fatalf("the ended transaction's write was committed (count %d)", n)
		}
		if err := conn.Raw(func(dc any) error {
			dc.(*embedded.EmbeddedConnection).SetStatementTimeout(0)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		tx, err = conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx on the same connection afterwards: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	})

	// The BeginTx context is used until the transaction ends: once it ends,
	// database/sql rolls the transaction back and its write is gone.
	t.Run("BeginTx context ends the transaction", func(t *testing.T) {
		txCtx, cancel := context.WithCancel(ctx)
		tx, err := db.BeginTx(txCtx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO Item VALUES (101, 'in tx')"); err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := tx.Commit(); err == nil {
			t.Fatal("Commit after the BeginTx context ended succeeded")
		}
		if n := countIDs(t, db, 101); n != 0 {
			t.Fatalf("the ended transaction's write was committed (count %d)", n)
		}
	})

	t.Run("read-only transactions are refused", func(t *testing.T) {
		if _, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); apiCode(err) != api.ErrCodeUnsupportedOperation {
			t.Fatalf("BeginTx(ReadOnly) = %v, want 0A000", err)
		}
	})

	// Settings a borrower changes through Conn.Raw last that borrow: the next
	// borrower of the pooled connection runs without them.
	t.Run("per-borrow settings reset with the borrow", func(t *testing.T) {
		pool := testkit.SetupErrorDB(t, "/FRL/testdb_driver_robust_pool", "drvrobustpool",
			"CREATE TABLE Item (id BIGINT, label STRING, PRIMARY KEY (id))")
		pool.SetMaxOpenConns(1)
		testkit.MustExecCtx(t, pool, ctx, "INSERT INTO Item VALUES (1, 'one'), (2, 'two')")
		stats := &countingStats{}
		conn, err := pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Raw(func(dc any) error {
			ec := dc.(*embedded.EmbeddedConnection)
			ec.SetStatementTimeout(time.Nanosecond)
			ec.SetMaxResultBytes(1)
			ec.SetExecutionStatsLogger(stats)
			return ec.SetOption(api.OptMaxRows, 1)
		}); err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		// The one pooled connection, borrowed again.
		rows, err := pool.QueryContext(ctx, "SELECT id FROM Item")
		if err != nil {
			t.Fatalf("next borrower: %v (the previous borrower's settings reached it)", err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("next borrower: %v (the previous borrower's settings reached it)", err)
		}
		rows.Close()
		if n != 2 {
			t.Fatalf("next borrower read %d rows, want 2 (MAX_ROWS of the previous borrow?)", n)
		}
		if stats.n != 0 {
			t.Fatalf("the previous borrower's stats logger saw %d of the next borrower's statements", stats.n)
		}
	})

	// The nesting guard counts parenthesis tokens: parentheses inside a
	// string literal are text.
	t.Run("parentheses inside a literal are not nesting", func(t *testing.T) {
		label := strings.Repeat("(", 600)
		if _, err := db.ExecContext(ctx, "INSERT INTO Item VALUES (102, '"+label+"')"); err != nil {
			t.Fatalf("INSERT of a literal with 600 parentheses: %v", err)
		}
		var id int64
		if err := db.QueryRowContext(ctx, "SELECT id FROM Item WHERE label = '"+label+"'").Scan(&id); err != nil || id != 102 {
			t.Fatalf("SELECT by that literal: id %d, err %v; want 102", id, err)
		}
		values := make([]string, 0, 600)
		for i := 0; i < 600; i++ {
			values = append(values, "(1)")
		}
		if _, err := db.ExecContext(ctx, "SELECT id FROM Item WHERE id IN ("+strings.Join(values, ",")+")"); err != nil &&
			strings.Contains(err.Error(), "nesting exceeds") {
			t.Fatalf("600 sibling parentheses were counted as nesting: %v", err)
		}
		deep := strings.Repeat("(", 501) + "1" + strings.Repeat(")", 501)
		if _, err := db.ExecContext(ctx, "SELECT "+deep+" FROM Item"); apiCode(err) != api.ErrCodeSyntaxError ||
			!strings.Contains(err.Error(), "nesting exceeds") {
			t.Fatalf("501 nested parentheses = %v, want the nesting error (42601)", err)
		}
	})

	t.Run("a self-returning Valuer is refused", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id FROM Item WHERE id = ?", loopValuer{})
		if apiCode(err) != api.ErrCodeInvalidParameter {
			t.Fatalf("self-returning Valuer = %v, want 22023", err)
		}
	})
}
