package sqldriver_test

// RFC-173 S4 :2908/:3033 — projected EXISTS over a BURIED INNER box
// `(p JOIN q) JOIN r`. The 3-way `p JOIN q ... JOIN r ...` associates left, so
// the fold's left leg is the `(p JOIN q)` INNER cluster — a buried gated box.
//
// An INNER box is merge-transparent (SelectMergeRule flattens INNER/CROSS but
// never OUTER), so under AXIS-1 the box dissolves into the fold and the fold
// becomes a flat `[ForEach(p), ForEach(q), ForEach(r), Existential]` select —
// the N-WAY FLAT EXISTENTIAL shape. The N-way arm of implementJoinWithExistential
// plans it: a left-deep INNER cross-product NLJ chain (each level seeded so it
// births a positional merged row the next level reads through its buried-leaf
// windows), all join predicates as ONE merged-row filter, then the existential
// FlatMap fold. Java 4.12.11.0 folds `(p JOIN q) JOIN r` under projected EXISTS
// and answers [[10 true]]; the N-way arm now matches.
//
// The DISCRIMINATING probe below (dup-named `k` columns across p/q/r, a p.k
// projection AND a p.k EXISTS correlation, data where binding to q.k or r.k
// flips BOTH) guards the NewAnchoredJoinRecord last-leg-wins hazard: the buried
// leg must bind by its own [Start,Width) window, not by a bare last-wins key.

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_RFC173S4_BuriedInnerBoxProjectedExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173s4bibx"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173s4bibx_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE r (rid BIGINT, PRIMARY KEY (rid))"+
		" CREATE TABLE e (eid BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173s4bibx_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO p VALUES (1, 10), (2, 20)",
		"INSERT INTO q VALUES (1)",
		"INSERT INTO r VALUES (1)",
		"INSERT INTO e VALUES (1)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// (p JOIN q ON q.qid = p.id) = {(id=1, v=10)}; JOIN r ON r.rid = p.id keeps
	// id=1. EXISTS(e.eid = p.id=1) → true. Java answers [[10 true]].
	sqlText := "SELECT p.v, EXISTS (SELECT 1 FROM e WHERE e.eid = p.id) " +
		"FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id"
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		t.Fatalf("N-way flat existential fold must plan now, got: %v", err)
	}
	defer rows.Close()
	var got [][2]any
	for rows.Next() {
		var v int64
		var ex sql.NullBool
		if scanErr := rows.Scan(&v, &ex); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, [2]any{v, ex.Valid && ex.Bool})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := [][2]any{{int64(10), true}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("buried INNER box projected EXISTS rows = %v, want %v", got, want)
	}
}

// TestFDB_RFC173S4_BuriedInnerBoxProjectedExists_Discriminating is the
// buried-leg correctness discriminator. p, q, r all carry a column named `k`
// (a merged-row duplicate-name hazard); the projection AND the EXISTS
// correlation both read p.k. Data is chosen so a mis-bind to q.k or r.k (the
// NewAnchoredJoinRecord last-leg-wins hazard) flips BOTH the projected value
// and the EXISTS boolean — the buried p leg must resolve by its own
// [Start,Width) window, not a bare last-wins key. Java 4.12.11.0 semantics
// (standard SQL INNER joins + projected EXISTS): [[7 true] [8 false]].
func TestFDB_RFC173S4_BuriedInnerBoxProjectedExists_Discriminating(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173s4bibxd"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173s4bibxd_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, k BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE r (rid BIGINT, k BIGINT, PRIMARY KEY (rid))"+
		" CREATE TABLE e (eid BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173s4bibxd_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		// p.k is the DISCRIMINATOR: 7 (has a matching e) and 8 (no match).
		"INSERT INTO p VALUES (1, 100, 7), (2, 200, 8)",
		// q.k = 8 for BOTH join rows; r.k = 9 for both. A mis-bind of p.k to
		// q.k reads 8 for every row (both exists false); to r.k reads 9.
		"INSERT INTO q VALUES (1, 8), (2, 8)",
		"INSERT INTO r VALUES (1, 9), (2, 9)",
		// e only holds eid=7 → EXISTS(e.eid = p.k) is true ONLY for p.k=7.
		"INSERT INTO e VALUES (7)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// Both rows join (q.qid=p.id, r.rid=p.id hold for id=1,2). The projection
	// reads p.k; the EXISTS correlates on p.k. Correct binding to p.k:
	//   id=1: p.k=7 → EXISTS(e.eid=7)=true  → [7 true]
	//   id=2: p.k=8 → EXISTS(e.eid=8)=false → [8 false]
	// A mis-bind to q.k (=8) or r.k (=9) would flip the projected value AND
	// the EXISTS boolean — a loud, distinguishable wrong answer.
	sqlText := "SELECT p.k, EXISTS (SELECT 1 FROM e WHERE e.eid = p.k) " +
		"FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id"
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		t.Fatalf("discriminating N-way fold must plan, got: %v", err)
	}
	defer rows.Close()
	type row struct {
		k  int64
		ex bool
	}
	var got []row
	for rows.Next() {
		var k int64
		var ex sql.NullBool
		if scanErr := rows.Scan(&k, &ex); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, row{k, ex.Valid && ex.Bool})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].k < got[j].k })
	want := []row{{7, true}, {8, false}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("discriminating buried-leg rows = %v, want %v"+
			" (a wrong value/exists means p.k mis-bound to q.k/r.k — the"+
			" buried last-leg-wins hazard)", got, want)
	}
}

