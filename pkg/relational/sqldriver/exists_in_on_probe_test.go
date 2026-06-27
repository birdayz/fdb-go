package sqldriver_test

// Adversarial probes around EXISTS-in-ON (RFC-154 Phase 2a) — feature-edge
// stress: multiple EXISTS conjuncts, EXISTS-in-ON alongside WHERE-EXISTS,
// uncorrelated EXISTS, and a 3-way join with EXISTS-in-ON. Each asserts a
// hand-computed row set; a crash / planner error / wrong rows here is a bug to
// fix (or a clean rejection to pin).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_ExistsInOn_Probe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_exists_on_probe")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_exists_on_probe")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE exists_on_probe "+
			"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE e (id BIGINT NOT NULL, c_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_a_id ON c (a_id) "+
			"CREATE INDEX e_c_id ON e (c_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_exists_on_probe/s WITH TEMPLATE exists_on_probe")
	dsn := fmt.Sprintf("fdbsql:///testdb_exists_on_probe?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a={1,2,3}; c: 50→a1, 51→a2, 52→a1; d={1}; e: 900→c50.
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2), (3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (50, 1), (51, 2), (52, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id) VALUES (1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO e (id, c_id) VALUES (900, 50)")

	// Two EXISTS conjuncts in one ON → MORE than one existential quantifier on
	// the binary join, which the NLJ rule does not implement (single-existential
	// only — a pre-existing limit shared with WHERE EXISTS over a join). Must
	// reject CLEANLY (not the opaque "could not plan query").
	t.Run("two_exists_in_on_rejected", func(t *testing.T) {
		assertUnsupported(t, db, ctx,
			"SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id "+
				"AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id) "+
				"AND EXISTS (SELECT 1 FROM e WHERE e.c_id = c.id)")
	})

	// EXISTS in ON + EXISTS in WHERE together → two existential quantifiers on
	// the same join level → also beyond the single-existential NLJ shape.
	// Rejected cleanly by the buried-existential backstop.
	t.Run("exists_in_on_plus_where_exists_rejected", func(t *testing.T) {
		assertUnsupported(t, db, ctx,
			"SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id) "+
				"WHERE EXISTS (SELECT 1 FROM e WHERE e.c_id = c.id)")
	})

	// Uncorrelated EXISTS in ON: EXISTS(SELECT 1 FROM d) is always true (d
	// non-empty), so the ON reduces to the equi-join → all matches.
	t.Run("uncorrelated_exists_in_on", func(t *testing.T) {
		got := scanPairs(t, db, ctx,
			"SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d)")
		want := []string{"1|50", "1|52", "2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("uncorrelated-EXISTS-in-ON rows = %v, want %v", got, want)
		}
	})

	// 3-way join, EXISTS in the SECOND join's ON correlated to the first join's
	// left leg. a JOIN c ON c.a_id=a.id, then JOIN e ON e.c_id=c.id AND
	// EXISTS(d.id=a.id). e=900→c50; c50→a1; EXISTS(d.id=1) true → (1,50,900).
	t.Run("threeway_exists_in_second_on", func(t *testing.T) {
		got := scanTriples(t, db, ctx,
			"SELECT a.id, c.id, e.id FROM a JOIN c ON c.a_id = a.id "+
				"JOIN e ON e.c_id = c.id AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id)")
		want := []string{"1|50|900"}
		if !eqStrSlices(got, want) {
			t.Errorf("3-way EXISTS-in-2nd-ON rows = %v, want %v", got, want)
		}
	})
}

func scanPairs(t *testing.T, db *sql.DB, ctx context.Context, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	got := siScanRows(t, rows)
	return got
}

func scanTriples(t *testing.T, db *sql.DB, ctx context.Context, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var x, y, z sql.NullInt64
		if err := rows.Scan(&x, &y, &z); err != nil {
			t.Fatalf("scan: %v", err)
		}
		r := func(v sql.NullInt64) string {
			if !v.Valid {
				return "NULL"
			}
			return fmt.Sprintf("%d", v.Int64)
		}
		out = append(out, r(x)+"|"+r(y)+"|"+r(z))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
