package sqldriver_test

// RFC-173 S4 F2-LEFT — a PROJECTED EXISTS over a LEFT JOIN. Live-Java (4.12.11.0)
// ANSWERS this (folds the projected EXISTS, null-extends the null-supplying leg,
// evaluates the EXISTS against the null-padded row); Go rejected it (0AF00
// "not yet supported"). This is a genuine Java-parity REACH gap closed the
// producer-consistent way (Option 2, the ordinal fold): the undissolved LEFT box
// has correlatedStep1 false, so foldStep1Seed reconstructs the positional seed
// and a JoinLeftOuter NLJ null-extends the null-supplying leg positionally over
// the DefaultOnEmpty row; NO new name-model producer. Scan-leg scope only.
//
// The three shapes are the exact live-Java-verified probe outcomes (NO ORDER BY),
// so they double as the parity oracle. Dimension 1 is the twice-reverted
// null-on-empty × baked-seed hazard: the null-supplying leg's q.qid must be NULL
// in the positional merged row (never a wrong-slot/stale read) AND the EXISTS
// correlation `r.id = q.qid` must read THAT null (→ no match → false).
//
// ORDER-BY-FREE deliberately: adding an ORDER BY on top of this fold makes Java's
// Cascades fail to plan ("could not plan query") while Go handles it — a SEPARATE
// Go-beyond-Java planner reach, not the parity claim. Row order is asserted by
// sorting in Go, not via SQL, so these stay the verified parity shape.

import (
	"context"
	"database/sql"
	"sort"
	"testing"
)

func TestFDB_RFC173F2Left_ProjectedExistsOverLeftJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173f2left"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173f2left_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE r (id BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173f2left_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// p.id ∈ {1,2}; q.qid = 7 matches NEITHER p row → q is NULL-extended for
	// both. r.id = 5.
	if _, err := db.ExecContext(ctx, "INSERT INTO p VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("seed p: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO q VALUES (7)"); err != nil {
		t.Fatalf("seed q: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO r VALUES (5)"); err != nil {
		t.Fatalf("seed r: %v", err)
	}

	// (1) THE HAZARD PIN — null-padded correlated EXISTS. q NULL-extended (no
	// match), so q.qid = NULL, so EXISTS(SELECT 1 FROM r WHERE r.id = q.qid)
	// reads NULL → no match → false. Java: [[10 false] [20 false]].
	t.Run("dim1_null_padded_correlated_exists", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT p.v, EXISTS (SELECT 1 FROM r WHERE r.id = q.qid) "+
				"FROM p LEFT JOIN q ON q.qid = p.id")
		if err != nil {
			t.Fatalf("projected EXISTS over LEFT JOIN errored (Java answers it — F2-LEFT reach gap): %v", err)
		}
		defer rows.Close()
		var got [][2]any
		for rows.Next() {
			var v int64
			var ex bool
			if err := rows.Scan(&v, &ex); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, [2]any{v, ex})
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Slice(got, func(i, j int) bool { return got[i][0].(int64) < got[j][0].(int64) })
		want := [][2]any{{int64(10), false}, {int64(20), false}}
		if len(got) != len(want) {
			t.Fatalf("got %d rows %v, want %v (q NULL-extended → EXISTS(r.id=NULL) false)", len(got), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d = %v, want %v — the null-supplying leg's q.qid must be NULL and the EXISTS correlation must read it", i, got[i], want[i])
			}
		}
	})

	// (2) STAR with explicit NULL — the null-extension flows to the projection
	// positionally: q.qid projects explicit NULL. Java: [[10 <nil> false] [20 <nil> false]].
	t.Run("dim2_star_explicit_null", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT p.v, q.qid, EXISTS (SELECT 1 FROM r WHERE r.id = q.qid) "+
				"FROM p LEFT JOIN q ON q.qid = p.id")
		if err != nil {
			t.Fatalf("star projected EXISTS over LEFT JOIN errored: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var v int64
			var qid sql.NullInt64
			var ex bool
			if err := rows.Scan(&v, &qid, &ex); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if qid.Valid {
				t.Fatalf("q.qid = %d, want NULL (the LEFT null-extension must flow to the projection positionally)", qid.Int64)
			}
			if ex {
				t.Fatalf("EXISTS = true for v=%d, want false (q.qid NULL → no r match)", v)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if n != 2 {
			t.Fatalf("got %d rows, want 2", n)
		}
	})

	// (3) UNCORRELATED EXISTS over the LEFT fold — the leg-independent case rides
	// the fold. r is non-empty → true for every row. Java: [[10 true] [20 true]].
	t.Run("dim3_uncorrelated_exists", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT p.v, EXISTS (SELECT 1 FROM r) FROM p LEFT JOIN q ON q.qid = p.id")
		if err != nil {
			t.Fatalf("uncorrelated projected EXISTS over LEFT JOIN errored: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var v int64
			var ex bool
			if err := rows.Scan(&v, &ex); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !ex {
				t.Fatalf("EXISTS(SELECT 1 FROM r) = false for v=%d, want true (r is non-empty)", v)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if n != 2 {
			t.Fatalf("got %d rows, want 2", n)
		}
	})
}
