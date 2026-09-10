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
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
)

type oracleRow struct {
	id      int64
	a, b, c *int64
	s       *string
	v       *int64
}

func oracleInt(v int64) *int64 { return &v }

func oracleGenRows(n int) []oracleRow {
	rng := rand.New(rand.NewPCG(7, 11))
	out := make([]oracleRow, 0, n)
	pick := func(max int64, nullEvery int) *int64 {
		if rng.IntN(nullEvery) == 0 {
			return nil
		}
		return oracleInt(int64(rng.IntN(int(max))) + 1)
	}
	for i := 1; i <= n; i++ {
		r := oracleRow{id: int64(i)}
		r.a = pick(5, 9)
		r.b = pick(6, 9)
		r.c = pick(4, 9)
		if rng.IntN(9) != 0 {
			s := []string{"x", "y", "z"}[rng.IntN(3)]
			r.s = &s
		}
		if rng.IntN(9) != 0 {
			r.v = oracleInt(int64(rng.IntN(51)))
		}
		out = append(out, r)
	}
	return out
}

func (r oracleRow) insertSQL() string {
	lit := func(p *int64) string {
		if p == nil {
			return "NULL"
		}
		return fmt.Sprint(*p)
	}
	s := "NULL"
	if r.s != nil {
		s = "'" + *r.s + "'"
	}
	return fmt.Sprintf("(%d, %s, %s, %s, %s, %s)", r.id, lit(r.a), lit(r.b), lit(r.c), s, lit(r.v))
}

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
	pred  func(oracleRow) bool
	keys  func(oracleRow) []oracleKey // nil = unordered
	limit int
}

func oracleVal(p *int64) (int64, bool) {
	if p == nil {
		return 0, false
	}
	return *p, true
}

func oracleEq(p *int64, v int64) bool { x, ok := oracleVal(p); return ok && x == v }
func oracleGt(p *int64, v int64) bool { x, ok := oracleVal(p); return ok && x > v }
func oracleGe(p *int64, v int64) bool { x, ok := oracleVal(p); return ok && x >= v }
func oracleLt(p *int64, v int64) bool { x, ok := oracleVal(p); return ok && x < v }
func oracleLe(p *int64, v int64) bool { x, ok := oracleVal(p); return ok && x <= v }
func oracleNe(p *int64, v int64) bool { x, ok := oracleVal(p); return ok && x != v }
func oracleIn(p *int64, vs ...int64) bool {
	x, ok := oracleVal(p)
	if !ok {
		return false
	}
	for _, v := range vs {
		if v == x {
			return true
		}
	}
	return false
}

