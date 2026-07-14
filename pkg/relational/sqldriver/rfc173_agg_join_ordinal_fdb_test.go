package sqldriver_test

// RFC-173 regression pin for the STREAMING-AGGREGATE-over-JOIN INPUT edge: a GROUP
// BY / aggregate whose input is a 2-way ordinal join's MERGED positional row. The
// group key / aggregate operand is a QUALIFIED leg reference (D.DNAME, E.SALARY);
// before this slice the aggregate cursor resolved those by NAME off the merged
// Datum map (aggregateInputIsPlainScan walks only layout-preserving single-source
// wrappers, never a join, so the plain-scan positional arm did not fire). Now the
// aggregate reads its keys / operands off the merged PositionalRow through
// legWindowRowContext — the SAME spanAwareRow leg-window resolver that projection /
// filter over the same join merge already use (executeProjection / executeFilter).
//
// The name-keyed Datum row reader this aggregate-over-join input used to resolve
// against has been DELETED, so ordinalization is now structural: the aggregate
// reads its keys / operands off the merged PositionalRow through legWindowRowContext
// — the SAME leg-window resolver projection / filter over the join already use.
// This test exercises the representative aggregate-over-join shapes and asserts they
// return the correct rows. The schema is unique to this test, so no cached plan is
// shared.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_RFC173_AggregateJoinOrdinal(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggjoin_ord")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggjoin_ord")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggjoin_ord "+
			"CREATE TABLE dept (did BIGINT NOT NULL, dname STRING, PRIMARY KEY (did)) "+
			"CREATE TABLE emp (eid BIGINT NOT NULL, did BIGINT, salary BIGINT, PRIMARY KEY (eid))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggjoin_ord/s WITH TEMPLATE aggjoin_ord")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggjoin_ord?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// dept: eng(1), sales(2) ; emp: (10,1,100),(20,1,90),(30,2,80)
	mwjoMustExec(t, db, ctx,
		"INSERT INTO dept (did, dname) VALUES (1, 'eng'), (2, 'sales')")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO emp (eid, did, salary) VALUES (10, 1, 100), (20, 1, 90), (30, 2, 80)")

	// strPairs collects (string,int64) rows sorted for order-independent comparison.
	strPairs := func(t *testing.T, q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a string
			var b int64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, fmt.Sprintf("%s=%d", a, b))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}

	// Comma-join, qualified group key D.DNAME, COUNT(*) (reads only the key).
	t.Run("comma_join_group_key_count_star", func(t *testing.T) {
		if got := strPairs(t, "SELECT d.dname, COUNT(*) FROM dept d, emp e WHERE e.did = d.did GROUP BY d.dname"); got != "eng=2 sales=1" {
			t.Errorf("d.dname,COUNT(*) = %q, want %q", got, "eng=2 sales=1")
		}
	})

	// Comma-join, qualified group key + qualified aggregate operand E.SALARY.
	t.Run("comma_join_group_key_and_operand_sum", func(t *testing.T) {
		if got := strPairs(t, "SELECT d.dname, SUM(e.salary) FROM dept d, emp e WHERE e.did = d.did GROUP BY d.dname"); got != "eng=190 sales=80" {
			t.Errorf("d.dname,SUM(e.salary) = %q, want %q", got, "eng=190 sales=80")
		}
	})

	// Explicit INNER JOIN ... ON, qualified operand under MAX.
	t.Run("inner_join_on_max_operand", func(t *testing.T) {
		if got := strPairs(t, "SELECT d.dname, MAX(e.salary) FROM dept d JOIN emp e ON e.did = d.did GROUP BY d.dname"); got != "eng=100 sales=80" {
			t.Errorf("d.dname,MAX(e.salary) = %q, want %q", got, "eng=100 sales=80")
		}
	})

	// Explicit INNER JOIN ... ON, MIN operand.
	t.Run("inner_join_on_min_operand", func(t *testing.T) {
		if got := strPairs(t, "SELECT d.dname, MIN(e.salary) FROM dept d JOIN emp e ON e.did = d.did GROUP BY d.dname"); got != "eng=90 sales=80" {
			t.Errorf("d.dname,MIN(e.salary) = %q, want %q", got, "eng=90 sales=80")
		}
	})

	// UNQUALIFIED group key `dname` (unambiguous — only dept has it): exercises the
	// spanAwareRow bare-name path (no dot) over the merged row rather than the
	// alias-qualified leg window.
	t.Run("comma_join_unqualified_group_key", func(t *testing.T) {
		if got := strPairs(t, "SELECT dname, COUNT(*) FROM dept d, emp e WHERE e.did = d.did GROUP BY dname"); got != "eng=2 sales=1" {
			t.Errorf("dname,COUNT(*) = %q, want %q", got, "eng=2 sales=1")
		}
	})

	// Compound operand SUM(e.salary + d.did): leaf columns come from DIFFERENT legs,
	// so each must window to its own leg (D.DID vs E.SALARY) off the merged row.
	t.Run("compound_operand_cross_leg", func(t *testing.T) {
		// eng(did=1): (100+1)+(90+1)=192 ; sales(did=2): (80+2)=82
		if got := strPairs(t, "SELECT d.dname, SUM(e.salary + d.did) FROM dept d, emp e WHERE e.did = d.did GROUP BY d.dname"); got != "eng=192 sales=82" {
			t.Errorf("d.dname,SUM(e.salary+d.did) = %q, want %q", got, "eng=192 sales=82")
		}
	})
}
