package sqldriver_test

// Duplicate FROM-source aliases follow Java's model end-to-end: duplicates
// REGISTER at FROM (per-leg binding ids, unique quantifier correlations),
// every reference resolves per-ATTRIBUTE (a reference matching >1
// same-aliased source rejects with Java's exact 42702 `Ambiguous reference
// X`; a uniquely-matching reference BINDS to that leg), and the
// unreferenced star answers with duplicate columns — all live-verified
// against the 4.12.11.0 conformance server. Go used to approximate at the
// FROM walk (blanket 42702 on shared-column duplicates, clean 0AF00 declines
// for the answering classes), and before that approximation it silently
// bound last-leg-wins.

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

func TestFDB_DuplicateFromAliases(t *testing.T) {
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
	// same-aliased sources ANSWERS, bound to that leg (Java live-verified;
	// this used to be a clean 0AF00 decline).
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
	// The BARE unique reference over the disjoint duplicate — a discovered
	// WRONG-ROWS bug (silently returned NULLs before this fix): v lives
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
				t.Errorf("bare unique dup reference returned NULL — the previous wrong-rows bug")
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

	// The correlated-shadow qualified fallthrough (live-verified: Java
	// ANSWERS): RESOLUTION falls through to the outer p when the inner `q AS
	// p` lacks the column. Two mechanisms depending on inner arity, both
	// pinned here:
	//
	//  (1) SINGLE inner source (`q AS p`): ANSWERS — Java parity. The
	//      collision mint gives a single-table correlated-EXISTS inner a
	//      unique CorrelationName, so the resolver's isLocal short-circuit no
	//      longer swallows the parent hit: the fallthrough emits QOV(P)
	//      against the OUTER leg's binding and the query answers p.v=10,
	//      matching Java's live-verified behaviour. (Before minting, this
	//      declined LOUDLY at the executor's ordinal-resolution guard —
	//      cross-scope emission needed the mint to reach the
	//      correlated-EXISTS scope.)
	var fv int64
	if err := db.QueryRowContext(ctx,
		"SELECT p.v FROM p WHERE EXISTS (SELECT 1 FROM q AS p WHERE p.v = 10)").Scan(&fv); err != nil {
		t.Errorf("single-source shadowed fallthrough must ANSWER (Java parity, amendment (a)): %v", err)
	} else if fv != 10 {
		t.Errorf("single-source shadowed fallthrough = %d, want 10 (outer p.v)", fv)
	}
	// The NOT-EXISTS polarity twin of the fallthrough: the outer-only
	// conjunct evaluates UNDER the ∃ in both polarities (the placement
	// invariant), so NOT EXISTS ⇔ ¬(q non-empty ∧ p.v=10) ⇔ p.v≠10 → the
	// v=20 row. Polarity is this bug class's proven hiding axis — pinned so
	// the two polarities can never drift apart again.
	var nfv int64
	if err := db.QueryRowContext(ctx,
		"SELECT p.v FROM p WHERE NOT EXISTS (SELECT 1 FROM q AS p WHERE p.v = 10)").Scan(&nfv); err != nil {
		t.Errorf("single-source shadowed fallthrough NOT-EXISTS twin must ANSWER: %v", err)
	} else if nfv != 20 {
		t.Errorf("single-source shadowed fallthrough NOT-EXISTS twin = %d, want 20 (the complement row)", nfv)
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

// TestFDB_DupAliasOrderGroupCorrelated pins two gaps in duplicate-FROM-alias
// handling: a per-attribute reference that binds a LATER duplicate leg must
// keep binding through (1) ORDER BY / GROUP BY keys — the sort/group key
// must resolve to the minted binding's namespace (Q$DUP1.QID), never
// silently miss as the SQL alias (A.QID) and return scan-order/NULL-grouped
// rows — and (2) a CORRELATED subquery's outer scope — the outer-scope
// carrier must stay duplicate-preserving and binding-aware, so an inner
// reference resolves per-attribute across ALL same-aliased outer legs
// (1→bind, 0→fallthrough, ≥2→terminal 42702, at any scope depth).
// Expectations follow Java's live-verified per-attribute semantics; the
// corpus governs error-text parity for the correlated shapes.
func TestFDB_DupAliasOrderGroupCorrelated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4l_dupsg"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE w4l_dupsg_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE w4l_dupsg_tmpl"); err != nil {
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
	// TWO q rows so a second-leg sort/group key has distinguishable values.
	if _, err := db.ExecContext(ctx, "INSERT INTO q VALUES (1), (7)"); err != nil {
		t.Fatalf("seed q: %v", err)
	}

	queryInts := func(t *testing.T, q string) ([]int64, error) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			if !v.Valid {
				t.Errorf("NULL value — the dup-leg key silently missed\n  sql: %s", q)
			}
			got = append(got, v.Int64)
		}
		return got, rows.Err()
	}

	// ORDER BY a SECOND-leg-bound key: qid lives only on q (the later
	// duplicate, binding Q$DUP1) — the sort must order by the leg's values,
	// in both directions, and through LIMIT.
	if got, err := queryInts(t, "SELECT a.qid FROM p AS a, q AS a ORDER BY a.qid"); err != nil {
		t.Errorf("second-leg ORDER BY must ANSWER: %v", err)
	} else if !reflect.DeepEqual(got, []int64{1, 1, 7, 7}) {
		t.Errorf("second-leg ORDER BY ASC rows = %v, want [1 1 7 7] (sorted by the DUP leg's values, never scan order)", got)
	}
	if got, err := queryInts(t, "SELECT a.qid FROM p AS a, q AS a ORDER BY a.qid DESC LIMIT 1"); err != nil {
		t.Errorf("second-leg ORDER BY DESC LIMIT must ANSWER: %v", err)
	} else if !reflect.DeepEqual(got, []int64{7}) {
		t.Errorf("second-leg ORDER BY DESC LIMIT 1 = %v, want [7] (a missed sort key returns the scan-order row)", got)
	}
	// First-leg control: v lives only on p (the FIRST occurrence keeps the
	// alias as its binding) — must keep working alongside the dup fix.
	if got, err := queryInts(t, "SELECT a.v FROM p AS a, q AS a ORDER BY a.v DESC"); err != nil {
		t.Errorf("first-leg ORDER BY control must ANSWER: %v", err)
	} else if !reflect.DeepEqual(got, []int64{20, 20, 10, 10}) {
		t.Errorf("first-leg ORDER BY DESC rows = %v, want [20 20 10 10]", got)
	}

	// GROUP BY the second-leg-bound key: grouping must key the DUP leg's
	// values — a missed key groups everything under NULL.
	groupRows, err := db.QueryContext(ctx, "SELECT a.qid, COUNT(*) FROM p AS a, q AS a GROUP BY a.qid")
	if err != nil {
		t.Errorf("second-leg GROUP BY must ANSWER: %v", err)
	} else {
		counts := map[int64]int64{}
		n := 0
		for groupRows.Next() {
			var k, c sql.NullInt64
			if err := groupRows.Scan(&k, &c); err != nil {
				t.Errorf("group scan: %v", err)
				break
			}
			if !k.Valid {
				t.Errorf("GROUP BY key is NULL — the dup-leg group key silently missed")
				continue
			}
			counts[k.Int64] = c.Int64
			n++
		}
		groupRows.Close()
		if n != 2 || counts[1] != 2 || counts[7] != 2 {
			t.Errorf("second-leg GROUP BY = %v (%d groups), want {1:2, 7:2}", counts, n)
		}
	}

	// A CORRELATED subquery referencing the duplicate outer alias
	// resolves per-attribute across ALL same-aliased outer legs. qid is
	// unique to the SECOND leg: the inner reference must bind it (never
	// 42703 from an alias-collapsed outer-scope map, never the wrong leg).
	if got, err := queryInts(t, "SELECT a.qid FROM p AS a, q AS a WHERE EXISTS (SELECT 1 FROM p AS z WHERE z.id = a.qid)"); err != nil {
		t.Errorf("correlated second-leg outer dup must ANSWER per-attribute: %v", err)
	} else if !reflect.DeepEqual(got, []int64{1, 1}) {
		t.Errorf("correlated second-leg rows = %v, want [1 1] (qid=1 matches p.id=1; qid=7 matches nothing)", got)
	}
	// First-leg control: v unique to p.
	if got, err := queryInts(t, "SELECT a.v FROM p AS a, q AS a WHERE EXISTS (SELECT 1 FROM q AS z WHERE a.v = 10)"); err != nil {
		t.Errorf("correlated first-leg outer dup must ANSWER: %v", err)
	} else if !reflect.DeepEqual(got, []int64{10, 10}) {
		t.Errorf("correlated first-leg rows = %v, want [10 10]", got)
	}
	// Ambiguous inner reference over duplicate outer legs is TERMINAL 42702
	// (the ladder's ≥2 arm at correlation depth): id lives on BOTH p legs.
	var x any
	if err := db.QueryRowContext(ctx,
		"SELECT 1 FROM p AS a, p AS a WHERE EXISTS (SELECT 1 FROM q AS z WHERE a.id = 1)").Scan(&x); err == nil {
		t.Errorf("ambiguous correlated outer dup must reject 42702, got rows")
	} else if !strings.Contains(err.Error(), "Ambiguous reference A.ID") {
		t.Errorf("ambiguous correlated outer dup error = %v, want Ambiguous reference A.ID", err)
	}
}
