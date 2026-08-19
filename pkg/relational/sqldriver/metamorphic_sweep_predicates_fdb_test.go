package sqldriver_test

// Metamorphic bug hunter: runs the SAME query text against two schemas that
// hold IDENTICAL data and differ ONLY in which secondary indexes exist. Any
// row-set difference is a planner/index-matching defect, because the answer to
// a query may not depend on the presence of an index.
//
// Second oracle (TLP): for any predicate P, the id sets of `WHERE P`,
// `WHERE NOT P` and `WHERE P IS NULL` must partition the table exactly.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
)

type mhDB struct {
	name string
	db   *sql.DB
}

func mhScanIDs(ctx context.Context, db *sql.DB, q string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v sql.NullInt64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if !v.Valid {
			out = append(out, -999999)
			continue
		}
		out = append(out, v.Int64)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func mhScanStrings(ctx context.Context, db *sql.DB, q string) ([]string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			v := c.(*sql.NullString)
			if v.Valid {
				parts[i] = v.String
			} else {
				parts[i] = "NULL"
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- random predicate generation -------------------------------------------

// mhGen generates random predicates. The column names are parameters so the
// same generator can drive fixtures with different schemas; a generator that
// emits a column the table does not have produces a 42703 on BOTH sides, which
// is an EMPTY comparison, not a passing one.
type mhGen struct {
	r *rand.Rand
	// intCols/dblCols/strCols/boolCols default (when nil) to the hunt-1 fixture.
	intCols, dblCols, strCols, boolCols []string
}

func (g *mhGen) ints() []string {
	if g.intCols == nil {
		return []string{"a", "b", "id"}
	}
	return g.intCols
}

func (g *mhGen) dbls() []string {
	if g.dblCols == nil {
		return []string{"c"}
	}
	return g.dblCols
}

func (g *mhGen) strs() []string {
	if g.strCols == nil {
		return []string{"s"}
	}
	return g.strCols
}

func (g *mhGen) bools() []string {
	if g.boolCols == nil {
		return []string{"f"}
	}
	return g.boolCols
}

var (
	mhIntLits = []string{"-2", "-1", "0", "1", "2", "3", "10"}
	mhDblLits = []string{"-1.5", "-0.0", "0.0", "1.5", "2.25", "3.0"}
	mhStrLits = []string{"''", "'a'", "'ab'", "'b'", "'B'", "'abc'", "'z'"}
)

func (g *mhGen) pick(ss []string) string { return ss[g.r.Intn(len(ss))] }

func (g *mhGen) intExpr(depth int) string {
	if depth <= 0 || g.r.Intn(3) == 0 {
		if g.r.Intn(2) == 0 {
			return g.pick(g.ints())
		}
		return g.pick(mhIntLits)
	}
	op := g.pick([]string{"+", "-", "*"})
	return "(" + g.intExpr(depth-1) + " " + op + " " + g.intExpr(depth-1) + ")"
}

func (g *mhGen) cmpOp() string {
	return g.pick([]string{"=", "<>", "<", "<=", ">", ">="})
}

func (g *mhGen) atom(depth int) string {
	all := append(append(append(append([]string{}, g.ints()...), g.dbls()...), g.strs()...), g.bools()...)
	switch g.r.Intn(10) {
	case 0, 1:
		return g.pick(g.ints()) + " " + g.cmpOp() + " " + g.pick(mhIntLits)
	case 2:
		if len(g.dbls()) == 0 {
			return g.pick(g.ints()) + " " + g.cmpOp() + " " + g.pick(mhIntLits)
		}
		return g.pick(g.dbls()) + " " + g.cmpOp() + " " + g.pick(mhDblLits)
	case 3:
		return g.pick(g.strs()) + " " + g.cmpOp() + " " + g.pick(mhStrLits)
	case 4:
		return g.pick(all) + " IS " + g.pick([]string{"", "NOT "}) + "NULL"
	case 5:
		return g.pick(g.ints()) + " IN (" + g.pick(mhIntLits) + ", " + g.pick(mhIntLits) + ", " + g.pick(mhIntLits) + ")"
	case 6:
		lo, hi := g.pick(mhIntLits), g.pick(mhIntLits)
		return g.pick(g.ints()) + " BETWEEN " + lo + " AND " + hi
	case 7:
		return g.pick(g.strs()) + " LIKE " + g.pick([]string{"'a%'", "'%b'", "'%a%'", "'_b'", "'a_'", "''"})
	case 8:
		return g.intExpr(2) + " " + g.cmpOp() + " " + g.intExpr(2)
	default:
		if len(g.bools()) == 0 {
			return g.pick(g.ints()) + " " + g.cmpOp() + " " + g.pick(mhIntLits)
		}
		if g.r.Intn(2) == 0 {
			return g.pick(g.bools())
		}
		return "NOT " + g.pick(g.bools())
	}
}

func (g *mhGen) pred(depth int) string {
	if depth <= 0 {
		return g.atom(2)
	}
	switch g.r.Intn(5) {
	case 0:
		return "(NOT " + g.pred(depth-1) + ")"
	case 1, 2:
		return "(" + g.pred(depth-1) + " AND " + g.pred(depth-1) + ")"
	case 3:
		return "(" + g.pred(depth-1) + " OR " + g.pred(depth-1) + ")"
	default:
		return g.atom(2)
	}
}

// ---- fixture ----------------------------------------------------------------

const mhCols = "(id, a, b, c, s, f)"

// mhRowLiteral builds one fixture row. bNeverNull keeps column b NULL-free,
// which the aggregate sweep needs: b is the column its SUM indexes aggregate,
// and SUM over a group that loses its last non-NULL value is a KNOWN divergence
// (see sumResidualZero). Letting the sweep wander into it would report a defect
// that is already pinned, every run, and drown anything new.
func mhRowLiteral(r *rand.Rand, id int, bNeverNull bool) string {
	nul := func(p int, gen func() string) string {
		if r.Intn(100) < p {
			return "NULL"
		}
		return gen()
	}
	a := nul(18, func() string { return fmt.Sprintf("%d", r.Intn(7)-2) })
	bNullPct := 18
	if bNeverNull {
		bNullPct = 0
	}
	b := nul(bNullPct, func() string { return fmt.Sprintf("%d", r.Intn(4)) })
	c := nul(18, func() string {
		return []string{"-1.5", "-0.0", "0.0", "1.5", "2.25", "3.0"}[r.Intn(6)]
	})
	s := nul(18, func() string {
		return []string{"''", "'a'", "'ab'", "'b'", "'B'", "'abc'", "'z'", "'aa'"}[r.Intn(8)]
	})
	f := nul(18, func() string {
		if r.Intn(2) == 0 {
			return "true"
		}
		return "false"
	})
	return fmt.Sprintf("(%d, %s, %s, %s, %s, %s)", id, a, b, c, s, f)
}

func TestFDB_MetamorphicIndexDifferential(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_mh")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mh")
	table := "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c DOUBLE, s STRING, f BOOLEAN, PRIMARY KEY (id)) "
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE mh_idx "+table+
		"CREATE INDEX t_a ON t (a) "+
		"CREATE INDEX t_ab ON t (a, b) "+
		"CREATE INDEX t_c ON t (c) "+
		"CREATE INDEX t_s ON t (s) "+
		"CREATE INDEX t_ba ON t (b, a) "+
		"CREATE INDEX t_f ON t (f)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE mh_noidx "+table)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mh/si WITH TEMPLATE mh_idx")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mh/sn WITH TEMPLATE mh_noidx")

	open := func(schema string) *sql.DB {
		dsn := fmt.Sprintf("fdbsql:///testdb_mh?cluster_file=%s&schema=%s", clusterFilePath, schema)
		db, err := sql.Open("fdbsql", dsn)
		if err != nil {
			t.Fatalf("open %s: %v", schema, err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	idx := mhDB{"idx", open("si")}
	noidx := mhDB{"noidx", open("sn")}

	dataRand := rand.New(rand.NewSource(20260819))
	const nRows = 140
	var vals []string
	for i := 1; i <= nRows; i++ {
		vals = append(vals, mhRowLiteral(dataRand, i, false))
	}
	for start := 0; start < len(vals); start += 20 {
		end := start + 20
		if end > len(vals) {
			end = len(vals)
		}
		stmt := "INSERT INTO t " + mhCols + " VALUES " + strings.Join(vals[start:end], ", ")
		mwjoMustExec(t, idx.db, ctx, stmt)
		mwjoMustExec(t, noidx.db, ctx, stmt)
	}

	seed := int64(1)
	if s := os.Getenv("MH_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	iters := 150
	if s := os.Getenv("MH_ITERS"); s != "" {
		fmt.Sscan(s, &iters)
	}
	g := &mhGen{r: rand.New(rand.NewSource(seed))}

	allIDs, err := mhScanIDs(ctx, idx.db, "SELECT id FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("baseline scan: %v", err)
	}
	if len(allIDs) != nRows {
		t.Fatalf("fixture population: got %d rows, want %d", len(allIDs), nRows)
	}

	bothErr, checked := 0, 0
	for i := 0; i < iters; i++ {
		p := g.pred(2)
		q := "SELECT id FROM t WHERE " + p + " ORDER BY id"

		gi, ei := mhScanIDs(ctx, idx.db, q)
		gn, en := mhScanIDs(ctx, noidx.db, q)
		switch {
		case ei != nil && en != nil:
			bothErr++
			continue
		case ei != nil || en != nil:
			t.Errorf("ERROR-ASYMMETRY seed=%d i=%d\n  q: %s\n  idx err:   %v\n  noidx err: %v", seed, i, q, ei, en)
			continue
		}
		checked++
		if !mhEqIDs(gi, gn) {
			t.Errorf("ROW-DIFF seed=%d i=%d\n  q: %s\n  idx  (%d): %v\n  noidx(%d): %v\n  only-in-idx: %v\n  only-in-noidx: %v",
				seed, i, q, len(gi), gi, len(gn), gn, mhDiff(gi, gn), mhDiff(gn, gi))
			continue
		}

		// TLP: P / NOT P / P IS NULL must partition the table.
		var union []int64
		ok := true
		for _, variant := range []string{p, "NOT (" + p + ")", "(" + p + ") IS NULL"} {
			ids, err := mhScanIDs(ctx, idx.db, "SELECT id FROM t WHERE "+variant+" ORDER BY id")
			if err != nil {
				ok = false
				break
			}
			union = append(union, ids...)
		}
		if !ok {
			continue
		}
		sort.Slice(union, func(x, y int) bool { return union[x] < union[y] })
		if !mhEqIDs(union, allIDs) {
			t.Errorf("TLP-PARTITION seed=%d i=%d\n  p: %s\n  union(%d) != all(%d)\n  missing: %v\n  extra: %v",
				seed, i, p, len(union), len(allIDs), mhDiff(allIDs, union), mhDiff(union, allIDs))
		}
	}
	t.Logf("seed=%d iters=%d checked=%d both-error=%d", seed, iters, checked, bothErr)
	if checked < iters/2 {
		t.Fatalf("instrument dead: only %d/%d predicates were actually compared", checked, iters)
	}
}

func mhEqIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mhDiff returns the multiset a-minus-b.
func mhDiff(a, b []int64) []int64 {
	cnt := map[int64]int{}
	for _, v := range b {
		cnt[v]++
	}
	var out []int64
	for _, v := range a {
		if cnt[v] > 0 {
			cnt[v]--
			continue
		}
		out = append(out, v)
	}
	return out
}

var _ = mhScanStrings
