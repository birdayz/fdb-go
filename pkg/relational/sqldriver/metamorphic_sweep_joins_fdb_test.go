package sqldriver_test

// Third metamorphic axis: JOINs, subqueries (EXISTS / IN / scalar), set
// operations, derived tables and CTEs. Same principle: two schemas, identical
// data, differing only in which indexes exist. An index may change the PLAN;
// it may never change the ANSWER.

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

func TestFDB_MetamorphicJoinsSubqueries(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_mh3")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mh3")
	tables := "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, s STRING, PRIMARY KEY (id)) " +
		"CREATE TABLE u (uid BIGINT, ua BIGINT, ub BIGINT, us STRING, PRIMARY KEY (uid)) "
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE mh3_idx "+tables+
		"CREATE INDEX t_a ON t (a) "+
		"CREATE INDEX t_ba ON t (b, a) "+
		"CREATE INDEX t_s ON t (s) "+
		"CREATE INDEX u_ua ON u (ua) "+
		"CREATE INDEX u_uba ON u (ub, ua) "+
		"CREATE INDEX u_us ON u (us)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE mh3_noidx "+tables)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mh3/si WITH TEMPLATE mh3_idx")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mh3/sn WITH TEMPLATE mh3_noidx")

	open := func(schema string) *sql.DB {
		dsn := fmt.Sprintf("fdbsql:///testdb_mh3?cluster_file=%s&schema=%s", clusterFilePath, schema)
		db, err := sql.Open("fdbsql", dsn)
		if err != nil {
			t.Fatalf("open %s: %v", schema, err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	idb, ndb := open("si"), open("sn")

	execBoth := func(stmt string) {
		t.Helper()
		_, ei := idb.ExecContext(ctx, stmt)
		_, en := ndb.ExecContext(ctx, stmt)
		if ei != nil || en != nil {
			t.Fatalf("fixture exec failed\n  stmt: %s\n  idx: %v\n  noidx: %v", stmt, ei, en)
		}
	}

	dr := rand.New(rand.NewSource(424242))
	nul := func(p int, gen func() string) string {
		if dr.Intn(100) < p {
			return "NULL"
		}
		return gen()
	}
	strDom := []string{"''", "'a'", "'ab'", "'b'", "'B'", "'abc'", "'z'"}
	var trows, urows []string
	const nT, nU = 70, 70
	for i := 1; i <= nT; i++ {
		trows = append(trows, fmt.Sprintf("(%d, %s, %s, %s)", i,
			nul(20, func() string { return fmt.Sprintf("%d", dr.Intn(6)-1) }),
			nul(20, func() string { return fmt.Sprintf("%d", dr.Intn(4)) }),
			nul(20, func() string { return strDom[dr.Intn(len(strDom))] })))
	}
	for i := 1; i <= nU; i++ {
		urows = append(urows, fmt.Sprintf("(%d, %s, %s, %s)", 100+i,
			nul(20, func() string { return fmt.Sprintf("%d", dr.Intn(6)-1) }),
			nul(20, func() string { return fmt.Sprintf("%d", dr.Intn(4)) }),
			nul(20, func() string { return strDom[dr.Intn(len(strDom))] })))
	}
	for start := 0; start < len(trows); start += 20 {
		end := start + 20
		if end > len(trows) {
			end = len(trows)
		}
		execBoth("INSERT INTO t (id, a, b, s) VALUES " + strings.Join(trows[start:end], ", "))
	}
	for start := 0; start < len(urows); start += 20 {
		end := start + 20
		if end > len(urows) {
			end = len(urows)
		}
		execBoth("INSERT INTO u (uid, ua, ub, us) VALUES " + strings.Join(urows[start:end], ", "))
	}

	seed := int64(1)
	if s := os.Getenv("MH3_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	iters := 30
	if s := os.Getenv("MH3_ITERS"); s != "" {
		fmt.Sscan(s, &iters)
	}
	r := rand.New(rand.NewSource(seed))
	g := &mhGen{r: r, intCols: []string{"a", "b", "id"}, dblCols: nil, strCols: []string{"s"}, boolCols: []string{}}
	gu := &mhGen{r: r, intCols: []string{"ua", "ub", "uid"}, dblCols: nil, strCols: []string{"us"}, boolCols: []string{}}

	okByTag := map[string]int{}
	errByTag := map[string]int{}
	diffByTag := map[string]int{}
	// firstFailure keeps ONE example per tag; a sweep that reports every
	// instance of a single systemic defect buries the second defect.
	firstFailure := map[string]string{}
	compare := func(tag, q string) {
		t.Helper()
		gi, ei := mhScanStrings(ctx, idb, q)
		gn, en := mhScanStrings(ctx, ndb, q)
		switch {
		case ei != nil && en != nil:
			errByTag[tag]++
			if errByTag[tag] <= 1 {
				t.Logf("%s both-error (sample): %s\n  err: %v", tag, q, ei)
			}
			return
		case ei != nil || en != nil:
			diffByTag[tag+"/ERR"]++
			if _, seen := firstFailure[tag+"/ERR"]; !seen {
				firstFailure[tag+"/ERR"] = fmt.Sprintf("ERROR-ASYMMETRY\n  q: %s\n  idx err: %v\n  noidx err: %v", q, ei, en)
			}
			return
		}
		okByTag[tag]++
		if !mhEqRows(gi, gn) {
			diffByTag[tag]++
			if _, seen := firstFailure[tag]; !seen {
				firstFailure[tag] = fmt.Sprintf("ROW-DIFF\n  q: %s\n  %s\n  idx  (%d): %v\n  noidx(%d): %v",
					q, mhFirstDiff(gi, gn), len(gi), mhHead(gi), len(gn), mhHead(gn))
			}
		}
	}

	predT := func() string { return g.pred(1) }
	predU := func() string { return gu.pred(1) }

	joinCond := []string{
		"t.a = u.ua",
		"t.a = u.ua AND t.b = u.ub",
		"t.b = u.ub",
		"t.a < u.ua",
		"t.s = u.us",
		"t.a = u.ua OR t.b = u.ub",
	}

	for i := 0; i < iters; i++ {
		pt, pu := predT(), predU()
		jc := joinCond[r.Intn(len(joinCond))]

		compare("INNER", fmt.Sprintf(
			"SELECT t.id, u.uid FROM t, u WHERE %s AND (%s) ORDER BY t.id, u.uid", jc, pt))
		compare("JOIN-ON", fmt.Sprintf(
			"SELECT t.id, u.uid FROM t JOIN u ON %s WHERE %s ORDER BY t.id, u.uid", jc, pt))
		compare("LEFT-JOIN", fmt.Sprintf(
			"SELECT t.id, u.uid FROM t LEFT JOIN u ON %s ORDER BY t.id, u.uid", jc))
		compare("LEFT-JOIN-W", fmt.Sprintf(
			"SELECT t.id, u.uid FROM t LEFT JOIN u ON %s AND (%s) WHERE %s ORDER BY t.id, u.uid", jc, pu, pt))
		compare("EXISTS", fmt.Sprintf(
			"SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.ua = t.a AND (%s)) ORDER BY id", pu))
		compare("NOT-EXISTS", fmt.Sprintf(
			"SELECT id FROM t WHERE NOT EXISTS (SELECT 1 FROM u WHERE u.ua = t.a AND (%s)) ORDER BY id", pu))
		compare("IN-SUBQ", fmt.Sprintf(
			"SELECT id FROM t WHERE a IN (SELECT ua FROM u WHERE %s) ORDER BY id", pu))
		compare("NOT-IN-SUBQ", fmt.Sprintf(
			"SELECT id FROM t WHERE a NOT IN (SELECT ua FROM u WHERE %s) ORDER BY id", pu))
		compare("SCALAR-SUBQ", fmt.Sprintf(
			"SELECT id FROM t WHERE a > (SELECT MAX(ua) FROM u WHERE %s) ORDER BY id", pu))
		compare("UNION", fmt.Sprintf(
			"SELECT a AS x FROM t WHERE %s UNION SELECT ua AS x FROM u WHERE %s ORDER BY x", pt, pu))
		compare("UNION-ALL", fmt.Sprintf(
			"SELECT a AS x FROM t WHERE %s UNION ALL SELECT ua AS x FROM u WHERE %s ORDER BY x", pt, pu))
		compare("DERIVED", fmt.Sprintf(
			"SELECT x.id, x.a FROM (SELECT id, a, b FROM t WHERE %s) AS x WHERE x.b IS NOT NULL ORDER BY x.id", pt))
		compare("CTE", fmt.Sprintf(
			"WITH w AS (SELECT id, a FROM t WHERE %s) SELECT w.id, u.uid FROM w, u WHERE w.a = u.ua ORDER BY w.id, u.uid", pt))
		compare("HAVING", fmt.Sprintf(
			"SELECT t.a, COUNT(*) FROM t WHERE %s GROUP BY t.a HAVING COUNT(*) > 1 ORDER BY t.a", pt))
	}

	// unsupported names the shapes this engine REJECTS by design, matching Java:
	// `col IN (SELECT ...)` in every spelling and bare `UNION` (only UNION ALL is
	// supported). They are swept anyway, and their tally is asserted the other
	// way round — zero comparisons and a non-zero error count — so the day one of
	// them starts planning, this test says so instead of silently comparing
	// nothing. The rejection itself is pinned by the yamsql corpus
	// (in_subquery_decomposition.yaml); what is pinned HERE is that the sweep
	// knows which of its tags can contribute evidence.
	unsupported := map[string]bool{"IN-SUBQ": true, "NOT-IN-SUBQ": true, "UNION": true}

	total := 0
	for _, tag := range []string{
		"INNER", "JOIN-ON", "LEFT-JOIN", "LEFT-JOIN-W", "EXISTS", "NOT-EXISTS",
		"IN-SUBQ", "NOT-IN-SUBQ", "SCALAR-SUBQ", "UNION", "UNION-ALL", "DERIVED", "CTE", "HAVING",
	} {
		t.Logf("  %-12s compared=%d both-error=%d row-diff=%d err-asym=%d",
			tag, okByTag[tag], errByTag[tag], diffByTag[tag], diffByTag[tag+"/ERR"])
		total += okByTag[tag]
		switch {
		case unsupported[tag]:
			if okByTag[tag] != 0 {
				t.Errorf("%s is listed as unsupported but %d of its queries PLANNED. If the feature "+
					"landed, drop it from `unsupported` so the sweep starts comparing its rows.",
					tag, okByTag[tag])
			}
			if errByTag[tag] == 0 {
				t.Errorf("%s produced neither comparisons nor errors — it was not exercised at all", tag)
			}
		case okByTag[tag] == 0:
			t.Errorf("instrument dead for %s: zero comparisons ran, so its green says nothing", tag)
		}
	}
	t.Logf("seed=%d iters=%d total-compared=%d", seed, iters, total)
	keys := make([]string, 0, len(firstFailure))
	for k := range firstFailure {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Errorf("[%s] x%d %s", k, diffByTag[k], firstFailure[k])
	}
}