// TestFDB_OrderingLimitOracle — see the file comment.
func TestFDB_OrderingLimitOracle(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ordoracle")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ordoracle")
	mwjoMustExec(t, setup, ctx,
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
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ordoracle/s WITH TEMPLATE ordoracle")
	dsn := fmt.Sprintf("fdbsql:///testdb_ordoracle?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rows := oracleGenRows(300)
	for i := 0; i < len(rows); i += 50 {
		end := i + 50
		if end > len(rows) {
			end = len(rows)
		}
		var parts []string
		for _, r := range rows[i:end] {
			parts = append(parts, r.insertSQL())
		}
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b, c, s, v) VALUES "+strings.Join(parts, ","))
	}
	explain := mwjoExplainer(t, db, ctx)

	asc := func(p *int64) oracleKey { return oracleKey{v: p} }
	desc := func(p *int64) oracleKey { return oracleKey{v: p, desc: true} }
	idK := func(r oracleRow) oracleKey { return oracleKey{v: oracleInt(r.id)} }
	idD := func(r oracleRow) oracleKey { return oracleKey{v: oracleInt(r.id), desc: true} }

	cases := []oracleCase{
		{"a_eq_order_b", "SELECT id FROM t WHERE a = 1 ORDER BY b", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"a_eq_order_b_desc", "SELECT id FROM t WHERE a = 1 ORDER BY b DESC", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b)} }, 0},
		{"a_eq_order_b_desc_limit", "SELECT id FROM t WHERE a = 1 ORDER BY b DESC LIMIT 3", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b)} }, 3},
		{"a_eq_order_b_nulls_last", "SELECT id FROM t WHERE a = 1 ORDER BY b NULLS LAST", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{{v: r.b, nullLast: true}} }, 0},
		{"a_eq_order_b_c", "SELECT id FROM t WHERE a = 1 ORDER BY b, c", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), asc(r.c)} }, 0},
		{"a_eq_order_b_c_desc", "SELECT id FROM t WHERE a = 1 ORDER BY b DESC, c DESC", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b), desc(r.c)} }, 0},
		{"a_eq_order_b_c_desc_limit", "SELECT id FROM t WHERE a = 1 ORDER BY b DESC, c DESC LIMIT 4", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b), desc(r.c)} }, 4},
		{"a_eq_order_b_asc_c_desc", "SELECT id FROM t WHERE a = 1 ORDER BY b ASC, c DESC", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), desc(r.c)} }, 0},
		{"ab_eq_order_c", "SELECT id FROM t WHERE a = 1 AND b = 2 ORDER BY c", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleEq(r.b, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.c)} }, 0},
		{"ab_eq_order_c_desc_limit", "SELECT id FROM t WHERE a = 1 AND b = 2 ORDER BY c DESC LIMIT 2", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleEq(r.b, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.c)} }, 2},
		{"a_eq_b_gt_order_c", "SELECT id FROM t WHERE a = 1 AND b > 2 ORDER BY c", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleGt(r.b, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.c)} }, 0},
		{"a_eq_b_gt_order_b_c", "SELECT id FROM t WHERE a = 1 AND b > 2 ORDER BY b, c", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleGt(r.b, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), asc(r.c)} }, 0},
		{"a_eq_b_gt_order_b_c_desc_limit", "SELECT id FROM t WHERE a = 1 AND b > 2 ORDER BY b DESC, c DESC LIMIT 3", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleGt(r.b, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b), desc(r.c)} }, 3},
		{"a_gt_b_eq_order_a", "SELECT id FROM t WHERE a > 1 AND b = 2 ORDER BY a", func(r oracleRow) bool { return oracleGt(r.a, 1) && oracleEq(r.b, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a)} }, 0},
		{"a_in_order_b", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"a_in_order_b_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b LIMIT 5", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 5},
		{"a_in_order_b_desc_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b DESC LIMIT 5", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b)} }, 5},
		{"a_in_order_a_b", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY a, b", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a), asc(r.b)} }, 0},
		{"a_in_order_a_desc_b_desc", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY a DESC, b DESC", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.a), desc(r.b)} }, 0},
		{"a_in_order_a_desc_b_desc_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY a DESC, b DESC LIMIT 7", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.a), desc(r.b)} }, 7},
		{"a_in_order_b_id", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b, id", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), idK(r)} }, 0},
		{"a_in_order_b_id_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b, id LIMIT 6", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), idK(r)} }, 6},
		{"a_in_order_b_desc_id_desc_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b DESC, id DESC LIMIT 6", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b), idD(r)} }, 6},
		{"a_in_order_b_c", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b, c", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), asc(r.c)} }, 0},
		{"a_in_order_b_c_limit", "SELECT id FROM t WHERE a IN (3, 1, 2) ORDER BY b, c LIMIT 5", func(r oracleRow) bool { return oracleIn(r.a, 3, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), asc(r.c)} }, 5},
		{"a_eq_b_in_order_c", "SELECT id FROM t WHERE a = 1 AND b IN (5, 4) ORDER BY c", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleIn(r.b, 5, 4) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.c)} }, 0},
		{"a_eq_b_in_order_c_limit", "SELECT id FROM t WHERE a = 1 AND b IN (5, 4) ORDER BY c LIMIT 2", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleIn(r.b, 5, 4) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.c)} }, 2},
		{"a_eq_b_in_order_b_c", "SELECT id FROM t WHERE a = 1 AND b IN (5, 4) ORDER BY b, c", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleIn(r.b, 5, 4) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), asc(r.c)} }, 0},
		{"a_in_b_in_order_c", "SELECT id FROM t WHERE a IN (1, 2) AND b IN (5, 4) ORDER BY c", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleIn(r.b, 5, 4) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.c)} }, 0},
		{"a_in_b_in_order_c_limit", "SELECT id FROM t WHERE a IN (1, 2) AND b IN (5, 4) ORDER BY c LIMIT 3", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleIn(r.b, 5, 4) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.c)} }, 3},
		{"a_in_b_in_order_a_b_c", "SELECT id FROM t WHERE a IN (1, 2) AND b IN (5, 4) ORDER BY a, b, c", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleIn(r.b, 5, 4) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a), asc(r.b), asc(r.c)} }, 0},
		{"a_in_b_gt_order_b", "SELECT id FROM t WHERE a IN (1, 2) AND b > 3 ORDER BY b", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleGt(r.b, 3) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"a_in_b_gt_order_b_desc_limit", "SELECT id FROM t WHERE a IN (1, 2) AND b > 3 ORDER BY b DESC LIMIT 4", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleGt(r.b, 3) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b)} }, 4},
		{"a_in_b_gt_order_b_c_limit", "SELECT id FROM t WHERE a IN (1, 2) AND b > 3 ORDER BY b, c LIMIT 4", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleGt(r.b, 3) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), asc(r.c)} }, 4},
		{"or_order_id", "SELECT id FROM t WHERE a = 1 OR c = 2 ORDER BY id", func(r oracleRow) bool { return oracleEq(r.a, 1) || oracleEq(r.c, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{idK(r)} }, 0},
		{"or_order_id_limit", "SELECT id FROM t WHERE a = 1 OR c = 2 ORDER BY id LIMIT 5", func(r oracleRow) bool { return oracleEq(r.a, 1) || oracleEq(r.c, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{idK(r)} }, 5},
		{"or_same_col_order_b", "SELECT id FROM t WHERE a = 1 OR a = 2 ORDER BY b", func(r oracleRow) bool { return oracleEq(r.a, 1) || oracleEq(r.a, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"or_conj_order_c", "SELECT id FROM t WHERE (a = 1 AND b = 2) OR (a = 3 AND b = 4) ORDER BY c", func(r oracleRow) bool {
			return (oracleEq(r.a, 1) && oracleEq(r.b, 2)) || (oracleEq(r.a, 3) && oracleEq(r.b, 4))
		}, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.c)} }, 0},
		{"a_eq_c_eq_order_b", "SELECT id FROM t WHERE a = 1 AND c = 2 ORDER BY b", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleEq(r.c, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"a_eq_c_eq_order_id", "SELECT id FROM t WHERE a = 1 AND c = 2 ORDER BY id", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleEq(r.c, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{idK(r)} }, 0},
		{"a_eq_c_eq_order_id_desc_limit", "SELECT id FROM t WHERE a = 1 AND c = 2 ORDER BY id DESC LIMIT 3", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleEq(r.c, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{idD(r)} }, 3},
		{"a_null_order_b", "SELECT id FROM t WHERE a IS NULL ORDER BY b", func(r oracleRow) bool { return r.a == nil }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"a_null_b_gt_order_b_desc", "SELECT id FROM t WHERE a IS NULL AND b > 1 ORDER BY b DESC", func(r oracleRow) bool { return r.a == nil && oracleGt(r.b, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b)} }, 0},
		{"a_notnull_order_a", "SELECT id FROM t WHERE a IS NOT NULL ORDER BY a", func(r oracleRow) bool { return r.a != nil }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a)} }, 0},
		{"a_notnull_order_a_desc_limit", "SELECT id FROM t WHERE a IS NOT NULL ORDER BY a DESC, b DESC LIMIT 5", func(r oracleRow) bool { return r.a != nil }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.a), desc(r.b)} }, 5},
		{"a_between_order_a", "SELECT id FROM t WHERE a BETWEEN 2 AND 4 ORDER BY a", func(r oracleRow) bool { return oracleGe(r.a, 2) && oracleLe(r.a, 4) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a)} }, 0},
		{"a_between_order_a_desc_limit", "SELECT id FROM t WHERE a BETWEEN 2 AND 4 ORDER BY a DESC LIMIT 4", func(r oracleRow) bool { return oracleGe(r.a, 2) && oracleLe(r.a, 4) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.a)} }, 4},
		{"a_eq_b_between_order_b_desc", "SELECT id FROM t WHERE a = 1 AND b BETWEEN 2 AND 5 ORDER BY b DESC", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleGe(r.b, 2) && oracleLe(r.b, 5) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b)} }, 0},
		{"a_eq_b_range_c_eq_order_b", "SELECT id FROM t WHERE a = 1 AND b >= 2 AND b < 5 AND c = 1 ORDER BY b", func(r oracleRow) bool {
			return oracleEq(r.a, 1) && oracleGe(r.b, 2) && oracleLt(r.b, 5) && oracleEq(r.c, 1)
		}, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"a_ne_order_a", "SELECT id FROM t WHERE a <> 1 ORDER BY a", func(r oracleRow) bool { return oracleNe(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a)} }, 0},
		{"a_eq_b_ne_order_b", "SELECT id FROM t WHERE a = 1 AND b <> 2 ORDER BY b", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleNe(r.b, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"contradiction", "SELECT id FROM t WHERE a = 1 AND a = 2", func(r oracleRow) bool { return false }, nil, 0},
		{"a_eq_a_gt_order_b", "SELECT id FROM t WHERE a = 1 AND a > 0 ORDER BY b", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"a_gt_a_gt_order_a", "SELECT id FROM t WHERE a > 1 AND a > 2 ORDER BY a", func(r oracleRow) bool { return oracleGt(r.a, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a)} }, 0},
		{"empty_range", "SELECT id FROM t WHERE a > 5 AND a < 3", func(r oracleRow) bool { return false }, nil, 0},
		{"order_a_id", "SELECT id FROM t ORDER BY a, id", func(r oracleRow) bool { return true }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a), idK(r)} }, 0},
		{"order_a_id_limit", "SELECT id FROM t ORDER BY a, id LIMIT 10", func(r oracleRow) bool { return true }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a), idK(r)} }, 10},
		{"order_a_b_c_id_limit", "SELECT id FROM t ORDER BY a, b, c, id LIMIT 10", func(r oracleRow) bool { return true }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a), asc(r.b), asc(r.c), idK(r)} }, 10},
		{"order_a_desc_b_desc_limit", "SELECT id FROM t ORDER BY a DESC, b DESC LIMIT 10", func(r oracleRow) bool { return true }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.a), desc(r.b)} }, 10},
		{"a_eq_order_b_id_desc", "SELECT id FROM t WHERE a = 1 ORDER BY b, id DESC", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), idD(r)} }, 0},
		{"a_eq_order_b_offset", "SELECT id FROM t WHERE a = 1 ORDER BY b, id LIMIT 4 OFFSET 3", func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b), idK(r)} }, -4},
		{"s_eq_a_eq_order_b", "SELECT id FROM t WHERE a = 1 AND s = 'x' ORDER BY b", func(r oracleRow) bool { return oracleEq(r.a, 1) && r.s != nil && *r.s == "x" }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"s_in_order_s", "SELECT id FROM t WHERE s IN ('x', 'z') ORDER BY id", func(r oracleRow) bool { return r.s != nil && (*r.s == "x" || *r.s == "z") }, func(r oracleRow) []oracleKey { return []oracleKey{idK(r)} }, 0},
		{"a_in_c_eq_order_b", "SELECT id FROM t WHERE a IN (1, 2) AND c = 3 ORDER BY b", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleEq(r.c, 3) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"a_in_c_eq_order_b_limit", "SELECT id FROM t WHERE a IN (1, 2) AND c = 3 ORDER BY b LIMIT 3", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleEq(r.c, 3) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 3},
		{"a_in_c_eq_order_id_limit", "SELECT id FROM t WHERE a IN (1, 2) AND c = 3 ORDER BY id LIMIT 3", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleEq(r.c, 3) }, func(r oracleRow) []oracleKey { return []oracleKey{idK(r)} }, 3},
		{"a_in_dup_order_b", "SELECT id FROM t WHERE a IN (2, 2, 1) ORDER BY b", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"a_in_dup_order_b_limit", "SELECT id FROM t WHERE a IN (2, 2, 1) ORDER BY b LIMIT 5", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 5},
		{"not_in", "SELECT id FROM t WHERE a NOT IN (1, 2) ORDER BY a", func(r oracleRow) bool { x, ok := oracleVal(r.a); return ok && x != 1 && x != 2 }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a)} }, 0},
		{"not_a_eq", "SELECT id FROM t WHERE NOT (a = 1) ORDER BY a DESC LIMIT 5", func(r oracleRow) bool { x, ok := oracleVal(r.a); return ok && x != 1 }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.a)} }, 5},
		{"not_a_in_b_eq", "SELECT id FROM t WHERE NOT (a IN (1, 2)) AND b = 3 ORDER BY a", func(r oracleRow) bool { x, ok := oracleVal(r.a); return ok && x != 1 && x != 2 && oracleEq(r.b, 3) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.a)} }, 0},
		{"a_eq_or_b_null", "SELECT id FROM t WHERE a = 1 OR b IS NULL ORDER BY id", func(r oracleRow) bool { return oracleEq(r.a, 1) || r.b == nil }, func(r oracleRow) []oracleKey { return []oracleKey{idK(r)} }, 0},
		{"a_eq_and_or", "SELECT id FROM t WHERE a = 1 AND (b = 2 OR c = 3) ORDER BY b", func(r oracleRow) bool { return oracleEq(r.a, 1) && (oracleEq(r.b, 2) || oracleEq(r.c, 3)) }, func(r oracleRow) []oracleKey { return []oracleKey{asc(r.b)} }, 0},
		{"a_eq_and_or_limit", "SELECT id FROM t WHERE a = 1 AND (b = 2 OR c = 3) ORDER BY b DESC LIMIT 3", func(r oracleRow) bool { return oracleEq(r.a, 1) && (oracleEq(r.b, 2) || oracleEq(r.c, 3)) }, func(r oracleRow) []oracleKey { return []oracleKey{desc(r.b)} }, 3},
		{"a_in_or_c_in", "SELECT id FROM t WHERE a IN (1, 2) OR c IN (3, 4) ORDER BY id LIMIT 8", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) || oracleIn(r.c, 3, 4) }, func(r oracleRow) []oracleKey { return []oracleKey{idK(r)} }, 8},
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
			var exp []oracleRow
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
					expSet[r.id]++
				}
				if fmt.Sprint(gotSet) != fmt.Sprint(expSet) {
					t.Errorf("row set mismatch\n sql: %s\n plan: %s\n got %v\n exp %v", tc.sql, plan, got, expIDs(exp))
				}
				return
			}
			sort.SliceStable(exp, func(i, j int) bool { return oracleCmpKeys(tc.keys(exp[i]), tc.keys(exp[j])) < 0 })
			byID := map[int64]oracleRow{}
			for _, r := range rows {
				byID[r.id] = r
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
				expSet[r.id] = true
			}
			for i, g := range got {
				r, ok := byID[g]
				if !ok || !expSet[g] {
					t.Errorf("unexpected row %d\n sql: %s\n plan: %s\n got %v\n exp %v", g, tc.sql, plan, got, expIDs(want))
					return
				}
				if c := oracleCmpKeys(tc.keys(r), tc.keys(want[i])); c != 0 {
					t.Errorf("key mismatch at position %d (row %d vs expected row %d)\n sql: %s\n plan: %s\n got %v\n exp %v", i, g, want[i].id, tc.sql, plan, got, expIDs(want))
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

func expIDs(rs []oracleRow) []int64 {
	out := make([]int64, len(rs))
	for i, r := range rs {
		out[i] = r.id
	}
	return out
}
