package sqldriver_test

// Ordering / LIMIT / IN-list oracle battery over real FDB. Every query is
// answered by a Go oracle over the same generated rows (NULLs included) and
// checked for the row SET, the ORDER where the query asks for one (a partial
// order is checked key by key, a LIMIT as a valid top-k prefix, an OFFSET as
// the window after it), and duplicates. The shapes are the ones the
// generative rowdiff corpus does not reach: ORDER BY over a composite index
// with an equality prefix, DESC and NULLS placement, IN-union and IN-join
// under ORDER BY and LIMIT, OR-unions, two-sided ranges, contradictions.
// Written as a Cascades bug hunt's proof that these return correct rows; it
// stays so a regression on any of those axes is caught by the row check, not
// only by the plan pins.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// oracleKey is one ORDER BY key: nil = NULL. nullsLast flips NULL placement.
type oracleKey struct {
	v        *int64
	desc     bool
	nullLast bool
}

func oracleCmpKey(x, y oracleKey) int {
	// Returns -1 if x sorts before y.
	if x.v == nil && y.v == nil {
		return 0
	}
	nullFirst := !x.desc // FDB: NULL smallest → first under ASC, last under DESC
	if x.nullLast {
		nullFirst = false
	}
	if x.desc && x.nullLast {
		nullFirst = false
	}
	if x.v == nil {
		if nullFirst {
			return -1
		}
		return 1
	}
	if y.v == nil {
		if nullFirst {
			return 1
		}
		return -1
	}
	c := 0
	if *x.v < *y.v {
		c = -1
	} else if *x.v > *y.v {
		c = 1
	}
	if x.desc {
		c = -c
	}
	return c
}

func oracleCmpKeys(x, y []oracleKey) int {
	for i := range x {
		if c := oracleCmpKey(x[i], y[i]); c != 0 {
			return c
		}
	}
	return 0
}

type oracleCase struct {
	name  string
	sql   string
	pred  func(testkit.OracleRow) bool
	keys  func(testkit.OracleRow) []oracleKey // nil = unordered
	limit int
}

func oracleNe(p *int64, v int64) bool { x, ok := testkit.OracleVal(p); return ok && x != v }

