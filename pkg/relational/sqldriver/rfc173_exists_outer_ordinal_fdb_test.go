package sqldriver_test

// RFC-173 regression pin for the CORRELATED-EXISTS-over-scan OUTER-binding edge.
//
// Shape:  SELECT dname FROM dept d WHERE [NOT] EXISTS (SELECT 1 FROM emp e WHERE e.did = d.did)
//
// The outer of the EXISTS semi-join is a plain base-table scan; the inner subquery's
// correlated reference `d.did` is a LAZY FieldValue over QOV(outer). Before this slice
// the flatMap cursor bound the outer as its name-keyed Datum map, so `d.did` resolved
// by NAME (the outer-binding edge) and the downstream `SELECT dname` projection likewise
// read the outer column by NAME off the identity pass-through Datum (the output edge).
// Now the outer binds POSITIONALLY (its own scan PositionalRow, layout-safe via
// adaptLegPositional), so the inner correlated ref resolves by ordinal, and the identity
// pass-through propagates that PositionalRow so the downstream projection reads by ordinal.
//
// The name-keyed outer-binding path this shape used to take has been DELETED, so the
// outer binds POSITIONALLY (its own scan PositionalRow) as the only path: the inner
// correlated ref resolves by ordinal and the identity pass-through propagates that
// PositionalRow so the downstream projection reads by ordinal. This test asserts the
// exact rows for both the EXISTS and NOT-EXISTS shapes. Correlated EXISTS / NOT EXISTS
// has subtle NULL semantics; the asserted rows pin them. The schema is unique to this
// test, so no cached plan is shared.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_RFC173_ExistsOuterOrdinal(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_exists_ord")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_exists_ord")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE exists_ord "+
			"CREATE TABLE dept (did BIGINT NOT NULL, dname STRING, PRIMARY KEY (did)) "+
			"CREATE TABLE emp (eid BIGINT NOT NULL, did BIGINT, PRIMARY KEY (eid))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_exists_ord/s WITH TEMPLATE exists_ord")
	dsn := fmt.Sprintf("fdbsql:///testdb_exists_ord?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// dept: eng(1), sales(2), hr(3)   emp: did references 1 and 2 only (hr has none).
	mwjoMustExec(t, db, ctx,
		"INSERT INTO dept (did, dname) VALUES (1, 'eng'), (2, 'sales'), (3, 'hr')")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO emp (eid, did) VALUES (10, 1), (11, 1), (12, 2)")

	names := func(t *testing.T, q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, n)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}

	// EXISTS: dept rows that HAVE a matching emp — eng, sales (hr has none).
	t.Run("correlated_exists", func(t *testing.T) {
		if got := names(t, "SELECT dname FROM dept d WHERE EXISTS (SELECT 1 FROM emp e WHERE e.did = d.did)"); got != "eng,sales" {
			t.Errorf("EXISTS = %q, want %q", got, "eng,sales")
		}
	})
	// NOT EXISTS: dept rows with NO matching emp — hr only.
	t.Run("correlated_not_exists", func(t *testing.T) {
		if got := names(t, "SELECT dname FROM dept d WHERE NOT EXISTS (SELECT 1 FROM emp e WHERE e.did = d.did)"); got != "hr" {
			t.Errorf("NOT EXISTS = %q, want %q", got, "hr")
		}
	})
}