// TestFDB_RFC173S4_NWayWhereExists pins the WHERE-EXISTS sibling the relaxed
// dispatch also enables: `[ForEach×3, Existential]` reaches the N-way arm with a
// NON-projected result value (a plain WHERE EXISTS filter, not a fold). The RFC
// notes one change closes TWO residuals — the projected fold AND the
// WHERE-EXISTS flatten. Java 4.12.11.0: only p.id=1 has a matching e → [10].
func TestFDB_RFC173S4_NWayWhereExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173s4nwe"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173s4nwe_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE r (rid BIGINT, PRIMARY KEY (rid))"+
		" CREATE TABLE e (eid BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173s4nwe_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO p VALUES (1, 10), (2, 20)",
		"INSERT INTO q VALUES (1), (2)",
		"INSERT INTO r VALUES (1), (2)",
		"INSERT INTO e VALUES (1)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	sqlText := "SELECT p.v FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id " +
		"WHERE EXISTS (SELECT 1 FROM e WHERE e.eid = p.id)"
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		t.Fatalf("N-way WHERE-EXISTS must plan, got: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var v int64
		if scanErr := rows.Scan(&v); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 1 || got[0] != 10 {
		t.Fatalf("N-way WHERE-EXISTS rows = %v, want [10]", got)
	}
}

// TestFDB_RFC173S4_NWayExistsInnerJoin pins the correctness of a projected EXISTS
// whose INNER is itself an explicit JOIN..ON, under the N-way outer fold. The
// inner join's ON predicate MUST be enforced: e.eid=p.id matches, but the inner
// `e JOIN f ON f.fid=e.fid` has NO matching f → the inner join is EMPTY → EXISTS
// is FALSE. A regression here (EXISTS=true) means the inner ON predicate was
// dropped — a silent wrong answer (the shape declined cleanly before the N-way
// arm existed). Java 4.12.11.0: [[10 false]].
func TestFDB_RFC173S4_NWayExistsInnerJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173s4nweij"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173s4nweij_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE r (rid BIGINT, PRIMARY KEY (rid))"+
		" CREATE TABLE e (eid BIGINT, fid BIGINT, PRIMARY KEY (eid))"+
		" CREATE TABLE f (fid BIGINT, PRIMARY KEY (fid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173s4nweij_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO p VALUES (1, 10)",
		"INSERT INTO q VALUES (1)",
		"INSERT INTO r VALUES (1)",
		// e.eid=1 matches p.id=1, but e.fid=99 has NO matching f (f.fid=88).
		"INSERT INTO e VALUES (1, 99)",
		"INSERT INTO f VALUES (88)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	sqlText := "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON f.fid = e.fid WHERE e.eid = p.id) " +
		"FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id"
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		// Only a CLEAN PLANNER DECLINE (0AF00) is acceptable (correct-or-
		// conservative); a parser/binder/runtime error or any other SQLSTATE would
		// mask the intended fail-closed behavior, so assert 0AF00 specifically.
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			if string(apiErr.Code) != "0AF00" {
				t.Fatalf("N-way EXISTS-inner-join errored with SQLSTATE %q, want a clean 0AF00 planner decline (or correct rows): %v", apiErr.Code, err)
			}
			return
		}
		if !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("N-way EXISTS-inner-join errored non-0AF00 (want a clean planner decline or correct rows): %v", err)
		}
		return
	}
	defer rows.Close()
	var got [][2]any
	for rows.Next() {
		var v int64
		var ex sql.NullBool
		if scanErr := rows.Scan(&v, &ex); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, [2]any{v, ex.Valid && ex.Bool})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := [][2]any{{int64(10), false}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("N-way EXISTS-inner-join rows = %v, want %v (EXISTS=true means the inner"+
			" `e JOIN f ON f.fid=e.fid` ON predicate was DROPPED — a silent wrong answer)", got, want)
	}
}

