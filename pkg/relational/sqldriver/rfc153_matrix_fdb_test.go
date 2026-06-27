package sqldriver_test

// RFC-153 — joined/derived-preserved-side LEFT OUTER buried-merge correlation.
//
// When the preserved side of a LEFT OUTER is itself a join
// (`A JOIN B ... LEFT JOIN C ON c.a_id = a.id`), the ON predicate correlates C to
// a BURIED preserved source alias (`A`, hidden under the A⋈B join). The fix plans
// it as a correlated index-probe FlatMap that null-extends unmatched preserved
// rows. This FDB matrix pins ROW correctness (matched + LEFT/FULL-OUTER
// null-extension) across a matrix of buried-correlation shapes; the embedded
// companion test pins the typed probe plan shape.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// rfc153Canon renders a scanned (a.id, c.id) pair to a stable canonical string
// ("<a>|<c>", NULL rendered as "NULL") so row-set assertions are insensitive to
// output order and to NULL-ordering (which matters for FULL OUTER).
func rfc153Canon(a, c sql.NullInt64) string {
	render := func(v sql.NullInt64) string {
		if !v.Valid {
			return "NULL"
		}
		return fmt.Sprintf("%d", v.Int64)
	}
	return render(a) + "|" + render(c)
}

// TestFDB_RFC153_JoinedPreservedMatrix seeds A⋈B(⋈D) plus a partially-matching C
// and runs a matrix of LEFT/FULL OUTER joins whose preserved side is itself a
// join, correlating C to a buried preserved alias. Each case asserts the exact
// (order-insensitive) row set, proving the correlated probe FlatMap executes the
// OUTER null-extension semantics correctly.
func TestFDB_RFC153_JoinedPreservedMatrix(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rfc153mx")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rfc153mx")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rfc153mx "+
			"CREATE TABLE a (id BIGINT NOT NULL, flag BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, bx BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT NOT NULL, b_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, bx_ref BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id) "+
			"CREATE INDEX d_b_id ON d (b_id) "+
			"CREATE INDEX c_a_id ON c (a_id) "+
			"CREATE INDEX c_bx_ref ON c (bx_ref)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rfc153mx/s WITH TEMPLATE rfc153mx")
	dsn := fmt.Sprintf("fdbsql:///testdb_rfc153mx?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1, 0), (2, 0)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (10, 1, 100), (11, 2, 200)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d VALUES (1000, 10), (1001, 11)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (50, 1, 100), (51, 99, 999)")

	cases := []struct {
		name    string
		tail    string // query body after "SELECT a.id, c.id FROM "
		orderBy bool   // append "ORDER BY a.id" (false for FULL — a.id may be NULL)
		want    []string
	}{
		{
			name:    "joined_left",
			tail:    "a JOIN b ON b.a_id = a.id LEFT JOIN c ON c.a_id = a.id",
			orderBy: true,
			want:    []string{"1|50", "2|NULL"},
		},
		{
			name:    "buried_other_leg",
			tail:    "a JOIN b ON b.a_id = a.id LEFT JOIN c ON c.bx_ref = b.bx",
			orderBy: true,
			want:    []string{"1|50", "2|NULL"},
		},
		{
			name:    "three_way_deeper",
			tail:    "a JOIN b ON b.a_id = a.id JOIN d ON d.b_id = b.id LEFT JOIN c ON c.a_id = a.id",
			orderBy: true,
			want:    []string{"1|50", "2|NULL"},
		},
		{
			name:    "simple_preserved",
			tail:    "a LEFT JOIN c ON c.a_id = a.id",
			orderBy: true,
			want:    []string{"1|50", "2|NULL"},
		},
		{
			name:    "preserved_only",
			tail:    "a LEFT JOIN c ON a.flag = 1",
			orderBy: true,
			want:    []string{"1|NULL", "2|NULL"},
		},
		{
			name:    "full_joined",
			tail:    "a JOIN b ON b.a_id = a.id FULL OUTER JOIN c ON c.a_id = a.id",
			orderBy: false,
			want:    []string{"1|50", "2|NULL", "NULL|51"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			query := "SELECT a.id, c.id FROM " + tc.tail
			if tc.orderBy {
				query += " ORDER BY a.id"
			}
			rows, err := db.QueryContext(ctx, query)
			if err != nil {
				t.Errorf("query %q: %v", query, err)
				return
			}
			defer rows.Close()

			var got []string
			for rows.Next() {
				var aID, cID sql.NullInt64
				if err := rows.Scan(&aID, &cID); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, rfc153Canon(aID, cID))
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}

			want := append([]string(nil), tc.want...)
			sort.Strings(got)
			sort.Strings(want)
			if !equalStringSlices(got, want) {
				t.Errorf("query %q:\n  got  %v\n  want %v", query, got, want)
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFDB_RFC153_AggregateInnerNullExtension pins ROW correctness for the fail-closed
// axis (the 3-reviewer NAK dimension): the null-supplying side is an AGGREGATE whose
// grouping correlates to the buried preserved alias A. Its inner carries a StreamingAgg
// node the buried-merge rebaser does not rewrite, so the verifier fail-CLOSES → the LEFT
// OUTER declines the probe → materialized NLJ, which must null-extend correctly. A=3 has
// no C rows → no aggregate group → its row null-extends; A=1/2 carry their COUNT(*).
func TestFDB_RFC153_AggregateInnerNullExtension(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rfc153agg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rfc153agg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rfc153agg "+
			"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rfc153agg/s WITH TEMPLATE rfc153agg")
	dsn := fmt.Sprintf("fdbsql:///testdb_rfc153agg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1), (2), (3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (10, 1), (11, 2), (12, 3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (50, 1), (51, 1), (52, 2)") // a_id=3 absent

	rows, err := db.QueryContext(ctx,
		"SELECT a.id, g.cnt FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN (SELECT a_id, COUNT(*) cnt FROM c GROUP BY a_id) g ON g.a_id = a.id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var a, cnt sql.NullInt64
		if err := rows.Scan(&a, &cnt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, rfc153Canon(a, cnt))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)
	want := []string{"1|2", "2|1", "3|NULL"} // A=3 null-extended; A=1 cnt 2, A=2 cnt 1
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("aggregate-inner null-extension rows = %v, want %v (cross-product / dropped rows ⇒ wrong)", got, want)
	}
}
