package sqldriver_test

// Fourth axis: pagination. At a row count large enough to cross the executor's
// internal paging boundary, LIMIT/OFFSET slices must agree with the full result
// they slice, and the indexed and non-indexed schemas must still agree.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_MetamorphicPagingAtScale(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_mhp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mhp")
	table := "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, s STRING, PRIMARY KEY (id)) "
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE mhp_idx "+table+
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_ab ON t (a, b) CREATE INDEX t_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE mhp_noidx "+table)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mhp/si WITH TEMPLATE mhp_idx")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mhp/sn WITH TEMPLATE mhp_noidx")

	open := func(schema string) *sql.DB {
		dsn := fmt.Sprintf("fdbsql:///testdb_mhp?cluster_file=%s&schema=%s", clusterFilePath, schema)
		db, err := sql.Open("fdbsql", dsn)
		if err != nil {
			t.Fatalf("open %s: %v", schema, err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	idb, ndb := open("si"), open("sn")

	const n = 4000
	var vals []string
	for i := 1; i <= n; i++ {
		a := "NULL"
		if i%17 != 0 {
			a = fmt.Sprintf("%d", i%53)
		}
		s := "NULL"
		if i%13 != 0 {
			s = fmt.Sprintf("'k%03d'", i%97)
		}
		vals = append(vals, fmt.Sprintf("(%d, %s, %d, %s)", i, a, i%7, s))
	}
	for start := 0; start < len(vals); start += 200 {
		end := start + 200
		if end > len(vals) {
			end = len(vals)
		}
		stmt := "INSERT INTO t (id, a, b, s) VALUES " + strings.Join(vals[start:end], ", ")
		for _, db := range []*sql.DB{idb, ndb} {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}

	bases := []string{
		"SELECT id FROM t ORDER BY id",
		"SELECT id FROM t ORDER BY a, id",
		"SELECT id FROM t ORDER BY a DESC, id",
		"SELECT id, a FROM t WHERE a > 10 ORDER BY a, id",
		"SELECT id, s FROM t WHERE s IS NOT NULL ORDER BY s, id",
		"SELECT DISTINCT a FROM t ORDER BY a",
		"SELECT a, COUNT(*) FROM t GROUP BY a ORDER BY a",
	}
	slices := []struct{ limit, offset int }{
		{1, 0},
		{1, 1},
		{7, 0},
		{7, 3},
		{100, 0},
		{100, 250},
		{999, 1000},
		{2500, 0},
		{2500, 1500},
		{5, 3995},
	}

	checks := 0
	for _, base := range bases {
		full, ei := mhScanStrings(ctx, idb, base)
		fullN, en := mhScanStrings(ctx, ndb, base)
		if ei != nil || en != nil {
			t.Errorf("base query failed: %s -> %v / %v", base, ei, en)
			continue
		}
		if !mhEqRows(full, fullN) {
			t.Errorf("BASE ROW-DIFF\n  q: %s\n  %s\n  idx  (%d): %v\n  noidx(%d): %v",
				base, mhFirstDiff(full, fullN), len(full), mhHead(full), len(fullN), mhHead(fullN))
			continue
		}
		if len(full) == 0 {
			t.Errorf("instrument dead: base query returned no rows: %s", base)
			continue
		}
		for _, sl := range slices {
			q := fmt.Sprintf("%s LIMIT %d OFFSET %d", base, sl.limit, sl.offset)
			want := []string{}
			if sl.offset < len(full) {
				end := sl.offset + sl.limit
				if end > len(full) {
					end = len(full)
				}
				want = full[sl.offset:end]
			}
			for name, db := range map[string]*sql.DB{"idx": idb, "noidx": ndb} {
				got, err := mhScanStrings(ctx, db, q)
				if err != nil {
					t.Errorf("PAGE ERROR [%s] %s: %v", name, q, err)
					continue
				}
				checks++
				if !mhEqRows(got, want) {
					t.Errorf("PAGE MISMATCH [%s]\n  q: %s\n  %s\n  got (%d): %v\n  want(%d): %v",
						name, q, mhFirstDiff(got, want), len(got), mhHead(got), len(want), mhHead(want))
				}
			}
		}
	}
	t.Logf("paging checks run: %d", checks)
	if checks < len(bases)*len(slices) {
		t.Errorf("instrument dead: only %d paging checks ran, expected at least %d",
			checks, len(bases)*len(slices))
	}
}
