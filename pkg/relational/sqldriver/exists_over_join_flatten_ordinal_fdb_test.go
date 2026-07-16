package sqldriver_test

// Regression pin for a gated INNER 2-leg join carrying an EXISTS clause
// (translateJoinWithExists): the join seeds its baked ordinal row with the
// combined predicate list, and an existential quantifier contributes NO
// columns to that row (Java's model). A name-keyed row for this shape would
// conflate duplicate ID/GID column names across legs (last-leg-wins); the
// ordinal seed keeps each leg's value in its own slot instead, so the
// positional row reads per-slot correctly. The plain (no-EXISTS) cluster
// already resolves the same way — pin C below is the parity control.
//
// The pa/pb column-name collision (ID, GID on both) is load-bearing: it is
// exactly what a name-keyed row would conflate and the positional row keeps
// distinct.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_ExistsOverJoinFlattenOrdinalSeed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/i2c3_seed"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "i2c3_seed_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE pa (id BIGINT, gid BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE pb (id BIGINT, gid BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE pg (gid BIGINT, label STRING, PRIMARY KEY (gid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Group 10 passes EXISTS (pg has gid 10); group 20 fails it.
	if _, err := db.ExecContext(ctx, "INSERT INTO pa VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("seed pa: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO pb VALUES (100, 10), (101, 20)"); err != nil {
		t.Fatalf("seed pb: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO pg VALUES (10, 'a')"); err != nil {
		t.Fatalf("seed pg: %v", err)
	}

	// allRows scans every column of every row generically and renders each
	// row as "v1|v2|…" (NULL → <NULL>), sorted for determinism.
	allRows := func(t *testing.T, q string) []string {
		t.Helper()
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v\n  sql: %s", err, q)
		}
		defer r.Close()
		cols, err := r.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		var got []string
		for r.Next() {
			vals := make([]any, len(cols))
			for i := range vals {
				vals[i] = new(any)
			}
			if err := r.Scan(vals...); err != nil {
				t.Fatalf("scan: %v", err)
			}
			parts := make([]string, len(cols))
			for i, v := range vals {
				cell := *(v.(*any))
				if cell == nil {
					parts[i] = "<NULL>"
				} else {
					parts[i] = fmt.Sprint(cell)
				}
			}
			got = append(got, strings.Join(parts, "|"))
		}
		if err := r.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Strings(got)
		return got
	}
	want := func(t *testing.T, label, q string, want []string) {
		t.Helper()
		got := allRows(t, q)
		if len(got) != len(want) {
			t.Errorf("%s: rows = %v, want %v\n  sql: %s", label, got, want, q)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: rows = %v, want %v\n  sql: %s", label, got, want, q)
				return
			}
		}
	}

	qStar := "SELECT * FROM pa a, pb b WHERE a.gid = b.gid AND EXISTS (SELECT 1 FROM pg g WHERE g.gid = a.gid)"
	qStarNot := "SELECT * FROM pa a, pb b WHERE a.gid = b.gid AND NOT EXISTS (SELECT 1 FROM pg g WHERE g.gid = a.gid)"

	// (A) The duplicate-name observable through the EXISTS flatten: four
	// columns [ID GID ID GID], each leg's ID distinct by position. A
	// name-keyed row would conflate the dup names (last-leg-wins); the
	// ordinal seed keeps both.
	want(t, "A/star+EXISTS", qStar, []string{"1|10|100|10"})

	// (B) The NOT EXISTS polarity: group 20.
	want(t, "B/star+NOT EXISTS", qStarNot, []string{"2|20|101|20"})

	// (C) Parity control — the SAME star over the plain gated cluster (no
	// EXISTS; the group-10 restriction spelled as a plain conjunct). This
	// shape must pass with or without the EXISTS flatten.
	want(t, "C/star control (no EXISTS)",
		"SELECT * FROM pa a, pb b WHERE a.gid = b.gid AND a.gid = 10",
		[]string{"1|10|100|10"})

	// (D) Qualified projections, both polarities — the name model resolves
	// these via qualified Datum keys today; they must stay identical under
	// the ordinal seed (regression guards, not red).
	want(t, "D1/qualified EXISTS",
		"SELECT a.id, b.id FROM pa a, pb b WHERE a.gid = b.gid AND EXISTS (SELECT 1 FROM pg g WHERE g.gid = a.gid)",
		[]string{"1|100"})
	want(t, "D2/qualified NOT EXISTS",
		"SELECT a.id, b.id FROM pa a, pb b WHERE a.gid = b.gid AND NOT EXISTS (SELECT 1 FROM pg g WHERE g.gid = a.gid)",
		[]string{"2|101"})

	// (E) A non-EXISTS WHERE conjunct rides the baked predicate list.
	want(t, "E/conjunct mix",
		"SELECT a.id, b.id FROM pa a, pb b WHERE a.gid = b.gid AND a.id < 10 AND EXISTS (SELECT 1 FROM pg g WHERE g.gid = a.gid)",
		[]string{"1|100"})

	// (F) An UNCORRELATED scalar rider absorbs (pre-evaluated binding,
	// shape-agnostic — the ruling's absorption): rows unchanged.
	want(t, "F/uncorrelated scalar rider",
		"SELECT a.id, b.id FROM pa a, pb b WHERE a.gid = b.gid AND b.id >= (SELECT MIN(gid) FROM pg) AND EXISTS (SELECT 1 FROM pg g WHERE g.gid = a.gid)",
		[]string{"1|100"})

	// (G) EXISTS correlating to the SECOND leg (b): the baked correlation
	// predicate references the other leg's window.
	want(t, "G/EXISTS on second leg",
		"SELECT a.id, b.id FROM pa a, pb b WHERE a.gid = b.gid AND EXISTS (SELECT 1 FROM pg g WHERE g.gid = b.gid)",
		[]string{"1|100"})

	// (H) EXPLAIN determinism ×10 — both polarities, plus the PLAIN
	// symmetric join as the control (cost-tied swapped operands without the
	// existential machinery): a flip there is the generic winner machinery,
	// a flip only under EXISTS is the existential path.
	for _, q := range []string{qStar, qStarNot, "SELECT * FROM pa a, pb b WHERE a.gid = b.gid"} {
		var first string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&first); err != nil {
			t.Fatalf("explain: %v", err)
		}
		for i := 0; i < 9; i++ {
			var again string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&again); err != nil {
				t.Fatalf("explain %d: %v", i, err)
			}
			if again != first {
				t.Errorf("EXPLAIN is nondeterministic:\n  first: %s\n  again: %s", first, again)
				break
			}
		}
	}
}
