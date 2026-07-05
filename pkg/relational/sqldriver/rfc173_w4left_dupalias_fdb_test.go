package sqldriver_test

// RFC-173 QP-REF-BIND item 1 — duplicate FROM-source aliases follow Java's
// model end-to-end: duplicates REGISTER at FROM (per-leg binding ids, unique
// quantifier correlations), every reference resolves per-ATTRIBUTE (a
// reference matching >1 same-aliased source rejects with Java's exact 42702
// `Ambiguous reference X`; a uniquely-matching reference BINDS to that leg),
// and the unreferenced star answers with duplicate columns — all
// live-verified against the 4.12.11.0 conformance server. Before item 1, Go
// approximated at the FROM walk (blanket 42702 on shared-column duplicates,
// clean 0AF00 declines for the answering classes) and the pre-approximation
// state silently bound last-leg-wins.

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

func TestFDB_RFC173W4Left_DuplicateFromAliases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4l_dup"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE w4l_dup_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE w4l_dup_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO p VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("seed p: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO q VALUES (7)"); err != nil {
		t.Fatalf("seed q: %v", err)
	}

	reject := func(t *testing.T, q, wantSub string) {
		t.Helper()
		var x any
		err := db.QueryRowContext(ctx, q).Scan(&x)
		if err == nil {
			t.Errorf("must reject, got rows\n  sql: %s", q)
			return
		}
		if !strings.Contains(err.Error(), wantSub) {
			t.Errorf("error = %v, want it to contain %q\n  sql: %s", err, wantSub, q)
		}
	}

	// PER-ATTRIBUTE ambiguity (Java's SemanticAnalyzer attributes.size()==1
	// asserts, byte-equal text): a reference matching BOTH same-aliased
	// sources rejects at RESOLUTION.
	reject(t, "SELECT p.id FROM p, q, p", "Ambiguous reference P.ID")
	reject(t, "SELECT a.v FROM p AS a, p AS a", "Ambiguous reference A.V")
	// Later-pair three-way duplicate: the colliding pair need not involve
	// the first source.
	reject(t, "SELECT x.id FROM q AS x, p AS x, p AS x", "Ambiguous reference X.ID")
	// The same per-attribute 42702 through the WHERE walk: the SELECT ref
	// (x.qid) is unique to the q leg, the WHERE ref hits both p legs.
	reject(t, "SELECT x.qid FROM q AS x, p AS x, p AS x WHERE x.id = 1", "Ambiguous reference X.ID")

	// PER-ATTRIBUTE binding: a reference matching exactly ONE of the
	// same-aliased sources ANSWERS, bound to that leg (Java live-verified:
	// the item-1 flip — pre-item-1 this was the clean 0AF00 decline).
	var aid int64
	if err := db.QueryRowContext(ctx, "SELECT a.id FROM p AS a, q AS a WHERE a.id = 2").Scan(&aid); err != nil {
		t.Errorf("disjoint-column dup alias must ANSWER per-attribute (Java parity): %v", err)
	} else if aid != 2 {
		t.Errorf("disjoint-column dup alias = %d, want 2", aid)
	}
	// The SECOND leg's column binds too (q's qid; one q row × two p rows).
	rows2, err := db.QueryContext(ctx, "SELECT a.qid FROM p AS a, q AS a WHERE a.qid = 7")
	if err != nil {
		t.Errorf("second-leg dup binding must ANSWER: %v", err)
	} else {
		var got []int64
		for rows2.Next() {
			var v int64
			_ = rows2.Scan(&v)
			got = append(got, v)
		}
		rows2.Close()
		if !reflect.DeepEqual(got, []int64{7, 7}) {
			t.Errorf("second-leg dup rows = %v, want [7 7]", got)
		}
	}
	// The BARE unique reference over the disjoint duplicate — the discovered
	// master WRONG-ROWS bug (silently returned NULLs pre-item-1): v lives
	// only on p, so it resolves per-attribute and reads p's values.
	bareRows, err := db.QueryContext(ctx, "SELECT v FROM p AS a, q AS a")
	if err != nil {
		t.Errorf("bare unique dup reference must ANSWER: %v", err)
	} else {
		var got []int64
		for bareRows.Next() {
			var v sql.NullInt64
			_ = bareRows.Scan(&v)
			if !v.Valid {
				t.Errorf("bare unique dup reference returned NULL — the pre-item-1 wrong-rows bug")
			}
			got = append(got, v.Int64)
		}
		bareRows.Close()
		if !reflect.DeepEqual(got, []int64{10, 20}) {
			t.Errorf("bare unique dup rows = %v, want [10 20]", got)
		}
	}

	// SELECT * over the duplicate answers with DUPLICATE COLUMNS — Java's
	// exact layout (live-verified: cols [ID V QID ID V], full cross
	// product). The architecture-gate condition: duplicate labels AND
	// per-position values (the two p legs' slots vary independently across
	// the cross product — never a single-leg echo).
	// No ORDER BY: positional ORDER BY over a star SELECT is a SEPARATE
	// both-reject class (Java "Cascades planner could not plan query"; corpus
	// order_by_position_over_star) — not this test's subject. Row order is
	// immaterial here; the assertions below are set-membership over the cross
	// product.
	starRows, err := db.QueryContext(ctx, "SELECT * FROM p, q, p")
	if err != nil {
		t.Errorf("SELECT * over duplicates must ANSWER (Java: duplicate columns): %v", err)
	} else {
		cols, _ := starRows.Columns()
		if !reflect.DeepEqual(cols, []string{"ID", "V", "QID", "ID", "V"}) {
			t.Errorf("star columns = %v, want Java's [ID V QID ID V] (duplicate labels)", cols)
		}
		got := map[[5]int64]bool{}
		n := 0
		for starRows.Next() {
			var a, b, c, d, e int64
			if err := starRows.Scan(&a, &b, &c, &d, &e); err != nil {
				t.Errorf("star scan: %v", err)
				break
			}
			got[[5]int64{a, b, c, d, e}] = true
			n++
		}
		starRows.Close()
		want := [][5]int64{
			{1, 10, 7, 1, 10},
			{1, 10, 7, 2, 20},
			{2, 20, 7, 1, 10},
			{2, 20, 7, 2, 20},
		}
		if n != 4 {
			t.Errorf("star rows = %d, want the 4-row cross product", n)
		}
		for _, w := range want {
			if !got[w] {
				t.Errorf("star cross product missing row %v (per-position leg values must vary independently)", w)
			}
		}
	}

	// The CTE and derived star twins (Java live-verified: [ID ID], 4 rows).
	checkTwoColStar := func(t *testing.T, q string) {
		t.Helper()
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Errorf("star over duplicates must ANSWER: %v\n  sql: %s", err, q)
			return
		}
		defer r.Close()
		cols, _ := r.Columns()
		if len(cols) != 2 || cols[0] != "ID" || cols[1] != "ID" {
			t.Errorf("star columns = %v, want duplicate [ID ID]\n  sql: %s", cols, q)
		}
		got := map[[2]int64]bool{}
		n := 0
		for r.Next() {
			var a, b int64
			if err := r.Scan(&a, &b); err != nil {
				t.Errorf("scan: %v", err)
				return
			}
			got[[2]int64{a, b}] = true
			n++
		}
		if n != 4 || !got[[2]int64{1, 2}] || !got[[2]int64{2, 1}] {
			t.Errorf("star rows = %d %v, want the 4-row cross product incl. the mixed pairs\n  sql: %s", n, got, q)
		}
	}
	checkTwoColStar(t, "WITH w AS (SELECT id FROM p) SELECT * FROM w, w")
	checkTwoColStar(t, "SELECT * FROM (SELECT id FROM p) AS d, (SELECT id FROM p) AS d")

	// The SAME-TABLE self-cross star (`p, p`) — the corner deriveColumnsFromJoin's
	// display-sequence check does NOT trip (both legs have IDENTICAL display
	// names, so the merge sequence [ID V ID V] equals the RC's regardless of any
	// leg regroup), yet the result must still be per-position correct. It is:
	// the positional row and the columns align by construction, so serving is by
	// slot. Cols [ID V ID V]; the 4-row cross product with independently-varying
	// leg slots — a same-bare/different-binding reorder must not silently echo
	// one leg.
	ppRows, err := db.QueryContext(ctx, "SELECT * FROM p, p")
	if err != nil {
		t.Errorf("SELECT * FROM p, p must ANSWER: %v", err)
	} else {
		defer ppRows.Close()
		cols, _ := ppRows.Columns()
		if !reflect.DeepEqual(cols, []string{"ID", "V", "ID", "V"}) {
			t.Errorf("p,p star columns = %v, want [ID V ID V]", cols)
		}
		got := map[[4]int64]bool{}
		for ppRows.Next() {
			var a, b, c, d int64
			if err := ppRows.Scan(&a, &b, &c, &d); err != nil {
				t.Errorf("p,p scan: %v", err)
				break
			}
			got[[4]int64{a, b, c, d}] = true
		}
		for _, w := range [][4]int64{{1, 10, 1, 10}, {1, 10, 2, 20}, {2, 20, 1, 10}, {2, 20, 2, 20}} {
			if !got[w] {
				t.Errorf("p,p star missing row %v (per-position leg values must vary independently)", w)
			}
		}
	}

	// A legitimate self-join with DISTINCT aliases keeps working.
	var n int64
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM p AS a, p AS b WHERE a.id = b.id").Scan(&n); err != nil {
		t.Fatalf("distinct-alias self-join: %v", err)
	}
	if n != 2 {
		t.Errorf("self-join count = %d, want 2", n)
	}

	// A duplicate alias naming an UNDEFINED table is the undefined-table
	// error's territory (42F01) — resolution declines on unknowable tables,
	// so the ambiguity path cannot mask it.
	reject(t, "SELECT * FROM nosuch AS a, p AS a", "42F01")

	// The correlated-shadow qualified fallthrough (design-ruling amendment
	// (a), live-verified: Java ANSWERS): RESOLUTION falls through (M1), but the
	// SHADOWED variant — inner alias == outer alias — cannot yet be EMITTED
	// (QOV(P) would bind the inner leg's quantifier; cross-scope binding ids
	// are the booked follow-on). It declines LOUDLY: NEVER wrong rows. Two
	// decline mechanisms depending on inner arity, both pinned here:
	//
	//  (1) SINGLE inner source (`q AS p`): the resolver's isLocal short-circuit
	//      leaves needsQualification false, so it declines one step later at the
	//      executor's ordinal-resolution guard — the field is unresolvable in
	//      the inner row. Loud, never wrong rows.
	var fv int64
	if err := db.QueryRowContext(ctx,
		"SELECT p.v FROM p WHERE EXISTS (SELECT 1 FROM q AS p WHERE p.v = 10)").Scan(&fv); err == nil {
		t.Errorf("single-source shadowed fallthrough unexpectedly ANSWERED %d — if cross-scope binding landed, flip this pin to (10) and unmark the divergence", fv)
	} else if !strings.Contains(err.Error(), "not resolvable in the runtime row") {
		t.Errorf("single-source shadow must decline at the ordinal guard, got: %v", err)
	}
	//  (2) MULTI inner source (`q AS p, r AS x`): needsQualification is true, so
	//      the resolver catches the shadow at PLAN time — CorrelatedShadowError
	//      → 42703. This is the load-bearing expr.ResolveIdentifier decline.
	if err := db.QueryRowContext(ctx,
		"SELECT p.v FROM p WHERE EXISTS (SELECT 1 FROM q AS p, q AS x WHERE p.v = 10)").Scan(&fv); err == nil {
		t.Errorf("multi-source shadowed fallthrough unexpectedly ANSWERED %d — if cross-scope binding landed, flip this pin to (10)", fv)
	} else if !strings.Contains(err.Error(), "42703") || !strings.Contains(err.Error(), "shadowed by a same-named FROM source") {
		t.Errorf("multi-source shadow must decline 42703 CorrelatedShadowError, got: %v", err)
	}
	var cv int64
	if err := db.QueryRowContext(ctx,
		"SELECT p.v FROM p WHERE EXISTS (SELECT 1 FROM q AS z WHERE p.v = 10)").Scan(&cv); err != nil {
		t.Errorf("unshadowed correlated control must ANSWER: %v", err)
	} else if cv != 10 {
		t.Errorf("unshadowed correlated control = %d, want 10", cv)
	}
	// The inner-ambiguity-is-terminal twin: a locally-ambiguous reference
	// never falls through to a same-aliased outer (both engines 42702).
	reject(t, "SELECT x.id FROM p AS x WHERE EXISTS (SELECT 1 FROM p AS x, p AS x WHERE x.id = 1)",
		"Ambiguous reference X.ID")
}
