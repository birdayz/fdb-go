package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_PostAggregateOuterReference executes, on real FDB, an aggregate
// block whose computed output or HAVING reads an ENCLOSING block's value — an
// outer field, an AT ordinal, a scalar element. The value is constant across
// the aggregated rows, so it is composable from the aggregate output as it
// stands (the constantCorrelations arm of Java's
// SemanticAnalyzer.isComposableFrom) and pulls up unchanged. Each row set is
// the Java target's (conformance/ws_f_join_unnest_conformance_test.go
// measures them), except the scalar subquery in a select list, which the
// target's grammar lacks and Go answers as a read-side extension.
//
// Before, every such reference was refused (0AF00 "outside the aggregate
// output contract"), and in a grouped block the name-based coverage check
// called `w.f` ungrouped (42803) because the local h also declares F, and a
// field h lacks undefined (42703). A LOCAL non-grouping reference — including
// one whose source shadows the outer name — is still Java's GROUPING_ERROR.
func TestFDB_PostAggregateOuterReference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/post_aggregate_outer_reference"
	setup := testkit.OpenDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE post_aggregate_outer_reference_tmpl" +
			" CREATE TABLE w (id BIGINT, f BIGINT, arr BIGINT ARRAY, g BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE post_aggregate_outer_reference_tmpl",
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+testkit.ClusterFile()+"&schema=MAIN")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO w VALUES (1, 1, [10, 11], 5), (2, 2, [20], 6), (3, 3, [], 7)",
		"INSERT INTO h VALUES (1, 10)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	for _, tc := range []struct {
		sql     string
		want    []string
		wantErr api.ErrorCode
	}{
		{sql: `SELECT d.c FROM w, (SELECT COUNT(*) + w.f AS c FROM h) AS d`, want: []string{"[2]", "[3]", "[4]"}},
		{sql: `SELECT d.c FROM w, w.arr AS v AT p, (SELECT COUNT(*) + p AS c FROM h) AS d`, want: []string{"[2]", "[2]", "[3]"}},
		{sql: `SELECT d.c FROM w, w.arr AS v, (SELECT MAX(h.f) - v AS c FROM h) AS d`, want: []string{"[-10]", "[-1]", "[0]"}},
		{sql: `SELECT d.c FROM w, (SELECT COUNT(*) AS c FROM h HAVING COUNT(*) < w.f) AS d`, want: []string{"[1]", "[1]"}},
		{sql: `SELECT d.k, d.c FROM w, (SELECT h.id AS k, SUM(h.f) + w.f AS c FROM h GROUP BY h.id) AS d`, want: []string{"[1 11]", "[1 12]", "[1 13]"}},
		{sql: `SELECT d.k, d.c FROM w, (SELECT h.id AS k, SUM(h.f) + w.g AS c FROM h GROUP BY h.id) AS d`, want: []string{"[1 15]", "[1 16]", "[1 17]"}},
		{sql: `SELECT d.k FROM w, (SELECT h.id AS k FROM h GROUP BY h.id HAVING COUNT(*) < w.f) AS d`, want: []string{"[1]", "[1]"}},
		{sql: `SELECT d.k, d.c FROM w, (SELECT h.f AS k, SUM(h.id) * w.f AS c FROM h GROUP BY h.f) AS d`, want: []string{"[10 1]", "[10 2]", "[10 3]"}},
		{sql: `SELECT w.id, (SELECT COUNT(*) + w.f FROM h) FROM w`, want: []string{"[1 2]", "[2 3]", "[3 4]"}},
		// An outer value below the aggregate: an operand, a grouping key.
		{sql: `SELECT d.s FROM w, (SELECT SUM(h.f * w.f) AS s FROM h) AS d`, want: []string{"[10]", "[20]", "[30]"}},
		{sql: `SELECT d.k, d.s FROM w, (SELECT h.f + w.f AS k, COUNT(*) AS s FROM h GROUP BY h.f + w.f) AS d`, want: []string{"[11 1]", "[12 1]", "[13 1]"}},
		// Local non-grouping references stay GROUPING_ERROR.
		{sql: `SELECT COUNT(*) + h.f FROM h`, wantErr: api.ErrCodeGroupingError},
		{sql: `SELECT d.c FROM w, (SELECT COUNT(*) + w.f AS c FROM w AS w) AS d`, wantErr: api.ErrCodeGroupingError},
		{sql: `SELECT d.k FROM w, (SELECT h.id AS k FROM h GROUP BY h.id HAVING COUNT(*) < h.f) AS d`, wantErr: api.ErrCodeGroupingError},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.sql)
			if tc.wantErr != "" {
				var apiErr *api.Error
				if err == nil {
					rows.Close()
					t.Fatalf("answered, want %s", tc.wantErr)
				}
				if !errors.As(err, &apiErr) || apiErr.Code != tc.wantErr {
					t.Fatalf("err = %v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				cells := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range cells {
					ptrs[i] = &cells[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, fmt.Sprint(cells))
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			sort.Strings(got)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFDB_PostAggregateOuterReferenceInOrderBy pins the ORDER BY half of the
// post-aggregate contract: a grouped derived body ordering by an expression
// over its own aggregate or grouping key and an enclosing row's value. LIMIT 1
// makes the order observable — each enclosing row keeps a different group,
// chosen by distance to its own value. With a single group the sort never
// evaluates a key, which is how a sort that could not see the enclosing
// binding answered the one-row join spec and failed here ("exact QOV W has
// no declared runtime binding"). Parentheses are explicit: the grammar
// gives every arithmetic operator one left-associative precedence, as Java's
// does. The same shapes run against the Java engine in conformance's
// ws_f_join_unnest spec.
func TestFDB_PostAggregateOuterReferenceInOrderBy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/post_aggregate_outer_reference_order_by"
	setup := testkit.OpenDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE post_aggregate_outer_reference_order_by_tmpl" +
			" CREATE TABLE w (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE post_aggregate_outer_reference_order_by_tmpl",
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+testkit.ClusterFile()+"&schema=MAIN")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Groups by h.f: 10 holds one row, 20 two, 30 three.
	for _, stmt := range []string{
		"INSERT INTO w VALUES (1, 1), (2, 2), (3, 3)",
		"INSERT INTO h VALUES (1, 10), (4, 20), (6, 20), (7, 30), (8, 30), (9, 30)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		// The group whose count is nearest w.f.
		{
			`SELECT w.id, d.k FROM w, (SELECT h.f AS k FROM h GROUP BY h.f ORDER BY (COUNT(*) - w.f) * (COUNT(*) - w.f) LIMIT 1) AS d`,
			[]string{"[1 10]", "[2 20]", "[3 30]"},
		},
		// The grouping key nearest w.id: 1 for w1 and w2, 4 for w3.
		{
			`SELECT w.id, d.k FROM w, (SELECT h.id AS k FROM h GROUP BY h.id ORDER BY (h.id - w.id) * (h.id - w.id) LIMIT 1) AS d`,
			[]string{"[1 1]", "[2 1]", "[3 4]"},
		},
		// The largest group first, whatever w.f scales it by.
		{
			`SELECT w.id, d.k, d.c FROM w, (SELECT h.f AS k, COUNT(*) AS c FROM h GROUP BY h.f ORDER BY COUNT(*) * w.f DESC LIMIT 1) AS d`,
			[]string{"[1 30 3]", "[2 30 3]", "[3 30 3]"},
		},
		// The same over an ungrouped body: the sort key reads the enclosing
		// row per sorted row.
		{
			`SELECT w.id, d.k FROM w, (SELECT h.id AS k FROM h ORDER BY (h.id - w.id) * (h.id - w.id) LIMIT 1) AS d`,
			[]string{"[1 1]", "[2 1]", "[3 4]"},
		},
		// A local alias shadowing the enclosing one: the sort keys read the
		// local w (h), whose largest f is 30 (ids 7, 8, 9, the first by id is
		// 7); read through the enclosing row they would tie and keep id 1.
		{
			`SELECT w.id, d.k FROM w, (SELECT w.id AS k FROM h AS w ORDER BY w.f DESC, w.id LIMIT 1) AS d`,
			[]string{"[1 7]", "[2 7]", "[3 7]"},
		},
		// Without a LIMIT the order is not observable, but the reference is
		// still bound: every group for every enclosing row.
		{
			`SELECT d.k FROM w, (SELECT h.f AS k FROM h GROUP BY h.f ORDER BY COUNT(*) + w.f) AS d`,
			[]string{"[10]", "[10]", "[10]", "[20]", "[20]", "[20]", "[30]", "[30]", "[30]"},
		},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			if got := testkit.SortedRowStrings(t, db, ctx, tc.sql); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}