// TestFDB_OrderingLimitOracle — see the file comment.
func TestFDB_OrderingLimitOracle(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_ordoracle")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_ordoracle")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ordoracle "+
			"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_a ON t (a) "+
			"CREATE INDEX idx_ab ON t (a, b) "+
			"CREATE INDEX idx_abc ON t (a, b, c) "+
			"CREATE INDEX idx_c ON t (c) "+
			"CREATE INDEX idx_s ON t (s) "+
			"CREATE INDEX cnt_by_a AS SELECT COUNT(*) FROM t GROUP BY a "+
			"CREATE INDEX sum_v_by_ab AS SELECT SUM(v) FROM t GROUP BY a, b "+
			"CREATE INDEX max_v_by_a AS SELECT MAX(v) FROM t GROUP BY a")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_ordoracle/s WITH TEMPLATE ordoracle")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_ORDORACLE?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rows := testkit.OracleGenRows(300)
	for i := 0; i < len(rows); i += 50 {
		end := i + 50
		if end > len(rows) {
			end = len(rows)
		}
		var parts []string
		for _, r := range rows[i:end] {
			parts = append(parts, r.InsertSQL())
		}
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO t (id, a, b, c, s, v) VALUES "+strings.Join(parts, ","))
	}
	explain := testkit.Explainer(t, db, ctx)

	asc := func(p *int64) oracleKey { return oracleKey{v: p} }
	desc := func(p *int64) oracleKey { return oracleKey{v: p, desc: true} }
	idK := func(r testkit.OracleRow) oracleKey { return oracleKey{v: testkit.OracleInt(r.ID)} }
	idD := func(r testkit.OracleRow) oracleKey { return oracleKey{v: testkit.OracleInt(r.ID), desc: true} }

	cases := []oracleCase{
		{"a_eq_order_b", "SELECT id FROM t WHERE a = 1 ORDER BY b", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"a_eq_order_b_desc", "SELECT id FROM t WHERE a = 1 ORDER BY b DESC", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B)} }, 0},
		{"a_eq_order_b_desc_limit", "SELECT id FROM t WHERE a = 1 ORDER BY b DESC LIMIT 3", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B)} }, 3},
		{"a_eq_order_b_nulls_last", "SELECT id FROM t WHERE a = 1 ORDER BY b NULLS LAST", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{{v: r.B, nullLast: true}} }, 0},
		{"a_eq_order_b_c", "SELECT id FROM t WHERE a = 1 ORDER BY b, c", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), asc(r.C)} }, 0},
		{"a_eq_order_b_c_desc", "SELECT id FROM t WHERE a = 1 ORDER BY b DESC, c DESC", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B), desc(r.C)} }, 0},
		{"a_eq_order_b_c_desc_limit", "SELECT id FROM t WHERE a = 1 ORDER BY b DESC, c DESC LIMIT 4", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B), desc(r.C)} }, 4},
		{"a_eq_order_b_asc_c_desc", "SELECT id FROM t WHERE a = 1 ORDER BY b ASC, c DESC", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), desc(r.C)} }, 0},
		{"ab_eq_order_c", "SELECT id FROM t WHERE a = 1 AND b = 2 ORDER BY c", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleEq(r.B, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.C)} }, 0},
		{"ab_eq_order_c_desc_limit", "SELECT id FROM t WHERE a = 1 AND b = 2 ORDER BY c DESC LIMIT 2", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleEq(r.B, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.C)} }, 2},
		{"a_eq_b_gt_order_c", "SELECT id FROM t WHERE a = 1 AND b > 2 ORDER BY c", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleGt(r.B, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.C)} }, 0},
		{"a_eq_b_gt_order_b_c", "SELECT id FROM t WHERE a = 1 AND b > 2 ORDER BY b, c", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleGt(r.B, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), asc(r.C)} }, 0},
		{"a_eq_b_gt_order_b_c_desc_limit", "SELECT id FROM t WHERE a = 1 AND b > 2 ORDER BY b DESC, c DESC LIMIT 3", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleGt(r.B, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B), desc(r.C)} }, 3},
		{"a_gt_b_eq_order_a", "SELECT id FROM t WHERE a > 1 AND b = 2 ORDER BY a", func(r testkit.OracleRow) bool { return testkit.OracleGt(r.A, 1) && testkit.OracleEq(r.B, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A)} }, 0},
		{"a_in_order_b", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"a_in_order_b_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b LIMIT 5", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 5},
		{"a_in_order_b_desc_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b DESC LIMIT 5", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B)} }, 5},
		{"a_in_order_a_b", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY a, b", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A), asc(r.B)} }, 0},
		{"a_in_order_a_desc_b_desc", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY a DESC, b DESC", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.A), desc(r.B)} }, 0},
		{"a_in_order_a_desc_b_desc_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY a DESC, b DESC LIMIT 7", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.A), desc(r.B)} }, 7},
		{"a_in_order_b_id", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b, id", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), idK(r)} }, 0},
		{"a_in_order_b_id_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b, id LIMIT 6", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), idK(r)} }, 6},
		{"a_in_order_b_desc_id_desc_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b DESC, id DESC LIMIT 6", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B), idD(r)} }, 6},
		{"a_in_order_b_c", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b, c", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), asc(r.C)} }, 0},
		{"a_in_order_b_c_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b, c LIMIT 5", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 3, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), asc(r.C)} }, 5},
		{"a_eq_b_in_order_c", "SELECT id FROM t WHERE a = 1 AND b IN (5, 4) ORDER BY c", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleIn(r.B, 5, 4) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.C)} }, 0},
		{"a_eq_b_in_order_c_limit", "SELECT id FROM t WHERE a = 1 AND b IN (5, 4) ORDER BY c LIMIT 2", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleIn(r.B, 5, 4) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.C)} }, 2},
		{"a_eq_b_in_order_b_c", "SELECT id FROM t WHERE a = 1 AND b IN (5, 4) ORDER BY b, c", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleIn(r.B, 5, 4) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), asc(r.C)} }, 0},
		{"a_in_b_in_order_c", "SELECT id FROM t WHERE a IN (1, 2) AND b IN (5, 4) ORDER BY c", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) && testkit.OracleIn(r.B, 5, 4) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.C)} }, 0},
		{"a_in_b_in_order_c_limit", "SELECT id FROM t WHERE a IN (1, 2) AND b IN (5, 4) ORDER BY c LIMIT 3", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) && testkit.OracleIn(r.B, 5, 4) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.C)} }, 3},
		{"a_in_b_in_order_a_b_c", "SELECT id FROM t WHERE a IN (1, 2) AND b IN (5, 4) ORDER BY a, b, c", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) && testkit.OracleIn(r.B, 5, 4) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A), asc(r.B), asc(r.C)} }, 0},
		{"a_in_b_gt_order_b", "SELECT id FROM t WHERE a IN (1, 2) AND b > 3 ORDER BY b", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) && testkit.OracleGt(r.B, 3) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"a_in_b_gt_order_b_desc_limit", "SELECT id FROM t WHERE a IN (1, 2) AND b > 3 ORDER BY b DESC LIMIT 4", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) && testkit.OracleGt(r.B, 3) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B)} }, 4},
		{"a_in_b_gt_order_b_c_limit", "SELECT id FROM t WHERE a IN (1, 2) AND b > 3 ORDER BY b, c LIMIT 4", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) && testkit.OracleGt(r.B, 3) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), asc(r.C)} }, 4},
		{"or_order_id", "SELECT id FROM t WHERE a = 1 OR c = 2 ORDER BY id", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) || testkit.OracleEq(r.C, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{idK(r)} }, 0},
		{"or_order_id_limit", "SELECT id FROM t WHERE a = 1 OR c = 2 ORDER BY id LIMIT 5", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) || testkit.OracleEq(r.C, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{idK(r)} }, 5},
		{"or_same_col_order_b", "SELECT id FROM t WHERE a = 1 OR a = 2 ORDER BY b", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) || testkit.OracleEq(r.A, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"or_conj_order_c", "SELECT id FROM t WHERE (a = 1 AND b = 2) OR (a = 3 AND b = 4) ORDER BY c", func(r testkit.OracleRow) bool {
			return (testkit.OracleEq(r.A, 1) && testkit.OracleEq(r.B, 2)) || (testkit.OracleEq(r.A, 3) && testkit.OracleEq(r.B, 4))
		}, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.C)} }, 0},
		{"a_eq_c_eq_order_b", "SELECT id FROM t WHERE a = 1 AND c = 2 ORDER BY b", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleEq(r.C, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"a_eq_c_eq_order_id", "SELECT id FROM t WHERE a = 1 AND c = 2 ORDER BY id", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleEq(r.C, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{idK(r)} }, 0},
		{"a_eq_c_eq_order_id_desc_limit", "SELECT id FROM t WHERE a = 1 AND c = 2 ORDER BY id DESC LIMIT 3", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && testkit.OracleEq(r.C, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{idD(r)} }, 3},
		{"a_null_order_b", "SELECT id FROM t WHERE a IS NULL ORDER BY b", func(r testkit.OracleRow) bool { return r.A == nil }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"a_null_b_gt_order_b_desc", "SELECT id FROM t WHERE a IS NULL AND b > 1 ORDER BY b DESC", func(r testkit.OracleRow) bool { return r.A == nil && testkit.OracleGt(r.B, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B)} }, 0},
		{"a_notnull_order_a", "SELECT id FROM t WHERE a IS NOT NULL ORDER BY a", func(r testkit.OracleRow) bool { return r.A != nil }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A)} }, 0},
		{"a_notnull_order_a_desc_limit", "SELECT id FROM t WHERE a IS NOT NULL ORDER BY a DESC, b DESC LIMIT 5", func(r testkit.OracleRow) bool { return r.A != nil }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.A), desc(r.B)} }, 5},
		{"a_between_order_a", "SELECT id FROM t WHERE a BETWEEN 2 AND 4 ORDER BY a", func(r testkit.OracleRow) bool { return testkit.OracleGe(r.A, 2) && testkit.OracleLe(r.A, 4) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A)} }, 0},
		{"a_between_order_a_desc_limit", "SELECT id FROM t WHERE a BETWEEN 2 AND 4 ORDER BY a DESC LIMIT 4", func(r testkit.OracleRow) bool { return testkit.OracleGe(r.A, 2) && testkit.OracleLe(r.A, 4) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.A)} }, 4},
		{"a_eq_b_between_order_b_desc", "SELECT id FROM t WHERE a = 1 AND b BETWEEN 2 AND 5 ORDER BY b DESC", func(r testkit.OracleRow) bool {
			return testkit.OracleEq(r.A, 1) && testkit.OracleGe(r.B, 2) && testkit.OracleLe(r.B, 5)
		}, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B)} }, 0},
		{"a_eq_b_range_c_eq_order_b", "SELECT id FROM t WHERE a = 1 AND b >= 2 AND b < 5 AND c = 1 ORDER BY b", func(r testkit.OracleRow) bool {
			return testkit.OracleEq(r.A, 1) && testkit.OracleGe(r.B, 2) && testkit.OracleLt(r.B, 5) && testkit.OracleEq(r.C, 1)
		}, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"a_ne_order_a", "SELECT id FROM t WHERE a <> 1 ORDER BY a", func(r testkit.OracleRow) bool { return oracleNe(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A)} }, 0},
		{"a_eq_b_ne_order_b", "SELECT id FROM t WHERE a = 1 AND b <> 2 ORDER BY b", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && oracleNe(r.B, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"contradiction", "SELECT id FROM t WHERE a = 1 AND a = 2", func(r testkit.OracleRow) bool { return false }, nil, 0},
		{"a_eq_a_gt_order_b", "SELECT id FROM t WHERE a = 1 AND a > 0 ORDER BY b", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"a_gt_a_gt_order_a", "SELECT id FROM t WHERE a > 1 AND a > 2 ORDER BY a", func(r testkit.OracleRow) bool { return testkit.OracleGt(r.A, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A)} }, 0},
		{"empty_range", "SELECT id FROM t WHERE a > 5 AND a < 3", func(r testkit.OracleRow) bool { return false }, nil, 0},
		{"order_a_id", "SELECT id FROM t ORDER BY a, id", func(r testkit.OracleRow) bool { return true }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A), idK(r)} }, 0},
		{"order_a_id_limit", "SELECT id FROM t ORDER BY a, id LIMIT 10", func(r testkit.OracleRow) bool { return true }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A), idK(r)} }, 10},
		{"order_a_b_c_id_limit", "SELECT id FROM t ORDER BY a, b, c, id LIMIT 10", func(r testkit.OracleRow) bool { return true }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A), asc(r.B), asc(r.C), idK(r)} }, 10},
		{"order_a_desc_b_desc_limit", "SELECT id FROM t ORDER BY a DESC, b DESC LIMIT 10", func(r testkit.OracleRow) bool { return true }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.A), desc(r.B)} }, 10},
		{"a_eq_order_b_id_desc", "SELECT id FROM t WHERE a = 1 ORDER BY b, id DESC", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), idD(r)} }, 0},
		{"a_eq_order_b_offset", "SELECT id FROM t WHERE a = 1 ORDER BY b, id LIMIT 4 OFFSET 3", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B), idK(r)} }, -4},
		{"s_eq_a_eq_order_b", "SELECT id FROM t WHERE a = 1 AND s = 'x' ORDER BY b", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) && r.S != nil && *r.S == "x" }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"s_in_order_s", "SELECT id FROM t WHERE s IN ('x', 'z') ORDER BY id", func(r testkit.OracleRow) bool { return r.S != nil && (*r.S == "x" || *r.S == "z") }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{idK(r)} }, 0},
		{"a_in_c_eq_order_b", "SELECT id FROM t WHERE a IN (1, 2) AND c = 3 ORDER BY b", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) && testkit.OracleEq(r.C, 3) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"a_in_c_eq_order_b_limit", "SELECT id FROM t WHERE a IN (1, 2) AND c = 3 ORDER BY b LIMIT 3", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) && testkit.OracleEq(r.C, 3) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 3},
		{"a_in_c_eq_order_id_limit", "SELECT id FROM t WHERE a IN (1, 2) AND c = 3 ORDER BY id LIMIT 3", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) && testkit.OracleEq(r.C, 3) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{idK(r)} }, 3},
		{"a_in_dup_order_b", "SELECT id FROM t WHERE a IN (2, 2, 1) ORDER BY b", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"a_in_dup_order_b_limit", "SELECT id FROM t WHERE a IN (2, 2, 1) ORDER BY b LIMIT 5", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 5},
		{"not_in", "SELECT id FROM t WHERE a NOT IN (1, 2) ORDER BY a", func(r testkit.OracleRow) bool { x, ok := testkit.OracleVal(r.A); return ok && x != 1 && x != 2 }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A)} }, 0},
		{"not_a_eq", "SELECT id FROM t WHERE NOT (a = 1) ORDER BY a DESC LIMIT 5", func(r testkit.OracleRow) bool { x, ok := testkit.OracleVal(r.A); return ok && x != 1 }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.A)} }, 5},
		{"not_a_in_b_eq", "SELECT id FROM t WHERE NOT (a IN (1, 2)) AND b = 3 ORDER BY a", func(r testkit.OracleRow) bool {
			x, ok := testkit.OracleVal(r.A)
			return ok && x != 1 && x != 2 && testkit.OracleEq(r.B, 3)
		}, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.A)} }, 0},
		{"a_eq_or_b_null", "SELECT id FROM t WHERE a = 1 OR b IS NULL ORDER BY id", func(r testkit.OracleRow) bool { return testkit.OracleEq(r.A, 1) || r.B == nil }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{idK(r)} }, 0},
		{"a_eq_and_or", "SELECT id FROM t WHERE a = 1 AND (b = 2 OR c = 3) ORDER BY b", func(r testkit.OracleRow) bool {
			return testkit.OracleEq(r.A, 1) && (testkit.OracleEq(r.B, 2) || testkit.OracleEq(r.C, 3))
		}, func(r testkit.OracleRow) []oracleKey { return []oracleKey{asc(r.B)} }, 0},
		{"a_eq_and_or_limit", "SELECT id FROM t WHERE a = 1 AND (b = 2 OR c = 3) ORDER BY b DESC LIMIT 3", func(r testkit.OracleRow) bool {
			return testkit.OracleEq(r.A, 1) && (testkit.OracleEq(r.B, 2) || testkit.OracleEq(r.C, 3))
		}, func(r testkit.OracleRow) []oracleKey { return []oracleKey{desc(r.B)} }, 3},
		{"a_in_or_c_in", "SELECT id FROM t WHERE a IN (1, 2) OR c IN (3, 4) ORDER BY id LIMIT 8", func(r testkit.OracleRow) bool { return testkit.OracleIn(r.A, 1, 2) || testkit.OracleIn(r.C, 3, 4) }, func(r testkit.OracleRow) []oracleKey { return []oracleKey{idK(r)} }, 8},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explain(tc.sql)
			rs, err := db.QueryContext(ctx, tc.sql)
			if err != nil {
				t.Fatalf("query %q: %v", tc.sql, err)
			}
			defer rs.Close()
			var got []int64
			for rs.Next() {
				var v int64
				if err := rs.Scan(&v); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, v)
			}
			if err := rs.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			var exp []testkit.OracleRow
			for _, r := range rows {
				if tc.pred(r) {
					exp = append(exp, r)
				}
			}
			if tc.keys == nil {
				gotSet := map[int64]int{}
				for _, g := range got {
					gotSet[g]++
				}
				expSet := map[int64]int{}
				for _, r := range exp {
					expSet[r.ID]++
				}
				if fmt.Sprint(gotSet) != fmt.Sprint(expSet) {
					t.Errorf("row set mismatch\n sql: %s\n plan: %s\n got %v\n exp %v", tc.sql, plan, got, expIDs(exp))
				}
				return
			}
			sort.SliceStable(exp, func(i, j int) bool { return oracleCmpKeys(tc.keys(exp[i]), tc.keys(exp[j])) < 0 })
			byID := map[int64]testkit.OracleRow{}
			for _, r := range rows {
				byID[r.ID] = r
			}
			// Validate: got rows ⊆ exp, keys monotone, and the got key sequence
			// equals the first len(got) expected keys (a valid top-k prefix), and
			// without LIMIT the whole set matches.
			offset := 0
			limit := tc.limit
			if limit < 0 {
				offset = 3
				limit = -limit
			}
			want := exp
			if offset > 0 {
				if offset > len(want) {
					want = nil
				} else {
					want = want[offset:]
				}
			}
			if limit > 0 && len(want) > limit {
				want = want[:limit]
			}
			if len(got) != len(want) {
				t.Errorf("row count mismatch: got %d want %d\n sql: %s\n plan: %s\n got %v\n exp %v", len(got), len(want), tc.sql, plan, got, expIDs(want))
				return
			}
			expSet := map[int64]bool{}
			for _, r := range exp {
				expSet[r.ID] = true
			}
			for i, g := range got {
				r, ok := byID[g]
				if !ok || !expSet[g] {
					t.Errorf("unexpected row %d\n sql: %s\n plan: %s\n got %v\n exp %v", g, tc.sql, plan, got, expIDs(want))
					return
				}
				if c := oracleCmpKeys(tc.keys(r), tc.keys(want[i])); c != 0 {
					t.Errorf("key mismatch at position %d (row %d vs expected row %d)\n sql: %s\n plan: %s\n got %v\n exp %v", i, g, want[i].ID, tc.sql, plan, got, expIDs(want))
					return
				}
			}
			if limit == 0 && offset == 0 {
				gotSet := map[int64]bool{}
				for _, g := range got {
					gotSet[g] = true
				}
				if len(gotSet) != len(expSet) {
					t.Errorf("duplicate rows\n sql: %s\n plan: %s\n got %v", tc.sql, plan, got)
				}
			}
			t.Logf("ok (%d rows) plan: %s", len(got), plan)
		})
	}
}

func expIDs(rs []testkit.OracleRow) []int64 {
	out := make([]int64, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