// TestFDB_RFC173S4_NWayNotExists pins the N-way arm's NEGATED existential residual
// (the `negated → ComparisonIsNull` branch): a `WHERE NOT EXISTS` over a 3-way
// fold. Polarity discriminator: id=1 has a matching e (EXISTS true → NOT EXISTS
// false → EXCLUDED); id=2 has no match (EXISTS false → NOT EXISTS true → KEPT).
// Correct → [20]. An inverted polarity (NOT EXISTS treated as EXISTS) → [10] —
// a loud, distinguishable silent-wrong. Java 4.12.11.0: [20].
func TestFDB_RFC173S4_NWayNotExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173s4nwne"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173s4nwne_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE r (rid BIGINT, PRIMARY KEY (rid))"+
		" CREATE TABLE e (eid BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173s4nwne_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO p VALUES (1, 10), (2, 20)",
		"INSERT INTO q VALUES (1), (2)",
		"INSERT INTO r VALUES (1), (2)",
		"INSERT INTO e VALUES (1)", // only id=1 has a match
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	sqlText := "SELECT p.v FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id " +
		"WHERE NOT EXISTS (SELECT 1 FROM e WHERE e.eid = p.id)"
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		t.Fatalf("N-way WHERE NOT EXISTS must plan, got: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var v int64
		if scanErr := rows.Scan(&v); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 1 || got[0] != 20 {
		t.Fatalf("N-way NOT EXISTS rows = %v, want [20] (a [10] means the NOT-EXISTS"+
			" polarity was INVERTED — a silent wrong answer)", got)
	}
}

// TestFDB_RFC173S4_FourLegDiscriminating earns the "N-way" name past N=3: a
// 4-source fold `((p JOIN q) JOIN r) JOIN s` exercises a DEPTH-3 buried-leaf
// window level and a second fresh-alias accumulation the 3-leg case never
// reaches. All four legs carry a column `k`; the projection AND the EXISTS both
// read the DEEPEST first leg p.k. Data (p.k∈{7,8}, q.k=8, r.k=9, s.k=6, e={7})
// chosen so a mis-bind to ANY of q.k/r.k/s.k flips both → distinct wrong sets.
// Java 4.12.11.0: [[7 true],[8 false]].
func TestFDB_RFC173S4_FourLegDiscriminating(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173s4fld"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173s4fld_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, k BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE r (rid BIGINT, k BIGINT, PRIMARY KEY (rid))"+
		" CREATE TABLE s (sid BIGINT, k BIGINT, PRIMARY KEY (sid))"+
		" CREATE TABLE e (eid BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173s4fld_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO p VALUES (1, 100, 7), (2, 200, 8)",
		"INSERT INTO q VALUES (1, 8), (2, 8)",
		"INSERT INTO r VALUES (1, 9), (2, 9)",
		"INSERT INTO s VALUES (1, 6), (2, 6)",
		"INSERT INTO e VALUES (7)", // EXISTS(e.eid=p.k) true only for p.k=7
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	sqlText := "SELECT p.k, EXISTS (SELECT 1 FROM e WHERE e.eid = p.k) " +
		"FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id JOIN s ON s.sid = p.id"
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		t.Fatalf("4-leg discriminating fold must plan, got: %v", err)
	}
	defer rows.Close()
	type row struct {
		k  int64
		ex bool
	}
	var got []row
	for rows.Next() {
		var k int64
		var ex sql.NullBool
		if scanErr := rows.Scan(&k, &ex); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, row{k, ex.Valid && ex.Bool})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].k < got[j].k })
	want := []row{{7, true}, {8, false}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("4-leg discriminating rows = %v, want %v (a wrong value/exists means p.k"+
			" mis-bound to q.k/r.k/s.k — depth-3 buried-leaf windowing broke)", got, want)
	}
}
