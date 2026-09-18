package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/session"
)

func TestSystemFilterTypedSchema(t *testing.T) {
	t.Parallel()
	columns := []systemColumn{{"K", "BIGINT"}, {"F", "FLOAT"}, {"D", "DOUBLE"}}
	rows := [][]driver.Value{{int64(1), float64(1e20), float64(1e20)}, {int64(2), nil, nil}}
	for _, tc := range []struct {
		predicate string
		want      [][]driver.Value
	}{
		{"CAST(F AS BIGINT) = 2147483647 AND CAST(D AS INTEGER) = -1", rows[:1]},
		{"K = 2 AND CAST(F AS INTEGER) IS NULL", rows[1:]},
		{"NOT (F = 1.0)", rows[:1]},
		{"F = 1.0", nil},
		{"K = 1 OR F = 1.0", rows[:1]},
	} {
		q := parseSelect(t, "SELECT * FROM T WHERE "+tc.predicate)
		got, err := filterSysRows(context.Background(), nil, rows, columns, "T", q.whereExpr)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v/%v, want %v", tc.predicate, got, err, tc.want)
		}
	}
	for _, rows := range [][][]driver.Value{nil, rows} {
		q := parseSelect(t, "SELECT * FROM T WHERE missing = 1")
		_, err := filterSysRows(context.Background(), nil, rows, columns, "T", q.whereExpr)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedColumn {
			t.Errorf("%d rows: missing-column error must not depend on data: %v", len(rows), err)
		}
	}
	// A truncated row is an ordinal resolution failure, not SQL NULL.
	q := parseSelect(t, "SELECT * FROM T WHERE D IS NULL")
	if got, err := filterSysRows(context.Background(), nil, [][]driver.Value{{int64(1)}}, columns, "T", q.whereExpr); err == nil {
		t.Errorf("bad row layout yielded %v instead of error", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := filterSysRows(ctx, nil, rows, columns, "T", q.whereExpr); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled filter: %v", err)
	}
}

func TestSystemFilterStatementClock(t *testing.T) {
	t.Parallel()
	// A fixed session clock from an actual session, not a per-expression clock.
	conn := &EmbeddedConnection{sess: &session.Session{StatementTime: time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)}}
	instant := conn.statementNow()
	q := parseSelect(t, "SELECT * FROM T WHERE CURRENT_TIMESTAMP = CAST('"+instant.UTC().Format("2006-01-02 15:04:05.999999999")+"' AS TIMESTAMP)")
	rows := [][]driver.Value{{int64(1)}, {int64(2)}}
	got, err := filterSysRows(context.Background(), conn, rows, []systemColumn{{"ID", "BIGINT"}}, "T", q.whereExpr)
	if err != nil || !reflect.DeepEqual(got, rows) {
		t.Fatalf("statement clock: %v/%v, want %v", got, err, rows)
	}
}

// Retain the legacy interpreter's complete truth-table proof against the shared
// expression compiler now used by system tables, including UNKNOWN vs FALSE.
func TestSystemFilterTruthTables(t *testing.T) {
	t.Parallel()
	literals := []string{"TRUE", "FALSE", "CAST(NULL AS BOOLEAN)"}
	truth := []predicates.TriBool{predicates.TriTrue, predicates.TriFalse, predicates.TriUnknown}
	and := [3][3]int{{0, 1, 2}, {1, 1, 1}, {2, 1, 2}}
	or := [3][3]int{{0, 0, 0}, {0, 1, 2}, {0, 2, 2}}
	not := [3]int{1, 0, 2}
	check := func(sql string, want predicates.TriBool) {
		t.Helper()
		q := parseSelect(t, "SELECT * FROM t WHERE "+sql)
		resolver := expr.New(semantic.NewAnalyzer(semantic.NewInMemoryCatalog(), false), semantic.NewScope(nil))
		predicate, err := resolver.WalkPredicate(q.whereExpr.Expression())
		if err != nil {
			t.Fatal(err)
		}
		got, err := predicate.Eval(nil)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v/%v, want %v", sql, got, err, want)
		}
		rows := [][]driver.Value{{int64(1)}}
		filtered, err := filterSysRows(context.Background(), nil, rows, []systemColumn{{"ID", "BIGINT"}}, "T", q.whereExpr)
		wantRows := 0
		if want == predicates.TriTrue {
			wantRows = 1
		}
		if err != nil || len(filtered) != wantRows {
			t.Errorf("%s filter: %v/%v, want %d rows", sql, filtered, err, wantRows)
		}
	}
	for i, a := range literals {
		check("NOT ("+a+")", truth[not[i]])
		for j, b := range literals {
			check("("+a+") AND ("+b+")", truth[and[i][j]])
			check("("+a+") OR ("+b+")", truth[or[i][j]])
		}
	}
}
