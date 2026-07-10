package sqldriver_test

// RFC-173 S4 — the CORRELATED-INDEX EXISTS name path is PERMANENT and
// Java-correct. A WHERE-EXISTS that
// correlates into a leg BURIED in an inner join is the canonical semijoin;
// its good plan is a CORRELATED INDEX SCAN (`Scan(A, [=b.aid])` SARG'd under
// the FlatMap), which REQUIRES name binding to flow the sibling comparand
// into A's index. The ordinal seed is a Go-only optimization for the
// INDEPENDENT-legs materialized-NLJ case; it cannot express a positional
// correlation, and Java has no positional-correlation concept either — so
// this shape STAYS name-model, permanently. This pins that the name path
// keeps the SARG'd index plan (a cross-product would be an N³-vs-N regression
// on the shape where the index matters most) AND returns correct rows. The
// atomic cap therefore cannot delete NewAnchoredJoinRecord for this shape.
//
// This is the required permanence regression: it TRIPS if a future
// producer-retirement re-ordinalizes the correlated-index existential and
// drops the SARG (the reverted commit-A cross-product wall).

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestFDB_RFC173_CorrelatedIndexExistsStaysIndexed(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173corridx"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173corridx_tmpl"+
		" CREATE TABLE a (id BIGINT, av BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE b (bid BIGINT, aid BIGINT, PRIMARY KEY (bid))"+
		" CREATE TABLE c (cid BIGINT, aid BIGINT, PRIMARY KEY (cid))"+
		" CREATE TABLE e (eid BIGINT, eref BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173corridx_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"INSERT INTO a VALUES (1, 10), (2, 20)",
		"INSERT INTO b VALUES (101, 1), (102, 2)",
		"INSERT INTO c VALUES (201, 1), (202, 2)",
		"INSERT INTO e VALUES (301, 1)", // e correlates only a.id=1
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}

	sqlText := "SELECT a.av FROM a, b, c WHERE b.aid = a.id AND c.aid = a.id" +
		" AND EXISTS (SELECT 1 FROM e WHERE e.eref = a.id)"

	// (1) The PLAN keeps the SARG'd correlated index scan under the join — NOT
	// a full A×B×C cross product. `[=]` marks a SARG'd equality scan; the
	// cross-product form is NLJ(NLJ(Scan(A),Scan(B)),Scan(C)) with no SARG.
	var explain string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+sqlText).Scan(&explain); err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !strings.Contains(explain, "[=]") {
		t.Fatalf("correlated-index EXISTS lost its SARG'd index scan (a cross-product regression — the reverted commit-A wall):\n%s", explain)
	}
	if strings.Contains(explain, "NestedLoopJoin(INNER, NestedLoopJoin(INNER, Scan(A), Scan(B)), Scan(C))") {
		t.Fatalf("correlated-index EXISTS regressed to the full A×B×C cross product:\n%s", explain)
	}

	// (2) Correct rows: only a.id=1 has an e match → av=10.
	rows, qErr := db.QueryContext(ctx, sqlText)
	if qErr != nil {
		t.Fatalf("query: %v", qErr)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var v int64
		if sErr := rows.Scan(&v); sErr != nil {
			t.Fatalf("scan: %v", sErr)
		}
		got = append(got, v)
	}
	if rErr := rows.Err(); rErr != nil {
		t.Fatalf("rows: %v", rErr)
	}
	if len(got) != 1 || got[0] != 10 {
		t.Fatalf("rows = %v, want [10]", got)
	}
}
