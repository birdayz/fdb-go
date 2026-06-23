package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// gojDB sets up emp + dept for GROUP-BY-over-join tests. Groups by dept:
//
//	eng (did=1): Alice(100), Bob(90) → COUNT 2, MAX 100, SUM 190
//	sales (did=2): Charlie(80)       → COUNT 1, MAX 80,  SUM 80
func gojDB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/goj_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "goj_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE dept (did BIGINT, dname STRING, PRIMARY KEY (did))"+
		" CREATE TABLE emp (eid BIGINT, did BIGINT, ename STRING, salary BIGINT, PRIMARY KEY (eid))"); err != nil {
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
	if _, err := db.ExecContext(ctx, "INSERT INTO dept VALUES (1,'eng'),(2,'sales')"); err != nil {
		t.Fatalf("seed dept: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO emp VALUES (10,1,'Alice',100),(20,1,'Bob',90),(30,2,'Charlie',80)"); err != nil {
		t.Fatalf("seed emp: %v", err)
	}
	return db, ctx
}

type gojRow struct {
	dname string
	cnt   int64
	mx    int64
}

func gojRead(t *testing.T, ctx context.Context, db *sql.DB, q string) []gojRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var got []gojRow
	for rows.Next() {
		var r gojRow
		if err := rows.Scan(&r.dname, &r.cnt, &r.mx); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	return got
}

// TestFDB_GroupByOverJoin pins that GROUP BY over a join with a group key from a
// joined table works (was 42703: validateGroupByProjection only knew the first
// table's columns, so a joined-table group key failed the existence check).
// Covers INNER JOIN and comma-join, qualified and bare group keys.
func TestFDB_GroupByOverJoin(t *testing.T) {
	t.Parallel()
	db, ctx := gojDB(t, "core")

	want := []gojRow{{"eng", 2, 100}, {"sales", 1, 80}}
	check := func(name, q string) {
		t.Helper()
		got := gojRead(t, ctx, db, q)
		if len(got) != len(want) {
			t.Fatalf("%s: got %d rows %+v, want %+v", name, len(got), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s row %d: got %+v, want %+v", name, i, got[i], want[i])
			}
		}
	}

	check("inner_join_qualified",
		"SELECT d.dname, COUNT(*), MAX(e.salary) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY d.dname ORDER BY d.dname")
	check("comma_join_qualified",
		"SELECT d.dname, COUNT(*), MAX(e.salary) FROM emp AS e, dept AS d WHERE e.did = d.did GROUP BY d.dname ORDER BY d.dname")
	check("inner_join_bare_key",
		"SELECT dname, COUNT(*), MAX(e.salary) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY dname ORDER BY dname")
}

// TestFDB_GroupByOverJoin_SumHavingMultiKey covers the other aggregate/shape
// axes over a join: SUM, HAVING on the grouped output, and a multi-key GROUP BY
// mixing a joined-table key and a first-table key.
func TestFDB_GroupByOverJoin_SumHavingMultiKey(t *testing.T) {
	t.Parallel()
	db, ctx := gojDB(t, "shmk")

	// SUM over join: eng→190, sales→80.
	rows, err := db.QueryContext(ctx,
		"SELECT d.dname, SUM(e.salary) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY d.dname ORDER BY d.dname")
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	sums := map[string]int64{}
	for rows.Next() {
		var dn string
		var s int64
		if err := rows.Scan(&dn, &s); err != nil {
			t.Fatalf("sum scan: %v", err)
		}
		sums[dn] = s
	}
	rows.Close()
	if sums["eng"] != 190 || sums["sales"] != 80 {
		t.Fatalf("SUM over join = %v, want eng:190 sales:80", sums)
	}

	// HAVING on the grouped join output: only eng has COUNT(*) > 1.
	hrows, err := db.QueryContext(ctx,
		"SELECT d.dname, COUNT(*) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY d.dname HAVING COUNT(*) > 1 ORDER BY d.dname")
	if err != nil {
		t.Fatalf("having: %v", err)
	}
	var hnames []string
	for hrows.Next() {
		var dn string
		var c int64
		if err := hrows.Scan(&dn, &c); err != nil {
			t.Fatalf("having scan: %v", err)
		}
		hnames = append(hnames, dn)
	}
	hrows.Close()
	if len(hnames) != 1 || hnames[0] != "eng" {
		t.Fatalf("HAVING COUNT(*)>1 over join = %v, want [eng]", hnames)
	}

	// Multi-key GROUP BY mixing joined-table key (d.dname) + first-table key
	// (e.did) — each (dname,did) pair groups; here did==dept so 2 groups.
	mrows, err := db.QueryContext(ctx,
		"SELECT d.dname, e.did, COUNT(*) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY d.dname, e.did ORDER BY d.dname")
	if err != nil {
		t.Fatalf("multikey: %v", err)
	}
	n := 0
	for mrows.Next() {
		n++
	}
	mrows.Close()
	if n != 2 {
		t.Fatalf("multi-key GROUP BY over join = %d groups, want 2", n)
	}
}

// TestFDB_AggOverJoin_EmptyGroupHaving pins the Java 4.12 empty-implicit-group
// behaviour on the CASCADES path for an ungrouped COUNT(*) over an EMPTY join:
// the implicit single group (COUNT=0) is formed and HAVING then filters it —
// `HAVING COUNT(*) >= 0` keeps the [0] row, `HAVING COUNT(*) > 5` filters it
// out. Java 4.11 had a bug (it treated empty input as "no grouping at all" so
// HAVING never fired and returned 0 rows); 4.12 fixed it. The corpus pins the
// single-table shape (agg_empty_count_having_passes /
// having_count_star_eq_zero_empty); this is the join variant, end-to-end through
// the driver. Every SELECT now goes through the Cascades generator — the legacy
// embedded aggregate executor was removed in RFC-145 (there is no second path).
func TestFDB_AggOverJoin_EmptyGroupHaving(t *testing.T) {
	t.Parallel()
	db, ctx := gojDB(t, "emptyhaving")

	// Empty join result (no emp has salary > 99999) → implicit group COUNT=0.
	// HAVING COUNT(*) >= 0 passes → exactly one row [0]. Revert-proof: the old
	// 4.11-aligned guard skipped the synthetic group, so Scan would get
	// sql.ErrNoRows here.
	var n int64
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did WHERE e.salary > 99999 HAVING COUNT(*) >= 0").
		Scan(&n); err != nil {
		t.Fatalf("empty-group HAVING >=0 over join: want one [0] row (Java 4.12), got error %v", err)
	}
	if n != 0 {
		t.Fatalf("empty-group HAVING >=0 over join: COUNT(*) = %d, want 0", n)
	}

	// HAVING COUNT(*) > 5 filters the [0] implicit group back out → 0 rows.
	// Proves the fix EMITS the group and lets evalHaving filter it (not an
	// unconditional emit).
	rows, err := db.QueryContext(ctx,
		"SELECT COUNT(*) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did WHERE e.salary > 99999 HAVING COUNT(*) > 5")
	if err != nil {
		t.Fatalf("empty-group HAVING >5 over join: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		var got int64
		_ = rows.Scan(&got)
		t.Fatalf("empty-group HAVING >5 over join: want 0 rows (the [0] group fails HAVING), got a row COUNT=%d", got)
	}
}

// TestFDB_GroupByOverJoin_FirstTableKey guards the converse: grouping by a
// FIRST-table column over a join still works (no regression from widening the
// validated field set).
func TestFDB_GroupByOverJoin_FirstTableKey(t *testing.T) {
	t.Parallel()
	db, ctx := gojDB(t, "first")
	// Group by emp.did (first table); did=1 → 2 rows max 100, did=2 → 1 row max 80.
	got := gojRead(t, ctx, db,
		"SELECT CAST(e.did AS STRING), COUNT(*), MAX(e.salary) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY e.did ORDER BY e.did")
	want := []gojRow{{"1", 2, 100}, {"2", 1, 80}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("first-table key: got %+v, want %+v", got, want)
	}
}

// TestFDB_GroupByOverJoin_UndefinedKeyStillRejects guards that a genuinely
// undefined GROUP BY column over a join still errors (the existence check isn't
// silently disabled for joins).
func TestFDB_GroupByOverJoin_UndefinedKeyStillRejects(t *testing.T) {
	t.Parallel()
	db, ctx := gojDB(t, "undef")
	_, err := db.ExecContext(ctx,
		"SELECT nosuchcol, COUNT(*) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY nosuchcol")
	if err == nil {
		t.Fatal("GROUP BY a column that exists in NEITHER joined table should still error, not be silently accepted")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *api.Error: %T %v", err, err)
	}
	if apiErr.Code != api.ErrCodeUndefinedColumn {
		t.Fatalf("undefined GROUP BY column over join: code = %s, want %s (42703)", apiErr.Code, api.ErrCodeUndefinedColumn)
	}
}

// TestFDB_GroupByWrongQualifierRejected pins the load-bearing invariant behind
// validateGroupByProjection's deliberately qualifier-blind existence check: the
// check compares only the BARE name against the UNION of all join sources, so
// `d.salary` (SALARY lives on emp, not the qualified dept) passes that coarse
// gate — and is SAFE only because every call site is BRACKETED by a precise
// semantic resolver gate that overrides it (top-level: resolver runs BEFORE;
// correlated-scalar: resolver runs AFTER — see the validateGroupByProjection
// invariant comment). Covers BOTH call sites (Graefe: verify the correlated-scalar
// site is equally resolver-gated; codex: the existence-check dimension the suite
// missed).
func TestFDB_GroupByWrongQualifierRejected(t *testing.T) {
	t.Parallel()
	db, ctx := gojDB(t, "wrongqual")

	// (1) Top-level GROUP BY path. d.salary: bare SALARY is in union(emp,dept),
	// so the bare-name check passes; the resolver (resolveColumnName, ~L1002)
	// must reject the wrong qualifier with 42703.
	_, err := db.ExecContext(ctx,
		"SELECT d.salary, COUNT(*) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY d.salary")
	if err == nil {
		t.Fatal("top-level: GROUP BY d.salary (salary is on emp, not dept) must be rejected, not silently bound to emp.salary")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("top-level: error is not *api.Error: %T %v", err, err)
	}
	if apiErr.Code != api.ErrCodeUndefinedColumn {
		t.Fatalf("top-level wrong-qualifier GROUP BY: code = %s, want %s (42703)", apiErr.Code, api.ErrCodeUndefinedColumn)
	}

	// (2) Correlated-scalar-subquery path. Here validateGroupByProjection (~L4414)
	// runs FIRST and the GROUP-BY-key resolver gate (resolveCorrelatedGroupKeyValues,
	// ~L4654) runs AFTER — the inverse ordering of the top-level path. The inner
	// subquery has a single source (dept d2), so d2.salary is a genuinely-absent
	// column on dept and the resolver gate must reject it BY NAME (proving it's the
	// GROUP-BY-key resolution catching it, not a generic parse failure).
	//
	// The richer "SALARY is in the join union but wrong-qualified" shape — the
	// inverse of part (1) — is NOT reachable at this site: a correlated scalar
	// subquery that itself contains an INNER JOIN + GROUP BY is unsupported (0A000),
	// rejected before any GROUP-BY validation. So the cross-table mis-bind cannot
	// arise here; the single-source absent-column rejection is the gate that
	// matters, and the qualifier-blind union check (which only ever sees one
	// source here) can't false-accept anything the resolver wouldn't also catch.
	_, err2 := db.ExecContext(ctx,
		"SELECT e.eid, (SELECT COUNT(*) FROM dept AS d2 WHERE d2.did = e.did GROUP BY d2.salary) FROM emp AS e")
	if err2 == nil {
		t.Fatal("correlated: inner GROUP BY d2.salary (salary not on dept) must be rejected")
	}
	if !strings.Contains(strings.ToUpper(err2.Error()), "SALARY") {
		t.Fatalf("correlated: expected the GROUP-BY-key resolver to name the absent column, got %v", err2)
	}
}
