package sqldriver_test

// Second metamorphic axis: ORDER BY / LIMIT / DISTINCT / GROUP BY, plus index
// MAINTENANCE under DML. Two schemas hold identical data and differ only in
// which indexes (value AND aggregate) exist; every query text is run against
// both and the answers must be identical, row-for-row, in order.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
)

func mhEqRows(a, b []string) bool {
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

// mhHead truncates a row list so one systemic divergence cannot bury the rest
// of the report under thousands of rows.
func mhHead(rows []string) []string {
	if len(rows) <= 25 {
		return rows
	}
	return append(append([]string{}, rows[:25]...), fmt.Sprintf("...(+%d more)", len(rows)-25))
}

func mhFirstDiff(a, b []string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("row %d: idx=%q noidx=%q", i, a[i], b[i])
		}
	}
	return fmt.Sprintf("common prefix equal; lengths %d vs %d", len(a), len(b))
}

func TestFDB_MetamorphicOrderingAggregatesDML(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_mh2")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mh2")
	table := "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c DOUBLE, s STRING, f BOOLEAN, PRIMARY KEY (id)) "
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE mh2_idx "+table+
		"CREATE INDEX t_a ON t (a) "+
		"CREATE INDEX t_ab ON t (a, b) "+
		"CREATE INDEX t_c ON t (c) "+
		"CREATE INDEX t_s ON t (s) "+
		"CREATE INDEX t_ba ON t (b, a) "+
		// Every SUM index here aggregates b, which the fixture keeps NULL-free.
		// SUM over a NULLABLE column has a known, separately pinned divergence
		// (sumResidualZero); a sweep that kept walking into it would report the
		// same pinned defect on every run and bury anything new underneath.
		"CREATE INDEX t_cnt_a AS SELECT COUNT(*) FROM t GROUP BY a "+
		"CREATE INDEX t_sum_b_a AS SELECT SUM(b) FROM t GROUP BY a "+
		"CREATE INDEX t_cnt_b AS SELECT COUNT(*) FROM t GROUP BY b "+
		"CREATE INDEX t_min_b_a AS SELECT MIN(b) FROM t GROUP BY a "+
		"CREATE INDEX t_max_b_a AS SELECT MAX(b) FROM t GROUP BY a")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE mh2_noidx "+table)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mh2/si WITH TEMPLATE mh2_idx")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mh2/sn WITH TEMPLATE mh2_noidx")

	open := func(schema string) *sql.DB {
		dsn := fmt.Sprintf("fdbsql:///testdb_mh2?cluster_file=%s&schema=%s", clusterFilePath, schema)
		db, err := sql.Open("fdbsql", dsn)
		if err != nil {
			t.Fatalf("open %s: %v", schema, err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	idb, ndb := open("si"), open("sn")

	// exec runs the same statement on both schemas; a divergence in whether it
	// SUCCEEDS is itself a finding (index maintenance rejecting a legal write).
	exec := func(stmt string) bool {
		t.Helper()
		ri, ei := idb.ExecContext(ctx, stmt)
		rn, en := ndb.ExecContext(ctx, stmt)
		if (ei == nil) != (en == nil) {
			t.Errorf("DML-ASYMMETRY\n  stmt: %s\n  idx err:   %v\n  noidx err: %v", stmt, ei, en)
			return false
		}
		if ei == nil && en == nil {
			ai, e1 := ri.RowsAffected()
			an, e2 := rn.RowsAffected()
			if e1 == nil && e2 == nil && ai != an {
				t.Errorf("DML-ROWCOUNT-DIFF\n  stmt: %s\n  idx affected=%d noidx affected=%d", stmt, ai, an)
			}
		}
		return ei == nil && en == nil
	}

	// stateInSync compares the FULL table after every mutation. Without it a
	// divergent write desynchronizes the two fixtures and every later
	// comparison reports a difference that is an echo of the first one — the
	// finding would be real but unattributable.
	stateInSync := func(after string) {
		t.Helper()
		const q = "SELECT id, a, b, c, s, f FROM t ORDER BY id"
		gi, ei := mhScanStrings(ctx, idb, q)
		gn, en := mhScanStrings(ctx, ndb, q)
		if ei != nil || en != nil {
			t.Fatalf("state read failed after %q: %v / %v", after, ei, en)
		}
		if !mhEqRows(gi, gn) {
			t.Fatalf("STATE-DIVERGENCE after %q\n  %s\n  idx  (%d rows): %v\n  noidx(%d rows): %v",
				after, mhFirstDiff(gi, gn), len(gi), mhHead(gi), len(gn), mhHead(gn))
		}
	}

	dataRand := rand.New(rand.NewSource(20260819))
	const nRows = 120
	var vals []string
	for i := 1; i <= nRows; i++ {
		vals = append(vals, mhRowLiteral(dataRand, i, true))
	}
	for start := 0; start < len(vals); start += 20 {
		end := start + 20
		if end > len(vals) {
			end = len(vals)
		}
		exec("INSERT INTO t " + mhCols + " VALUES " + strings.Join(vals[start:end], ", "))
	}

	seed := int64(1)
	if s := os.Getenv("MH2_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	iters := 120
	if s := os.Getenv("MH2_ITERS"); s != "" {
		fmt.Sscan(s, &iters)
	}
	r := rand.New(rand.NewSource(seed))
	g := &mhGen{r: r}

	// okByTag / errByTag exist so a green cannot come from an empty population:
	// a tag whose every query errors on BOTH sides compares nothing.
	okByTag := map[string]int{}
	errByTag := map[string]int{}
	compare := func(tag, q string) bool {
		t.Helper()
		gi, ei := mhScanStrings(ctx, idb, q)
		gn, en := mhScanStrings(ctx, ndb, q)
		switch {
		case ei != nil && en != nil:
			errByTag[tag]++
			if errByTag[tag] <= 2 {
				t.Logf("%s both-error (sample): %s\n  err: %v", tag, q, ei)
			}
			return false
		case ei != nil || en != nil:
			t.Errorf("%s ERROR-ASYMMETRY seed=%d\n  q: %s\n  idx err:   %v\n  noidx err: %v", tag, seed, q, ei, en)
			return false
		}
		if !mhEqRows(gi, gn) {
			t.Errorf("%s ROW-DIFF seed=%d\n  q: %s\n  %s\n  idx  (%d): %v\n  noidx(%d): %v",
				tag, seed, q, mhFirstDiff(gi, gn), len(gi), gi, len(gn), gn)
			return false
		}
		okByTag[tag]++
		return true
	}

	orderKeys := []string{
		"a", "a DESC", "a, b", "a DESC, b", "a, b DESC", "b, a",
		"c", "c DESC", "s", "s DESC", "a, c, s", "b DESC, a DESC",
	}
	groupKeys := []string{"a", "b", "s", "f", "a, b", "b, a"}
	aggSets := []string{
		"COUNT(*)",
		"COUNT(a), COUNT(*)",
		"SUM(a)",
		"SUM(b), COUNT(*)",
		"MIN(a), MAX(a)",
		"MIN(c), MAX(c)",
		"MIN(s), MAX(s)",
		"AVG(a)",
		"SUM(a), SUM(b), COUNT(*), MIN(b), MAX(b)",
	}

	nextID := int64(nRows + 1)
	compared := 0
	mutations := 0
	for i := 0; i < iters; i++ {
		p := g.pred(2)

		// (1) total ORDER BY, with and without LIMIT/OFFSET.
		ok := orderKeys[r.Intn(len(orderKeys))]
		q := fmt.Sprintf("SELECT id, a, b, c, s FROM t WHERE %s ORDER BY %s, id", p, ok)
		if compare("ORDER", q) {
			compared++
		}
		if r.Intn(2) == 0 {
			lim := 1 + r.Intn(12)
			off := r.Intn(8)
			q2 := fmt.Sprintf("%s LIMIT %d OFFSET %d", q, lim, off)
			compare("ORDER+LIMIT", q2)
		}

		// (2) DISTINCT.
		if r.Intn(2) == 0 {
			cols := []string{"a", "b", "a, b", "s", "c", "a, s"}[r.Intn(6)]
			compare("DISTINCT", fmt.Sprintf("SELECT DISTINCT %s FROM t WHERE %s ORDER BY %s", cols, p, cols))
		}

		// (3) GROUP BY with aggregates — the aggregate-index arm on the idx side.
		gk := groupKeys[r.Intn(len(groupKeys))]
		as := aggSets[r.Intn(len(aggSets))]
		compare("GROUPBY", fmt.Sprintf("SELECT %s, %s FROM t GROUP BY %s ORDER BY %s", gk, as, gk, gk))
		if r.Intn(2) == 0 {
			compare("GROUPBY+WHERE", fmt.Sprintf("SELECT %s, %s FROM t WHERE %s GROUP BY %s ORDER BY %s", gk, as, p, gk, gk))
		}

		// (4) ungrouped aggregate over a predicate.
		compare("AGG", fmt.Sprintf("SELECT %s FROM t WHERE %s", as, p))

		// (5) index maintenance: mutate both schemas identically, then keep going.
		// Cases 3 and 4 drive the mutation from a PREDICATE rather than a
		// primary key, so on the indexed side the rows are located through the
		// very index the write then maintains — the shape where an update that
		// re-keys a row can be re-visited by its own scan.
		randVal := func(col string) string {
			// b is never nulled: it is what the SUM indexes aggregate, and the
			// aggregate NULL axis is pinned exhaustively elsewhere.
			if col != "b" && r.Intn(4) == 0 {
				return "NULL"
			}
			switch col {
			case "a", "b":
				return mhIntLits[r.Intn(len(mhIntLits))]
			case "c":
				return mhDblLits[r.Intn(len(mhDblLits))]
			case "s":
				return mhStrLits[r.Intn(len(mhStrLits))]
			default:
				return []string{"true", "false"}[r.Intn(2)]
			}
		}
		var stmt string
		switch r.Intn(8) {
		case 0:
			stmt = fmt.Sprintf("INSERT INTO t %s VALUES %s", mhCols, mhRowLiteral(r, int(nextID), true))
			nextID++
		case 1:
			col := []string{"a", "b", "c", "s", "f"}[r.Intn(5)]
			stmt = fmt.Sprintf("UPDATE t SET %s = %s WHERE id = %d", col, randVal(col), 1+r.Intn(nRows))
		case 2:
			stmt = fmt.Sprintf("DELETE FROM t WHERE id = %d", 1+r.Intn(nRows))
		case 3:
			// Re-key an indexed column through a scan of that same column.
			col := []string{"a", "b", "c", "s"}[r.Intn(4)]
			stmt = fmt.Sprintf("UPDATE t SET %s = %s WHERE %s", col, randVal(col), g.pred(1))
		case 4:
			stmt = fmt.Sprintf("DELETE FROM t WHERE %s", g.pred(1))
		}
		if stmt != "" {
			mutations++
			if exec(stmt) {
				stateInSync(stmt)
			}
		}
	}
	t.Logf("seed=%d iters=%d order-compared=%d mutations=%d", seed, iters, compared, mutations)
	if mutations == 0 {
		t.Errorf("instrument dead: zero mutations ran, so index maintenance was never exercised")
	}
	for _, tag := range []string{"ORDER", "ORDER+LIMIT", "DISTINCT", "GROUPBY", "GROUPBY+WHERE", "AGG"} {
		t.Logf("  %-14s compared=%d both-error=%d", tag, okByTag[tag], errByTag[tag])
		if okByTag[tag] == 0 {
			t.Errorf("instrument dead for %s: zero comparisons actually ran", tag)
		}
	}
	if compared < iters/2 {
		t.Fatalf("instrument dead: only %d/%d ORDER comparisons ran", compared, iters)
	}
}
