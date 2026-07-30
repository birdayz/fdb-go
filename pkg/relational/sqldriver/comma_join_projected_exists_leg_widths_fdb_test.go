package sqldriver_test

// A 3-way COMMA join with a projected EXISTS, over legs of UNEQUAL width.
//
// The N-way comma-join harness (TestFDB_NWayCommaJoinProjectedExists) pins this
// shape only when every leg is as narrow as possible — a two-column first leg and
// single-column narrowing legs. Leg WIDTH is not incidental here: the merged row's
// leg windows are (offset, width) pairs, so a wider leg shifts every window after
// it, and the projected EXISTS correlates to a column of the FIRST leg. A window
// computed one slot off reads a neighbouring leg's value and the EXISTS silently
// flips — no error, just wrong rows.
//
// So the first leg carries an extra column the query never mentions, and both
// narrowing legs carry one of their own. The data makes a mis-bind visible rather
// than merely possible: wd holds {1,3}, so a.id 1 and 3 must read EXISTS=true and
// a.id 2 must read false, while wb.k is 8 everywhere and wc.k is 9 everywhere — a
// window that slid into either narrowing leg therefore answers UNIFORMLY, which is
// the failure this test names.
//
// Recorded while measuring the leg-identity census: this exact shape was once
// observed failing with the internal "ordinal join build: result value contains
// baked ordinal references but is a *values.FieldValue ... planner bug" error, in a
// schema carrying several more tables. That observation did not reproduce in
// isolation — varying leg width, the EXISTS column alias, and the presence of an
// ARRAY-column table in the same template all still plan — so what is pinned here
// is the CORRECT behaviour rather than a decline. If the shape ever regresses to
// that internal error, this is the test that catches it.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_CommaJoinProjectedExists_UnequalLegWidths(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/s4cjwide"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	// wa is WIDER than the harness's na (id, v): `k` is never referenced by the
	// query, so it exists purely to shift the legs that follow it.
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE s4cjwide_tmpl"+
		" CREATE TABLE wa (id BIGINT NOT NULL, v BIGINT, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE wb (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE wc (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE wd (id BIGINT NOT NULL, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE s4cjwide_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO wa VALUES (1, 100, 7), (2, 200, 8), (3, 300, 7)",
		// Uniform k in both narrowing legs: a window that slid into one of them
		// produces a UNIFORM EXISTS, the wrong answer this test names.
		"INSERT INTO wb VALUES (1, 8), (2, 8), (3, 8)",
		"INSERT INTO wc VALUES (1, 9), (2, 9), (3, 9)",
		"INSERT INTO wd VALUES (1), (3)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// Both spellings: an ALIASED projected EXISTS and a bare one. The alias changes
	// the projection's output naming, which sits upstream of the leg windows, so the
	// two must agree — pinning only one would leave the other naming path untested.
	for _, tc := range []struct{ name, sql string }{
		{
			"aliased EXISTS",
			"SELECT a.v, EXISTS (SELECT 1 FROM wd d WHERE d.id = a.id) AS has_d " +
				"FROM wa a, wb b, wc c WHERE a.id = b.id AND b.id = c.id",
		},
		{
			"bare EXISTS",
			"SELECT a.v, EXISTS (SELECT 1 FROM wd d WHERE d.id = a.id) " +
				"FROM wa a, wb b, wc c WHERE a.id = b.id AND b.id = c.id",
		},
	} {
		rows, qErr := db.QueryContext(ctx, tc.sql)
		if qErr != nil {
			t.Fatalf("%s: unequal-width comma join must plan, got: %v\n  sql: %s",
				tc.name, qErr, tc.sql)
		}
		var got []string
		for rows.Next() {
			var v int64
			var ex sql.NullBool
			if scanErr := rows.Scan(&v, &ex); scanErr != nil {
				rows.Close()
				t.Fatalf("%s: scan: %v", tc.name, scanErr)
			}
			got = append(got, fmt.Sprintf("%d|%v", v, ex.Valid && ex.Bool))
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			t.Fatalf("%s: rows: %v", tc.name, rowsErr)
		}
		sort.Strings(got)
		want := []string{"100|true", "200|false", "300|true"}
		if len(got) != len(want) {
			t.Fatalf("%s: rows = %v, want %v", tc.name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: rows = %v, want %v — a UNIFORM answer means the projected "+
					"EXISTS stopped correlating to the FIRST leg's id and read a shifted "+
					"window (wb.k=8 and wc.k=9 are uniform, so a slid window answers the "+
					"same for every row)", tc.name, got, want)
			}
		}
	}
}
