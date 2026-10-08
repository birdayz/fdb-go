package testkit

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func MustExecCtx(t *testing.T, db execer, ctx context.Context, query string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// mwjoInsertRange inserts row(i) for i in [lo, hi] as multi-row INSERTs of 100.
// For fixtures whose test reads only the loaded rows: one autocommit statement
// per row cost these probes most of their runtime. 500-row statements hit the
// 5s transaction limit (1007) on an overloaded box; 100 matches the other
// batched fixtures here.
func MwjoInsertRange(t *testing.T, db execer, ctx context.Context, table string, lo, hi int, row func(i int) string) {
	t.Helper()
	const chunk = 100
	vals := make([]string, 0, chunk)
	for i := lo; i <= hi; i++ {
		vals = append(vals, row(i))
		if len(vals) == chunk || i == hi {
			MustExecCtx(t, db, ctx, "INSERT INTO "+table+" VALUES "+strings.Join(vals, ", "))
			vals = vals[:0]
		}
	}
}

func Explainer(t *testing.T, db *sql.DB, ctx context.Context) func(string) string {
	return func(query string) string {
		t.Helper()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("db.Conn: %v", err)
		}
		defer conn.Close()
		var plan string
		if err := conn.Raw(func(driverConn any) error {
			ec, ok := driverConn.(*embedded.EmbeddedConnection)
			if !ok {
				t.Fatalf("expected *embedded.EmbeddedConnection, got %T", driverConn)
			}
			p, err := ec.PlanExplain(ctx, query)
			if err != nil {
				return err
			}
			plan = p
			return nil
		}); err != nil {
			t.Fatalf("PlanExplain(%q): %v", query, err)
		}
		return plan
	}
}
