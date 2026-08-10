package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_AmbiguousColumnRefRejected pins the upstream invariant that several
// first-match name lookups in the planner silently depend on and none of them
// cites: a column reference whose name is carried by more than one visible
// source is REJECTED with SQLSTATE 42702 before it ever reaches the planner.
//
// Why this is load-bearing rather than incidental. A leg-concat row legitimately
// declares one name twice — `SELECT * FROM zn AS a, zn AS b` produces
// ID,PARTNER,K,ID,PARTNER,K in a single leg window, measured in production — and
// several sites resolve a reference against that row BY NAME. They are safe only
// because no reference reaching them can be ambiguous, and that guarantee lives
// here, in the semantic layer, not at those sites. Nothing pinned it. Relaxing
// any of these rejections would arm every one of those lookups at once, and the
// symptom would be a wrong-column read: the loser of a first match is a real
// column of the same type, so nothing downstream rejects it.
//
// This is a NEGATIVE result made checkable. The claim it protects is "the
// ambiguous case is unreachable", and a claim of unreachability is worth exactly
// as much as the test that fails when it stops being true.
//
// Java and Postgres agree, which is why the rejection is conformance and not a
// Go-only strictness: Java's Type.Record builds its name maps with
// ImmutableMap.toImmutableMap, which THROWS on a duplicate key, and Postgres
// answers 42702 for the same shapes.
func TestFDB_AmbiguousColumnRefRejected(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/ambig_colref"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE ambig_tmpl"+
		" CREATE TABLE zn (id BIGINT, partner BIGINT, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE zp (pid BIGINT, fk BIGINT, PRIMARY KEY (pid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE ambig_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"INSERT INTO zn VALUES (1, 2, 100), (2, 1, 200)",
		"INSERT INTO zp VALUES (1, 200)",
	} {
		if _, e := db.ExecContext(ctx, s); e != nil {
			t.Fatalf("seed %q: %v", s, e)
		}
	}

	// Each shape names a column that TWO visible sources declare. None of them
	// has an honest answer, so each must be refused rather than resolved.
	ambiguous := []struct{ name, query string }{
		{
			"bare over a self-join",
			"SELECT k FROM zn AS a, zn AS b WHERE a.partner = b.id",
		},
		{
			"bare in a predicate over a self-join",
			"SELECT a.id FROM zn AS a, zn AS b WHERE k = 100",
		},
		{
			"qualified into a derived table declaring the name twice",
			"SELECT d.k FROM (SELECT x.k, y.k FROM zn AS x, zn AS y) AS d",
		},
		{
			"bare over a SELECT * derived table declaring the name twice",
			"SELECT k FROM (SELECT * FROM zn AS x, zn AS y) AS d",
		},
		{
			"correlated read into a dup-named derived table",
			"SELECT a.id FROM zn AS a JOIN (SELECT x.k, y.k FROM zn AS x, zn AS y) AS d " +
				"ON d.k = a.partner",
		},
		{
			"ambiguous name under EXISTS over a dup-named derived table",
			"SELECT a.id FROM zn AS a WHERE EXISTS " +
				"(SELECT 1 FROM (SELECT x.k, y.k FROM zn AS x, zn AS y) AS d WHERE d.k = a.partner)",
		},
		{
			"ORDER BY an ambiguous name",
			"SELECT a.id FROM zn AS a, zn AS b WHERE a.partner = b.id ORDER BY k",
		},
	}
	for _, tc := range ambiguous {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.query)
			if err == nil {
				// Some drivers defer the error to the first Next/Scan, so a nil
				// error here is not yet a pass.
				defer rows.Close()
				if rows.Next() {
					t.Fatalf("an ambiguous column reference RESOLVED and returned rows.\n"+
						"  query: %s\n"+
						"  Two visible sources declare that name, so any answer is a first "+
						"match dressed as a fact. Several planner sites resolve references "+
						"against a leg-concat row BY NAME and are safe ONLY because this "+
						"rejection makes the ambiguous case unreachable; if this shape now "+
						"plans, every one of those lookups is armed and the failure mode is "+
						"a silent wrong-column read.", tc.query)
				}
				if err = rows.Err(); err == nil {
					t.Fatalf("an ambiguous column reference was accepted (no error, no rows).\n"+
						"  query: %s\n"+
						"  See above: this rejection is what makes the by-name planner "+
						"lookups safe.", tc.query)
				}
			}
			// 42702 is `ambiguous_column`. The assertion is on the CODE, not on the
			// message: a different code would mean the query is being refused for
			// some unrelated reason, which would make this test pass while proving
			// nothing about ambiguity.
			if !strings.Contains(err.Error(), "42702") {
				t.Fatalf("ambiguous column reference rejected with the WRONG error.\n"+
					"  query: %s\n"+
					"  got:   %v\n"+
					"  want:  SQLSTATE 42702 (ambiguous_column).\n"+
					"  A refusal for an unrelated reason would let this test pass while "+
					"the ambiguity guard itself was gone.", tc.query, err)
			}
		})
	}

	// THE OTHER HALF, and the reason this test cannot be satisfied by refusing
	// everything. A guard that rejected all of the above by rejecting all
	// self-joins would be green on the block above and useless. These shapes name
	// the SAME columns unambiguously and must still plan and return rows.
	unambiguous := []struct {
		name, query string
		wantRows    int
	}{
		{"qualified over a self-join", "SELECT a.k, b.k FROM zn AS a, zn AS b WHERE a.partner = b.id", 2},
		{"renamed in a derived table", "SELECT d.xk, d.yk FROM (SELECT x.k AS xk, y.k AS yk FROM zn AS x, zn AS y) AS d", 4},
		{"unambiguous bare over one source", "SELECT k FROM zn AS a", 2},
		{"bare naming only one side's column", "SELECT partner FROM zn AS a, zp AS p WHERE p.fk = a.k", 1},
	}
	for _, tc := range unambiguous {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatalf("an UNAMBIGUOUS reference was rejected.\n"+
					"  query: %s\n"+
					"  err:   %v\n"+
					"  The ambiguity guard has over-fired. A guard that refuses these "+
					"would make the rejection block above vacuous — it would be passing "+
					"because everything is refused, not because ambiguity is caught.",
					tc.query, err)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("%s: iterate: %v", tc.query, err)
			}
			if n != tc.wantRows {
				t.Fatalf("%s: got %d rows, want %d", tc.query, n, tc.wantRows)
			}
		})
	}
}
